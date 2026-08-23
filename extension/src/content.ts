/**
 * Quick Download — content script.
 *
 * Two jobs, one file, because a page only ever needs one of these injected:
 *
 *   1. The page scanner. Walks the DOM for <video>, <audio> and <img> elements
 *      and produces a thumbnail for each one, on request from the popup.
 *   2. The floating download button. An IDM-style overlay that appears on
 *      hover over any video and hands it straight to the engine.
 *
 * The manifest registers this on every page for (2); the service worker also
 * injects it on demand for (1), which the guard below makes harmless.
 *
 * Why a content script at all: the network sniffer sees URLs, not pictures.
 * A HLS stream's <video> element has a blob: src that is meaningless outside
 * the page, but its rendered frame — or its poster — is exactly the preview a
 * user needs to tell two videos apart.
 *
 * IMPORTANT: this file must stay dependency-free and must contain NO import or
 * export statements. TypeScript treats any file with one as an ES module, and
 * chrome.scripting.executeScript({files}) runs classic scripts only — a module
 * would silently fail to execute. Types are declared locally and erased at
 * compile time, and the injection guard uses a cast rather than `declare
 * global` (which would itself require module scope).
 */

interface PageMetaLocal {
  ogImage?: string;
  youtubeId?: string;
}

interface ScannedMediaLocal {
  tag: 'video' | 'audio' | 'image';
  src: string;
  poster?: string;
  thumbnail?: string;
  title?: string;
  duration?: number;
  width?: number;
  height?: number;
}

(() => {
  // The service worker re-injects this file every time the popup opens.
  // Registering the listener twice would answer each message twice.
  const guard = window as unknown as { __quickDownloadScannerReady?: boolean };
  if (guard.__quickDownloadScannerReady) return;
  guard.__quickDownloadScannerReady = true;

  /** Thumbnail geometry: small enough to keep chrome.storage.session happy. */
  const THUMB_WIDTH = 320;
  const THUMB_HEIGHT = 180;
  const THUMB_QUALITY = 0.7;
  /** Ignore sprites, spacers, tracking pixels and avatars. */
  const MIN_IMAGE_EDGE = 200;
  const MAX_ITEMS = 40;

  function absolute(url: string | null | undefined): string {
    if (!url) return '';
    try {
      return new URL(url, document.baseURI).href;
    } catch {
      return url;
    }
  }

  /**
   * Captures the currently displayed frame of a video.
   *
   * This fails by design for cross-origin media without CORS headers: drawing
   * it taints the canvas and toDataURL throws SecurityError. That is not a bug
   * to work around — it is the browser protecting pixel data — so we simply
   * fall back to the poster image.
   */
  function captureFrame(video: HTMLVideoElement): string | undefined {
    // HAVE_CURRENT_DATA: there is at least one frame to draw.
    if (video.readyState < 2) return undefined;
    if (!video.videoWidth || !video.videoHeight) return undefined;

    try {
      const ratio = video.videoHeight / video.videoWidth;
      const canvas = document.createElement('canvas');
      canvas.width = THUMB_WIDTH;
      canvas.height = Math.max(1, Math.round(THUMB_WIDTH * ratio)) || THUMB_HEIGHT;

      const ctx = canvas.getContext('2d');
      if (!ctx) return undefined;
      ctx.drawImage(video, 0, 0, canvas.width, canvas.height);

      return canvas.toDataURL('image/jpeg', THUMB_QUALITY);
    } catch {
      // SecurityError (tainted canvas) or an out-of-memory canvas.
      return undefined;
    }
  }

  /** Best-effort human title for a media element. */
  function titleFor(el: Element): string {
    const explicit =
      el.getAttribute('title') ||
      el.getAttribute('aria-label') ||
      el.getAttribute('alt');
    if (explicit && explicit.trim()) return explicit.trim();

    // Look for a heading in the surrounding card/section.
    let node: Element | null = el.parentElement;
    for (let depth = 0; node && depth < 4; depth++, node = node.parentElement) {
      const heading = node.querySelector('h1, h2, h3, [role="heading"]');
      const text = heading?.textContent?.trim();
      if (text) return text.slice(0, 200);
    }
    return document.title.trim().slice(0, 200);
  }

  /** Pulls the real source out of a <video>, including a nested <source>. */
  function sourceOf(el: HTMLVideoElement | HTMLAudioElement): string {
    if (el.currentSrc) return el.currentSrc;
    if (el.src) return absolute(el.src);
    const source = el.querySelector('source');
    return absolute(source?.getAttribute('src'));
  }

  /**
   * Page-level artwork. This is what rescues streams: an HLS or YouTube video
   * plays from a blob: URL whose frames are usually cross-origin (so the canvas
   * taints), but essentially every video page advertises a still through
   * OpenGraph, and YouTube publishes one at a predictable address.
   */
  function pageMeta(): PageMetaLocal {
    const meta = (selector: string): string => {
      const el = document.querySelector(selector);
      const value = el?.getAttribute('content') || el?.getAttribute('href') || '';
      return value ? absolute(value) : '';
    };

    const ogImage =
      meta('meta[property="og:image"]') ||
      meta('meta[name="og:image"]') ||
      meta('meta[name="twitter:image"]') ||
      meta('meta[property="twitter:image"]') ||
      meta('link[rel="image_src"]') ||
      undefined;

    return { ogImage, youtubeId: youTubeVideoId(location.href) };
  }

  /** Extracts the video id from any YouTube URL shape. */
  function youTubeVideoId(raw: string): string | undefined {
    let u: URL;
    try {
      u = new URL(raw);
    } catch {
      return undefined;
    }
    const host = u.hostname.toLowerCase().replace(/^www\./, '');
    const isYouTube =
      host === 'youtube.com' ||
      host.endsWith('.youtube.com') ||
      host === 'youtu.be' ||
      host === 'youtube-nocookie.com';
    if (!isYouTube) return undefined;

    if (host === 'youtu.be') {
      const id = u.pathname.replace(/^\//, '').split('/')[0];
      return /^[\w-]{6,}$/.test(id) ? id : undefined;
    }
    const v = u.searchParams.get('v');
    if (v && /^[\w-]{6,}$/.test(v)) return v;

    const m = u.pathname.match(/^\/(?:shorts|live|embed|v)\/([\w-]{6,})/);
    return m ? m[1] : undefined;
  }

  /** YouTube's predictable still for a video id. */
  function youTubeThumbnail(id: string): string {
    return 'https://img.youtube.com/vi/' + id + '/hqdefault.jpg';
  }

  function scanVideos(out: ScannedMediaLocal[], meta: PageMetaLocal): void {
    document.querySelectorAll('video').forEach((video) => {
      if (out.length >= MAX_ITEMS) return;
      const src = sourceOf(video);
      const poster = absolute(video.getAttribute('poster'));

      // A video with neither a source nor a poster is a placeholder element -
      // unless the page advertises artwork, which is the normal case for a
      // stream player whose <video> is fed from a blob: URL.
      if (!src && !poster && !meta.ogImage && !meta.youtubeId) return;

      // Preference order, best first:
      //   1. a live canvas frame   - shows what is actually on screen
      //   2. the poster attribute  - the site's own still for this video
      //   3. the YouTube thumbnail - predictable and always correct
      //   4. og:image              - the page's advertised artwork
      const thumbnail =
        captureFrame(video) ??
        (poster || undefined) ??
        (meta.youtubeId ? youTubeThumbnail(meta.youtubeId) : undefined) ??
        meta.ogImage;

      out.push({
        tag: 'video',
        src,
        poster: poster || undefined,
        thumbnail,
        title: titleFor(video),
        duration: Number.isFinite(video.duration) ? video.duration : undefined,
        width: video.videoWidth || undefined,
        height: video.videoHeight || undefined,
      });
    });
  }

  function scanAudio(out: ScannedMediaLocal[]): void {
    document.querySelectorAll('audio').forEach((audio) => {
      if (out.length >= MAX_ITEMS) return;
      const src = sourceOf(audio);
      if (!src) return;
      out.push({
        tag: 'audio',
        src,
        title: titleFor(audio),
        duration: Number.isFinite(audio.duration) ? audio.duration : undefined,
      });
    });
  }

  function scanImages(out: ScannedMediaLocal[]): void {
    document.querySelectorAll('img').forEach((img) => {
      if (out.length >= MAX_ITEMS) return;
      const src = img.currentSrc || img.src;
      if (!src || src.startsWith('data:')) return;
      // Use the intrinsic size, not the CSS size: a 4000px photo scaled to a
      // 100px thumbnail on the page is still worth downloading.
      const w = img.naturalWidth || img.width;
      const h = img.naturalHeight || img.height;
      if (w < MIN_IMAGE_EDGE && h < MIN_IMAGE_EDGE) return;

      out.push({
        tag: 'image',
        src: absolute(src),
        // An image is its own thumbnail — no canvas, no data URL, no quota use.
        thumbnail: absolute(src),
        title: titleFor(img),
        width: w,
        height: h,
      });
    });
  }

  function scan(): {
    ok: boolean;
    pageTitle: string;
    pageUrl: string;
    media: ScannedMediaLocal[];
    pageThumbnail?: string;
  } {
    const media: ScannedMediaLocal[] = [];
    const meta = pageMeta();
    try {
      scanVideos(media, meta);
      scanAudio(media);
      scanImages(media);
    } catch (err) {
      // A broken page must not break the popup: return whatever we collected.
      console.debug('[quick-download] scan error', err);
    }
    return {
      ok: true,
      pageTitle: document.title,
      pageUrl: location.href,
      media,
      // Handed back so the service worker can give a sniffed stream a preview
      // even when the page has no <video> whose frame it could read.
      pageThumbnail: meta.youtubeId ? youTubeThumbnail(meta.youtubeId) : meta.ogImage,
    };
  }

  chrome.runtime.onMessage.addListener((message, _sender, sendResponse) => {
    // Only the top frame answers a scan. The script now runs in every frame
    // (the floating button needs to), and a message sent to the tab reaches
    // all of them - so without this, whichever frame replied first would win
    // and the popup would show a random subframe's media.
    if (message?.kind === 'qd-scan' && window.top === window) {
      // Master switch. The service worker already refuses to inject while
      // sniffing is off; this is the second line of defence for an instance
      // that was injected before the user flipped the switch.
      chrome.storage.local.get('settings', (bag) => {
        const settings = (bag as { settings?: { enabled?: boolean } })?.settings;
        if (settings?.enabled === false) {
          sendResponse({ ok: false, pageTitle: document.title, pageUrl: location.href, media: [] });
          return;
        }
        sendResponse(scan());
      });
      // Asynchronous reply: keep the message channel open.
      return true;
    }
    return false;
  });

  // -------------------------------------------------------------------------
  // Floating download button
  // -------------------------------------------------------------------------
  //
  // IDM-style: hover a video, a small button fades in over its top-right
  // corner. Three decisions shape the implementation:
  //
  //   - There is ONE button for the whole document, moved to whichever video is
  //     hovered, rather than one injected per video. Nothing is added inside the
  //     page's own layout, so no `overflow: hidden` can clip it, no flex
  //     container is disturbed, and a video being torn out of the DOM leaves
  //     nothing of ours behind to clean up.
  //   - It is position: fixed and anchored from getBoundingClientRect(), so it
  //     follows the video without the page needing a positioned ancestor.
  //   - The DOM is built with createElement/createElementNS, never innerHTML.
  //     Sites that enforce Trusted Types (YouTube among them) throw on an
  //     innerHTML assignment, which would break the button on exactly the sites
  //     it matters most on.

  const FAB_CLASS = 'qd-fab';
  /** Below a poster-sized box it is a decorative loop or an ad, not content. */
  const MIN_VIDEO_W = 160;
  const MIN_VIDEO_H = 90;
  /** Grace period so the pointer can travel from the video onto the button. */
  const HIDE_DELAY_MS = 240;
  const INSET = 10;
  const FAB_SIZE = 34;

  let fab: HTMLButtonElement | null = null;
  let fabMark: HTMLSpanElement | null = null;
  let anchor: HTMLVideoElement | null = null;
  let hideTimer: ReturnType<typeof setTimeout> | undefined;
  let frameRequest = 0;
  let tracking = false;
  let buttonEnabled = true;
  let observer: MutationObserver | null = null;
  let pointerWatching = false;
  let recountQueued = false;
  /** How many <video> elements the page currently has. */
  let videoCount = 0;

  function svgIcon(): SVGSVGElement {
    const ns = 'http://www.w3.org/2000/svg';
    const svg = document.createElementNS(ns, 'svg');
    svg.setAttribute('viewBox', '0 0 24 24');
    svg.setAttribute('width', '17');
    svg.setAttribute('height', '17');
    svg.setAttribute('fill', 'none');
    svg.setAttribute('stroke', 'currentColor');
    svg.setAttribute('stroke-width', '2');
    svg.setAttribute('stroke-linecap', 'round');
    svg.setAttribute('stroke-linejoin', 'round');
    svg.setAttribute('aria-hidden', 'true');
    svg.setAttribute('class', 'qd-fab__icon');

    const arrow = document.createElementNS(ns, 'path');
    arrow.setAttribute('d', 'M12 3v11m0 0 4-4m-4 4-4-4');
    const tray = document.createElementNS(ns, 'path');
    tray.setAttribute('d', 'M4 17v2a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-2');
    svg.append(arrow, tray);
    return svg;
  }

  function ensureFab(): HTMLButtonElement {
    if (fab && fab.isConnected) return fab;

    const button = document.createElement('button');
    button.type = 'button';
    button.className = FAB_CLASS;
    button.title = 'Download with Quick Download';
    button.setAttribute('aria-label', 'Download this video');

    const mark = document.createElement('span');
    mark.className = 'qd-fab__mark';
    button.append(svgIcon(), mark);

    // The page must not see these: a click on a player's overlay usually means
    // play/pause, and we are sitting on top of one.
    for (const type of ['pointerdown', 'mousedown', 'dblclick'] as const) {
      button.addEventListener(type, (e) => e.stopPropagation(), true);
    }
    button.addEventListener('click', (event) => {
      event.preventDefault();
      event.stopPropagation();
      void requestDownload();
    });
    button.addEventListener('pointerenter', cancelHide);
    button.addEventListener('pointerleave', scheduleHide);

    document.documentElement.append(button);
    fab = button;
    fabMark = mark;
    return button;
  }

  function removeFab(): void {
    cancelHide();
    stopTracking();
    fab?.remove();
    fab = null;
    fabMark = null;
    anchor = null;
  }

  /** Where the video is right now, or null if it is not worth a button. */
  function anchorRect(video: HTMLVideoElement): DOMRect | null {
    if (!video.isConnected) return null;
    const rect = video.getBoundingClientRect();
    if (rect.width < MIN_VIDEO_W || rect.height < MIN_VIDEO_H) return null;
    // Entirely off-screen: scrolled away, or a hidden pre-roll slot.
    if (rect.bottom <= 0 || rect.top >= innerHeight) return null;
    if (rect.right <= 0 || rect.left >= innerWidth) return null;
    return rect;
  }

  function position(): void {
    if (!fab || !anchor) return;
    const rect = anchorRect(anchor);
    if (!rect) {
      hide();
      return;
    }
    // Clamped to the viewport so a video hanging off the edge still shows it.
    const left = Math.min(Math.max(INSET, rect.right - FAB_SIZE - INSET), innerWidth - FAB_SIZE - 2);
    const top = Math.min(Math.max(INSET, rect.top + INSET), innerHeight - FAB_SIZE - 2);
    fab.style.left = `${Math.round(left)}px`;
    fab.style.top = `${Math.round(top)}px`;
  }

  function show(video: HTMLVideoElement): void {
    if (!buttonEnabled) return;
    // Fullscreen is the player's stage. Anything we add either does not render
    // (it is outside the fullscreen element) or lands on top of the player's
    // own controls, so we stay out of the way entirely.
    if (document.fullscreenElement) return;
    if (!anchorRect(video)) return;

    cancelHide();
    anchor = video;
    const button = ensureFab();
    setState('');
    position();
    button.classList.add('qd-fab--on');
    startTracking();
  }

  function hide(): void {
    cancelHide();
    stopTracking();
    fab?.classList.remove('qd-fab--on');
    anchor = null;
  }

  function scheduleHide(): void {
    cancelHide();
    hideTimer = setTimeout(hide, HIDE_DELAY_MS);
  }

  function cancelHide(): void {
    if (hideTimer !== undefined) {
      clearTimeout(hideTimer);
      hideTimer = undefined;
    }
  }

  /**
   * Scroll and resize listeners exist only while the button is visible, which
   * is the difference between a passive extension and one that runs code on
   * every scroll event of every page the user visits.
   */
  function startTracking(): void {
    if (tracking) return;
    tracking = true;
    addEventListener('scroll', onViewportChange, { passive: true, capture: true });
    addEventListener('resize', onViewportChange, { passive: true });
  }

  function stopTracking(): void {
    if (!tracking) return;
    tracking = false;
    removeEventListener('scroll', onViewportChange, true);
    removeEventListener('resize', onViewportChange);
    if (frameRequest) {
      cancelAnimationFrame(frameRequest);
      frameRequest = 0;
    }
  }

  function onViewportChange(): void {
    if (frameRequest) return;
    frameRequest = requestAnimationFrame(() => {
      frameRequest = 0;
      position();
    });
  }

  function setState(state: '' | 'busy' | 'done' | 'fail'): void {
    if (!fab || !fabMark) return;
    if (state) fab.dataset.state = state;
    else delete fab.dataset.state;
    fabMark.textContent = state === 'busy' ? '…' : state === 'done' ? '✓' : state === 'fail' ? '!' : '';
  }

  /**
   * What to hand the engine.
   *
   * A streamed video (HLS, DASH, YouTube) plays from a blob: URL that exists
   * only inside this renderer - useless to a process outside the browser. The
   * page URL is the right answer there: yt-dlp's extractors take it from
   * nothing to a finished file, which is exactly what the popup's "page"
   * entry does.
   */
  function targetFor(video: HTMLVideoElement): { url: string; streamType?: string } {
    const src = sourceOf(video);
    if (src && /^https?:/i.test(src)) return { url: src };
    return { url: location.href, streamType: 'site' };
  }

  async function requestDownload(): Promise<void> {
    const video = anchor;
    if (!video) return;
    const target = targetFor(video);
    setState('busy');

    try {
      const response = (await chrome.runtime.sendMessage({
        kind: 'downloadFromPage',
        url: target.url,
        pageUrl: location.href,
        title: titleFor(video) || document.title,
        streamType: target.streamType,
      })) as { ok?: boolean; error?: string } | undefined;

      setState(response?.ok ? 'done' : 'fail');
      if (fab && !response?.ok && response?.error) fab.title = response.error;
    } catch (err) {
      // Thrown when the extension has just been reloaded and this script is
      // orphaned. Nothing to recover: the next page load gets a live one.
      console.debug('[quick-download] download request failed', err);
      setState('fail');
    }

    setTimeout(() => {
      if (fab?.dataset.state !== 'busy') setState('');
    }, 1600);
  }

  // --- discovery -------------------------------------------------------------

  /**
   * Finds the video under the pointer.
   *
   * Listening for pointerenter on each <video> does not work on a real player.
   * YouTube, Vimeo and every custom skin cover the video with their own
   * controls layer, which is a sibling rather than a child - so the pointer
   * never enters the <video> box as far as hit testing is concerned, and the
   * button would appear on plain <video> tags only.
   *
   * elementsFromPoint sees straight through that: it returns the whole stack
   * under the cursor, overlay first, video somewhere below.
   */
  function videoUnder(event: PointerEvent): HTMLVideoElement | null {
    const target = event.target;
    if (target instanceof HTMLVideoElement) return target;

    // Only pay for the hit test on a page that actually has a video.
    if (videoCount === 0) return null;
    for (const el of document.elementsFromPoint(event.clientX, event.clientY)) {
      if (el instanceof HTMLVideoElement) return el;
      // Stop at our own button so hovering it cannot re-anchor anything.
      if (el === fab) return anchor;
    }
    return null;
  }

  function onPointerOver(event: PointerEvent): void {
    if (!buttonEnabled) return;
    const target = event.target;
    if (target instanceof Node && fab?.contains(target)) return;

    const video = videoUnder(event);
    if (video) show(video);
    else if (anchor) scheduleHide();
  }

  function startPointerWatch(): void {
    if (pointerWatching) return;
    pointerWatching = true;
    // Capture phase, because a player that stops propagation on its overlay
    // would otherwise hide every pointer event from us.
    document.addEventListener('pointerover', onPointerOver, { passive: true, capture: true });
  }

  function stopPointerWatch(): void {
    if (!pointerWatching) return;
    pointerWatching = false;
    document.removeEventListener('pointerover', onPointerOver, true);
  }

  /**
   * Keeps count of the videos on the page.
   *
   * Players are built long after load and swapped on every SPA navigation, so
   * a one-off querySelectorAll would find nothing on most sites. The count is
   * what gates the pointer listener: a page with no video (which is most of
   * them) costs nothing beyond this observer.
   */
  function recount(): void {
    videoCount = document.querySelectorAll('video').length;
    if (videoCount > 0) startPointerWatch();
    else {
      stopPointerWatch();
      hide();
    }
  }

  function startObserving(): void {
    if (observer) return;
    observer = new MutationObserver(() => {
      if (!buttonEnabled) return;
      // The anchored video may have just been torn out of the document.
      if (anchor && !anchor.isConnected) hide();
      if (recountQueued) return;
      recountQueued = true;
      // Coalesced: a busy page mutates hundreds of times per frame, and the
      // answer only has to be right by the time the user hovers something.
      requestAnimationFrame(() => {
        recountQueued = false;
        recount();
      });
    });
    observer.observe(document.documentElement, { childList: true, subtree: true });
  }

  function stopObserving(): void {
    observer?.disconnect();
    observer = null;
  }

  function setButtonEnabled(enabled: boolean): void {
    buttonEnabled = enabled;
    if (enabled) {
      startObserving();
      recount();
    } else {
      stopObserving();
      stopPointerWatch();
      removeFab();
    }
  }

  // Fullscreen: the button goes away and comes back with the player.
  document.addEventListener('fullscreenchange', () => {
    if (document.fullscreenElement) hide();
  });

  // Nothing of ours should survive the page.
  addEventListener('pagehide', () => {
    stopObserving();
    stopPointerWatch();
    removeFab();
  });

  chrome.storage.onChanged.addListener((changes, area) => {
    if (area !== 'local' || !changes.settings) return;
    const next = changes.settings.newValue as { enabled?: boolean } | undefined;
    setButtonEnabled(next?.enabled !== false);
  });

  // The master switch governs the overlay too: with sniffing off, the extension
  // should leave no trace on the page at all.
  chrome.storage.local.get('settings', (bag) => {
    const settings = (bag as { settings?: { enabled?: boolean } })?.settings;
    setButtonEnabled(settings?.enabled !== false);
  });
})();
