/**
 * Quick Download — page scanner (content script).
 *
 * Injected on demand by the service worker (chrome.scripting.executeScript)
 * when the popup opens. It walks the DOM for <video>, <audio> and <img>
 * elements and produces a thumbnail for each one.
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

  function scanVideos(out: ScannedMediaLocal[]): void {
    document.querySelectorAll('video').forEach((video) => {
      if (out.length >= MAX_ITEMS) return;
      const src = sourceOf(video);
      const poster = absolute(video.getAttribute('poster'));

      // A video with neither a source nor a poster is a placeholder element.
      if (!src && !poster) return;

      out.push({
        tag: 'video',
        src,
        poster: poster || undefined,
        // A live frame beats the poster: it shows what is actually playing.
        thumbnail: captureFrame(video) ?? (poster || undefined),
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
  } {
    const media: ScannedMediaLocal[] = [];
    try {
      scanVideos(media);
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
    };
  }

  chrome.runtime.onMessage.addListener((message, _sender, sendResponse) => {
    if (message?.kind === 'qd-scan') {
      sendResponse(scan());
    }
    // Synchronous reply: returning false/undefined closes the channel cleanly.
    return false;
  });
})();
