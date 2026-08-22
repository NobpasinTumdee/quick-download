/**
 * Quick Download — MV3 service worker.
 *
 * Responsibilities:
 *   1. Sniff network responses for downloadable media, including the streaming
 *      manifests (HLS/DASH) that modern sites hide their video behind.
 *   2. Ask the page for thumbnails (content.ts) when the popup opens.
 *   3. Optionally take over downloads Chrome starts itself.
 *   4. Forward chosen URLs to the Go engine over Native Messaging.
 *
 * Detection notes (MV3 reality check):
 *   - Blocking webRequest is gone in MV3, but OBSERVING responses is not:
 *     chrome.webRequest.onHeadersReceived still fires, it just cannot modify
 *     anything. That is all we need, and it gives us Content-Type and
 *     Content-Length, which a URL pattern alone cannot.
 *   - Streaming video never appears as a single file. The page fetches a
 *     manifest (.m3u8 / .mpd) and then thousands of segments, and hands the
 *     result to <video> as a blob: URL. A blob: URL is meaningless outside the
 *     page, so the only downloadable handle is the MANIFEST — that is what we
 *     capture, and what yt-dlp turns back into one file.
 *   - declarativeNetRequest is included as a second signal via
 *     onRuleMatchedDebug. That event only fires for UNPACKED extensions, so it
 *     is a development aid, never the primary path.
 *   - A service worker is killed when idle, so ALL state lives in
 *     chrome.storage.session, never in module scope.
 */

import {
  classify,
  cleanPageUrl,
  isSiteHost,
  isWatchPageUrl,
  isYouTubeHost,
  sniffDecision,
  IMAGE_EXT_RE,
  MEDIA_EXT_RE,
} from './filters.js';
import {
  NATIVE_HOST,
  DEFAULT_SETTINGS,
  type HostRequest,
  type HostResponse,
  type MediaItem,
  type MediaKind,
  type ScanResult,
  type Settings,
  type StreamType,
  type UiMessage,
} from './types.js';

const STORAGE_KEY = 'mediaItems';
const SETTINGS_KEY = 'settings';
const MAX_ITEMS = 300;
/**
 * Thumbnails are data: URLs. chrome.storage.session has a ~10 MB quota, so we
 * keep only the newest ones and drop the rest to stay far below it.
 */
const MAX_THUMBNAILS = 40;

// ---------------------------------------------------------------------------
// Storage helpers (session storage survives SW restarts, not browser restarts)
// ---------------------------------------------------------------------------

async function getItems(): Promise<MediaItem[]> {
  const bag = await chrome.storage.session.get(STORAGE_KEY);
  return (bag[STORAGE_KEY] as MediaItem[]) ?? [];
}

async function setItems(items: MediaItem[]): Promise<void> {
  const trimmed = items.slice(0, MAX_ITEMS);

  // Enforce the thumbnail budget: keep data: URLs on the newest entries only.
  // Remote-URL thumbnails cost nothing, so they are exempt.
  let dataUrls = 0;
  for (const item of trimmed) {
    if (item.thumbnail?.startsWith('data:')) {
      dataUrls++;
      if (dataUrls > MAX_THUMBNAILS) delete item.thumbnail;
    }
  }

  try {
    await chrome.storage.session.set({ [STORAGE_KEY]: trimmed });
  } catch (err) {
    // Quota exceeded: retry once with every heavy thumbnail stripped.
    console.warn('[quick-download] session storage full, dropping thumbnails', err);
    for (const item of trimmed) {
      if (item.thumbnail?.startsWith('data:')) delete item.thumbnail;
    }
    await chrome.storage.session.set({ [STORAGE_KEY]: trimmed });
  }
}

async function getSettings(): Promise<Settings> {
  const bag = await chrome.storage.local.get(SETTINGS_KEY);
  return { ...DEFAULT_SETTINGS, ...((bag[SETTINGS_KEY] as Partial<Settings>) ?? {}) };
}

async function saveSettings(patch: Partial<Settings>): Promise<Settings> {
  const next = { ...(await getSettings()), ...patch };
  await chrome.storage.local.set({ [SETTINGS_KEY]: next });
  return next;
}

// ---------------------------------------------------------------------------
// Native messaging
// ---------------------------------------------------------------------------

/**
 * sendNativeMessage spawns a fresh host process per call and tears it down
 * once the reply arrives — which is exactly why the Go host is only a relay to
 * a long-lived daemon. Wrapped in a promise with lastError handling so a
 * missing host manifest surfaces as a readable error instead of a silent drop.
 */
function sendToHost(message: HostRequest): Promise<HostResponse> {
  return new Promise((resolve) => {
    try {
      chrome.runtime.sendNativeMessage(NATIVE_HOST, message, (response) => {
        const err = chrome.runtime.lastError;
        if (err) {
          resolve({
            ok: false,
            type: message.type,
            error:
              `${err.message ?? 'native host unreachable'} — is the host manifest ` +
              `registered and does its allowed_origins list this extension id?`,
          });
          return;
        }
        resolve((response as HostResponse) ?? { ok: false, type: message.type, error: 'empty reply' });
      });
    } catch (e) {
      resolve({ ok: false, type: message.type, error: String(e) });
    }
  });
}

/** Collects the cookies a normal page request would have carried. */
async function cookieHeaderFor(url: string): Promise<string> {
  try {
    const cookies = await chrome.cookies.getAll({ url });
    return cookies.map((c) => `${c.name}=${c.value}`).join('; ');
  } catch {
    return '';
  }
}

/**
 * Sends one URL to the engine, carrying the browser context (referrer, cookies,
 * user agent) so that protected media resolves the same way it did in the page.
 *
 * For streams this context is not a nicety: a bare manifest URL fetched without
 * the page's Referer and cookies is a 403 on most CDNs.
 */
async function startDownload(
  url: string,
  opts: {
    filename?: string;
    pageUrl?: string;
    kind?: StreamType;
    mime?: string;
    title?: string;
  } = {},
): Promise<HostResponse> {
  const response = await sendToHost({
    type: 'download',
    url,
    filename: opts.filename,
    referrer: opts.pageUrl,
    cookie: await cookieHeaderFor(url),
    userAgent: navigator.userAgent,
    kind: opts.kind ?? classify(url, opts.mime ?? ''),
    mime: opts.mime,
    title: opts.title,
    requestId: crypto.randomUUID(),
  });

  await flash(response.ok ? '✓' : '!', response.ok ? '#22c55e' : '#ef4444');
  if (!response.ok) {
    console.warn('[quick-download] engine refused the job:', response.error);
    reportProblem('Download failed to start', response.error ?? 'unknown error');
  }
  return response;
}

/** Failures surface in the popup and the log; no notifications permission needed. */
function reportProblem(title: string, message: string): void {
  console.info(`[quick-download] ${title}: ${message}`);
}

async function flash(text: string, color: string): Promise<void> {
  await chrome.action.setBadgeText({ text });
  await chrome.action.setBadgeBackgroundColor({ color });
  setTimeout(() => void refreshBadge(), 1500);
}

async function refreshBadge(): Promise<void> {
  const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
  const items = await getItems();
  const mine = tab?.id ? items.filter((i) => i.tabId === tab.id) : items;
  await chrome.action.setBadgeText({ text: mine.length ? String(mine.length) : '' });
  await chrome.action.setBadgeBackgroundColor({ color: '#6ea8fe' });
}

function labelFor(streamType: StreamType, kind: MediaKind): string | undefined {
  switch (streamType) {
    case 'hls':
      return 'HLS stream';
    case 'dash':
      return 'DASH stream';
    case 'site':
      return 'page extract';
    default:
      return kind === 'image' ? undefined : undefined;
  }
}

function guessName(url: string, kind: MediaKind, title?: string): string {
  // Manifests are named master.m3u8 / index.mpd on every site in the world,
  // so the page title is the only useful name for a stream.
  if (kind === 'stream') {
    const base = (title ?? '').trim().replace(/[\\/:*?"<>|]/g, '_').slice(0, 120);
    return base || 'stream';
  }
  try {
    const path = new URL(url).pathname;
    const base = decodeURIComponent(path.split('/').filter(Boolean).pop() ?? '');
    if (base && base.includes('.')) return base;
  } catch {
    /* fall through */
  }
  return `${kind}-${Date.now()}`;
}

// ---------------------------------------------------------------------------
// Detection
// ---------------------------------------------------------------------------

async function remember(item: MediaItem): Promise<void> {
  const items = await getItems();
  // Same URL in the same tab is the same file: refresh it instead of piling up.
  // This is what stops a live HLS playlist — re-fetched every few seconds —
  // from producing hundreds of identical entries.
  const existing = items.findIndex((i) => i.url === item.url && i.tabId === item.tabId);
  if (existing >= 0) {
    items[existing] = { ...items[existing], ...item, thumbnail: items[existing].thumbnail ?? item.thumbnail };
  } else {
    items.unshift(item);
  }
  await setItems(items);
  await refreshBadge();
}

chrome.webRequest.onHeadersReceived.addListener(
  (details) => {
    // Fire and forget: an observer must never delay the response.
    void inspect(details);
  },
  { urls: ['http://*/*', 'https://*/*'] },
  ['responseHeaders'],
);

async function inspect(details: chrome.webRequest.WebResponseHeadersDetails): Promise<void> {
  try {
    if (details.statusCode >= 400) return;
    if (details.url.includes('127.0.0.1:') || details.url.includes('localhost:')) return;

    const headers = details.responseHeaders ?? [];
    const header = (name: string): string =>
      headers.find((h) => h.name.toLowerCase() === name)?.value ?? '';

    const mime = header('content-type').split(';')[0].trim().toLowerCase();
    const size = Number.parseInt(header('content-length') || '0', 10) || 0;

    // One gate for everything: telemetry, auth endpoints, ad traffic, stream
    // segments, extractor-only CDNs and page URLs are all rejected here.
    const decision = sniffDecision(details.url, mime);
    if (!decision.keep) {
      console.debug('[quick-download] ignored (%s): %s', decision.reason, details.url);
      return;
    }
    const { streamType, kind } = decision;

    const settings = await getSettings();
    if (streamType !== 'direct') {
      if (!settings.captureStreams) return;
    } else if (kind === 'image') {
      if (!settings.captureImages) return;
      // Tiny images are sprites, avatars and tracking pixels: pure noise.
      if (size === 0 || size < settings.minImageBytes) return;
    }

    let pageUrl = '';
    let pageTitle = '';
    if (details.tabId >= 0) {
      try {
        const tab = await chrome.tabs.get(details.tabId);
        pageUrl = tab.url ?? '';
        pageTitle = tab.title ?? '';
      } catch {
        /* tab already gone */
      }
    }

    // On an extractor site, a stray media/manifest request is not the download
    // the user wants — the page entry is. Suppress it so YouTube shows exactly
    // one row instead of a dozen CDN and API URLs.
    if (pageUrl && isSiteHost(pageUrl) && isWatchPageUrl(pageUrl)) {
      console.debug('[quick-download] ignored (extractor page owns this tab): %s', details.url);
      return;
    }

    await remember({
      id: `${details.tabId}:${details.url}`,
      url: details.url,
      filename: guessName(details.url, kind, pageTitle),
      kind,
      streamType,
      mime,
      size,
      tabId: details.tabId,
      pageUrl,
      pageTitle,
      seenAt: Date.now(),
      label: labelFor(streamType, kind),
    });
  } catch (e) {
    console.debug('[quick-download] inspect failed', e);
  }
}

/**
 * Secondary signal from declarativeNetRequest. onRuleMatchedDebug only fires
 * for unpacked extensions; it is a development aid, not the primary detector.
 */
if (chrome.declarativeNetRequest?.onRuleMatchedDebug) {
  chrome.declarativeNetRequest.onRuleMatchedDebug.addListener((info) => {
    console.debug(
      '[quick-download] DNR rule %d matched %s',
      info.rule.ruleId,
      info.request.url,
    );
  });
}

// Forget a tab's finds when it goes away.
chrome.tabs.onRemoved.addListener(async (tabId) => {
  const items = await getItems();
  await setItems(items.filter((i) => i.tabId !== tabId));
  await refreshBadge();
});

chrome.tabs.onActivated.addListener(() => void refreshBadge());

// ---------------------------------------------------------------------------
// Thumbnails
// ---------------------------------------------------------------------------

/**
 * Injects content.ts into a tab and asks it to scan the DOM.
 *
 * Injection happens on demand rather than through a declared content script:
 * a video's frame is only capturable once it is actually playing, and running
 * a scanner on every page all the time would be pure overhead.
 */
async function scanTab(tabId: number): Promise<ScanResult | null> {
  try {
    await chrome.scripting.executeScript({
      target: { tabId },
      files: ['dist/content.js'],
    });
  } catch (err) {
    // chrome:// pages, the web store, PDFs and cross-origin iframes refuse
    // injection. That is expected — we simply have no thumbnails there.
    console.debug('[quick-download] cannot inject scanner', err);
    return null;
  }

  try {
    return (await chrome.tabs.sendMessage(tabId, { kind: 'qd-scan' })) as ScanResult;
  } catch (err) {
    console.debug('[quick-download] scanner did not answer', err);
    return null;
  }
}

/**
 * Merges DOM findings into the sniffed list.
 *
 * The join is deliberately loose. A sniffed HLS manifest and the <video>
 * element playing it share no URL — the element's src is a blob: — so we match
 * on the tab instead: if a page has exactly one video area, its frame is the
 * right preview for the stream we sniffed there.
 */
async function attachThumbnails(tabId: number): Promise<void> {
  const settings = await getSettings();
  if (!settings.captureThumbnails) return;

  const scan = await scanTab(tabId);
  if (!scan?.media?.length) return;

  const items = await getItems();
  const videoThumb = scan.media.find((m) => m.tag === 'video' && m.thumbnail)?.thumbnail;
  const videoMeta = scan.media.find((m) => m.tag === 'video');

  let changed = false;
  for (const item of items) {
    if (item.tabId !== tabId || item.thumbnail) continue;

    // Exact URL match first: direct <video src> and <img src> hits.
    const exact = scan.media.find((m) => m.src && m.src === item.url);
    if (exact?.thumbnail) {
      item.thumbnail = exact.thumbnail;
      item.duration ??= exact.duration;
      item.width ??= exact.width;
      item.height ??= exact.height;
      changed = true;
      continue;
    }

    // Streams have no matchable URL — fall back to the page's video frame.
    if (item.kind === 'stream' && videoThumb) {
      item.thumbnail = videoThumb;
      item.duration ??= videoMeta?.duration;
      item.width ??= videoMeta?.width;
      item.height ??= videoMeta?.height;
      changed = true;
      continue;
    }

    // An image is its own thumbnail; no DOM round trip needed.
    if (item.kind === 'image') {
      item.thumbnail = item.url;
      changed = true;
    }
  }

  if (changed) await setItems(items);
}

/** A page on a known site is itself downloadable via yt-dlp. */
async function pageItemFor(tabId: number): Promise<MediaItem | null> {
  try {
    const tab = await chrome.tabs.get(tabId);
    const raw = tab.url ?? '';
    // Only a real video page, and only after stripping the playlist and
    // tracking parameters that would send yt-dlp down its playlist extractor.
    if (!isWatchPageUrl(raw)) return null;
    const url = cleanPageUrl(raw);

    return {
      id: `page:${tabId}:${url}`,
      url,
      filename: guessName(url, 'stream', tab.title),
      kind: 'stream',
      streamType: 'site',
      mime: '',
      size: 0,
      tabId,
      pageUrl: url,
      pageTitle: tab.title ?? '',
      seenAt: Date.now(),
      thumbnail: tab.favIconUrl,
      label: isYouTubeHost(url) ? 'YouTube video' : 'this page',
    };
  } catch {
    return null;
  }
}

// ---------------------------------------------------------------------------
// Taking over Chrome's own downloads (opt-in, IDM style)
// ---------------------------------------------------------------------------

chrome.downloads.onDeterminingFilename.addListener((item, suggest) => {
  void (async () => {
    const settings = await getSettings();
    const isOurs = item.url.includes('127.0.0.1:') || item.url.includes('localhost:');
    const eligible =
      settings.interceptChromeDownloads &&
      !isOurs &&
      /^https?:/.test(item.url) &&
      (MEDIA_EXT_RE.test(item.url) ||
        IMAGE_EXT_RE.test(item.url) ||
        item.mime?.startsWith('video/') ||
        item.mime?.startsWith('audio/'));

    if (!eligible) {
      suggest();
      return;
    }

    // Hand it to our engine instead and stop Chrome's own transfer.
    // suggest() is still called: having returned true from the listener we owe
    // Chrome exactly one call, and an un-answered callback wedges the download
    // system for every other listener.
    suggest();
    await chrome.downloads.cancel(item.id).catch(() => undefined);
    await chrome.downloads.erase({ id: item.id }).catch(() => undefined);
    await startDownload(item.finalUrl || item.url, {
      filename: item.filename || undefined,
      pageUrl: item.referrer,
      mime: item.mime,
    });
  })();

  // Returning true keeps the suggestion callback alive for the async work.
  return true;
});

// ---------------------------------------------------------------------------
// Context menu
// ---------------------------------------------------------------------------

chrome.runtime.onInstalled.addListener(() => {
  chrome.contextMenus.removeAll(() => {
    chrome.contextMenus.create({
      id: 'qd-download',
      title: 'Download with Quick Download',
      contexts: ['link', 'video', 'audio', 'image'],
    });
    chrome.contextMenus.create({
      id: 'qd-download-page',
      title: 'Download video on this page (yt-dlp)',
      contexts: ['page', 'frame'],
    });
    chrome.contextMenus.create({
      id: 'qd-dashboard',
      title: 'Open Quick Download dashboard',
      contexts: ['action'],
    });
  });
});

chrome.contextMenus.onClicked.addListener((info, tab) => {
  switch (info.menuItemId) {
    case 'qd-dashboard':
      void sendToHost({ type: 'open_dashboard' });
      return;

    case 'qd-download-page': {
      // Hand the PAGE url to yt-dlp and let its extractor find the streams.
      const page = cleanPageUrl(tab?.url ?? info.pageUrl ?? '');
      if (page) {
        void startDownload(page, { pageUrl: page, kind: 'site', title: tab?.title });
      }
      return;
    }

    case 'qd-download': {
      const target = info.srcUrl || info.linkUrl || info.pageUrl;
      if (target) {
        void startDownload(target, { pageUrl: tab?.url ?? info.pageUrl, title: tab?.title });
      }
      return;
    }
  }
});

// ---------------------------------------------------------------------------
// Popup RPC
// ---------------------------------------------------------------------------

chrome.runtime.onMessage.addListener((message: UiMessage, _sender, sendResponse) => {
  void (async () => {
    switch (message.kind) {
      case 'list': {
        // The popup asks for a thumbnail pass before rendering.
        if (message.withThumbnails && message.tabId !== undefined) {
          await attachThumbnails(message.tabId);
        }

        const items = await getItems();
        const scoped = message.tabId ? items.filter((i) => i.tabId === message.tabId) : items;

        // Offer the page itself when it is on a site yt-dlp can extract.
        const extras: MediaItem[] = [];
        if (message.tabId !== undefined) {
          const page = await pageItemFor(message.tabId);
          if (page && !scoped.some((i) => i.url === page.url)) extras.push(page);
        }
        sendResponse([...extras, ...scoped]);
        break;
      }

      case 'download':
        sendResponse(
          await startDownload(message.item.url, {
            filename: message.item.kind === 'stream' ? message.item.filename : undefined,
            pageUrl: message.item.pageUrl,
            kind: message.item.streamType,
            mime: message.item.mime,
            title: message.item.pageTitle || message.item.filename,
          }),
        );
        break;

      case 'downloadUrl': {
        // A pasted YouTube link usually carries &list= from the address bar.
        const cleaned = cleanPageUrl(message.url);
        sendResponse(
          await startDownload(cleaned, {
            pageUrl: message.pageUrl,
            kind: isWatchPageUrl(cleaned) ? 'site' : undefined,
          }),
        );
        break;
      }

      case 'downloadPage': {
        const page = await pageItemFor(message.tabId);
        if (!page) {
          sendResponse({ ok: false, type: 'download', error: 'this page is not a known video site' });
          break;
        }
        sendResponse(
          await startDownload(page.url, {
            pageUrl: page.url,
            kind: 'site',
            title: page.pageTitle,
          }),
        );
        break;
      }

      case 'clear': {
        const items = await getItems();
        await setItems(message.tabId ? items.filter((i) => i.tabId !== message.tabId) : []);
        await refreshBadge();
        sendResponse({ ok: true });
        break;
      }

      case 'ping':
        sendResponse(await sendToHost({ type: 'ping' }));
        break;

      case 'openDashboard':
        sendResponse(await sendToHost({ type: 'open_dashboard' }));
        break;

      case 'getSettings':
        sendResponse(await getSettings());
        break;

      case 'setSettings':
        sendResponse(await saveSettings(message.settings));
        break;

      default:
        sendResponse({ ok: false, error: 'unknown message' });
    }
  })();

  // true = "I will call sendResponse asynchronously", which keeps the message
  // channel open. Forgetting this is the classic MV3 popup bug.
  return true;
});
