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

import { installContextMenus, registerContextMenus } from './contextMenu.js';
import {
  EngineFeed,
  hasActiveJobs,
  snapshotStates,
  terminalTransitions,
} from './engineSocket.js';
import {
  classify,
  cleanPageUrl,
  hostInList,
  youTubeThumbnail,
  youTubeVideoId,
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
  type JobProgress,
  type MediaItem,
  type MediaKind,
  type Quality,
  type EngineInfo,
  type ScanResult,
  PROGRESS_PORT,
  type ProgressPush,
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

/** Master switch. Every sniffing entry point checks this first. */
async function isEnabled(): Promise<boolean> {
  return (await getSettings()).enabled !== false;
}

/**
 * Greys out the toolbar icon while sniffing is off.
 *
 * The icon is desaturated at runtime from the real PNG rather than shipping a
 * second set of assets: fetch it, draw it into an OffscreenCanvas with a
 * grayscale filter, and hand chrome.action the resulting ImageData. Service
 * workers have no DOM, but OffscreenCanvas works there.
 */
async function paintActionIcon(enabled: boolean): Promise<void> {
  try {
    if (enabled) {
      // Restoring means clearing the override so the manifest icons apply.
      await chrome.action.setIcon({ path: {
        16: 'icons/icon16.png',
        32: 'icons/icon32.png',
        48: 'icons/icon48.png',
        128: 'icons/icon128.png',
      } });
      await chrome.action.setTitle({ title: 'Quick Download' });
      return;
    }

    const imageData: Record<number, ImageData> = {};
    for (const size of [16, 32, 48]) {
      const response = await fetch(chrome.runtime.getURL(`icons/icon${size}.png`));
      const bitmap = await createImageBitmap(await response.blob());
      const canvas = new OffscreenCanvas(size, size);
      const ctx = canvas.getContext('2d');
      if (!ctx) continue;
      ctx.filter = 'grayscale(1) opacity(0.45)';
      ctx.drawImage(bitmap, 0, 0, size, size);
      imageData[size] = ctx.getImageData(0, 0, size, size);
      bitmap.close();
    }
    await chrome.action.setIcon({ imageData });
    await chrome.action.setTitle({ title: 'Quick Download - sniffing is off' });
  } catch (err) {
    // A failed repaint must never break the toggle itself.
    console.debug('[quick-download] could not repaint the action icon', err);
  }
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
 * Cookies are only collected for domains the user has explicitly allowed.
 *
 * Forwarding a session cookie to a local process is the most sensitive thing
 * this extension does, and for most sites it buys nothing: public media needs
 * no credentials. Worse, on YouTube it actively breaks downloads by tripping
 * the anti-bot check. So the default is to send none, and the allowlist names
 * the few sites (Instagram, Facebook) where a login really is required.
 *
 * Both the media URL and the page it came from are checked: on Instagram the
 * page is instagram.com while the file lives on cdninstagram.com, and the
 * useful cookie belongs to the page.
 */
async function cookieFor(
  settings: Settings,
  downloadUrl: string,
  pageUrl?: string,
): Promise<string> {
  const allowlist = settings.cookieAllowlist ?? [];
  const targetAllowed = hostInList(downloadUrl, allowlist);
  const pageAllowed = !!pageUrl && hostInList(pageUrl, allowlist);
  if (!targetAllowed && !pageAllowed) return '';

  // Prefer cookies scoped to the actual request; fall back to the page's.
  const direct = targetAllowed ? await cookieHeaderFor(downloadUrl) : '';
  if (direct) return direct;
  return pageAllowed && pageUrl ? cookieHeaderFor(pageUrl) : '';
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
    quality?: Quality;
  } = {},
): Promise<HostResponse> {
  // The save path is a user setting rather than a per-call argument: it applies
  // to every download, including ones started from the context menu.
  const settings = await getSettings();

  const response = await sendToHost({
    type: 'download',
    url,
    filename: opts.filename,
    referrer: opts.pageUrl,
    cookie: await cookieFor(settings, url, opts.pageUrl),
    userAgent: navigator.userAgent,
    kind: opts.kind ?? classify(url, opts.mime ?? ''),
    mime: opts.mime,
    title: opts.title,
    quality: opts.quality ?? settings.defaultQuality,
    savePath: settings.savePath?.trim() || undefined,
    requestId: crypto.randomUUID(),
  });

  // Watch for what we just queued, so its completion is announced even if no
  // popup is ever opened.
  if (response.ok) holdForEnqueue();

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
    // Master switch: do no work at all while sniffing is off.
    if (!(await isEnabled())) return;

    const headers = details.responseHeaders ?? [];
    const header = (name: string): string =>
      headers.find((h) => h.name.toLowerCase() === name)?.value ?? '';

    const mime = header('content-type').split(';')[0].trim().toLowerCase();
    const size = Number.parseInt(header('content-length') || '0', 10) || 0;

    const settings = await getSettings();

    // One gate for everything: size floor, ad/telemetry hosts, stream segments,
    // extractor-only CDNs and page URLs are all decided here.
    const decision = sniffDecision(details.url, mime, size, {
      smartFilter: settings.smartFilter !== false,
      minBytes: Math.max(0, (settings.minFileSizeKB ?? 0) * 1024),
      imageMinBytes: settings.minImageBytes ?? 0,
      captureImages: settings.captureImages,
      captureStreams: settings.captureStreams,
    });
    if (!decision.keep) {
      console.debug('[quick-download] ignored (%s): %s', decision.reason, details.url);
      return;
    }
    const { streamType, kind } = decision;

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
  if (!(await isEnabled())) return null;
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
  // og:image / the YouTube still, used when no <video> frame was readable.
  const pageThumb = scan.pageThumbnail;

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

    // Streams have no matchable URL: use the page's video frame, and if the
    // canvas was tainted (the usual case for HLS and YouTube), the page's own
    // artwork. This is what stops streams from showing a bare icon.
    if (item.kind === 'stream') {
      const thumb = videoThumb ?? pageThumb;
      if (thumb) {
        item.thumbnail = thumb;
        item.duration ??= videoMeta?.duration;
        item.width ??= videoMeta?.width;
        item.height ??= videoMeta?.height;
        changed = true;
        continue;
      }
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
      // A YouTube still is derivable from the URL alone, so the page entry has
      // a real preview even before the scanner runs (or if injection fails).
      thumbnail: youTubeThumbnail(youTubeVideoId(url)) ?? tab.favIconUrl,
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
      settings.enabled !== false &&
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

/**
 * The popup document doubles as the side panel document; ?panel=1 lets it widen
 * its layout and hide the "open in side panel" button.
 */
async function configureSidePanel(): Promise<void> {
  try {
    await chrome.sidePanel.setOptions({ path: 'popup.html?panel=1', enabled: true });
  } catch (err) {
    console.debug('[quick-download] side panel unavailable', err);
  }
}

chrome.runtime.onStartup.addListener(() => {
  void (async () => {
    await paintActionIcon(await isEnabled());
    await configureSidePanel();
  })();

  // Chrome keeps context menus across restarts, so this is usually a no-op.
  // It is here for the profile where they were lost anyway: installContextMenus
  // clears before it creates, so replaying it costs nothing and repairs that.
  installContextMenus();
});

chrome.runtime.onInstalled.addListener(() => {
  void (async () => {
    await paintActionIcon(await isEnabled());
    await configureSidePanel();
  })();

  installContextMenus();
});

/**
 * The click handler is registered at the top level, not inside onInstalled: a
 * context-menu click is one of the events that wakes a sleeping service worker,
 * and a listener added asynchronously would miss the very click that started it.
 *
 * Everything it needs is injected, which keeps the routing logic testable and
 * keeps one path to the engine - so a right-click download picks up the save
 * path, the cookie allowlist and the default quality exactly like the popup.
 */
registerContextMenus({
  startDownload: (url, opts) => startDownload(url, opts),
  openDashboard: () => sendToHost({ type: 'open_dashboard' }),
  notificationsEnabled: async () => (await getSettings()).notifyOnComplete !== false,
});

// ---------------------------------------------------------------------------
// Engine feed
// ---------------------------------------------------------------------------
//
// The WebSocket to the engine lives here, not in the popup. Two things follow
// from that, and both are the point of the exercise:
//
//   - completion notifications fire whether or not any UI is open, and
//   - the popup becomes a subscriber, so opening it costs one message instead
//     of a fresh connection and a fresh reconnect loop.
//
// Chrome 116+ keeps a service worker alive while its WebSocket carries traffic,
// which is what makes this viable at all. The flip side is that a socket held
// open forever pins the worker forever, so EngineFeed is demand-driven: see the
// hold/release calls below.

const DEFAULT_ENGINE_ORIGIN = 'http://127.0.0.1:9090';

let feed: EngineFeed | null = null;
/** Cached from the last ping, so bringing the feed up costs no extra host call. */
let engineOrigin: string | undefined;
let feedStarting: Promise<EngineFeed | null> | null = null;

/** The most recent snapshot, replayed to a popup the moment it opens. */
let latestJobs: JobProgress[] = [];

/**
 * The state each job was in when we last looked.
 *
 * Held in memory on purpose. If the worker is torn down, this map is lost and
 * the next snapshot notifies about nothing - which is right: a job that
 * finished while nobody was watching is history, not news.
 */
let lastStates = new Map<string, string>();

/** Popups and side panels currently listening. */
const uiPorts = new Set<chrome.runtime.Port>();

/** Job ids already announced, in session storage so a respawn cannot repeat them. */
const NOTIFIED_KEY = 'notifiedJobs';

async function alreadyNotified(id: string): Promise<boolean> {
  try {
    const bag = await chrome.storage.session.get(NOTIFIED_KEY);
    const ids = (bag[NOTIFIED_KEY] as string[] | undefined) ?? [];
    if (ids.includes(id)) return true;
    // Keep the list bounded; the engine forgets old jobs long before this.
    await chrome.storage.session.set({ [NOTIFIED_KEY]: [...ids, id].slice(-200) });
    return false;
  } catch {
    return false;
  }
}

/**
 * Brings the feed up, resolving the engine's origin first.
 *
 * The ping doubles as the daemon's wake-up call, so this also covers the case
 * where the engine is not running yet when the first popup opens.
 */
function ensureFeed(): Promise<EngineFeed | null> {
  if (feed) return Promise.resolve(feed);
  if (feedStarting) return feedStarting;

  feedStarting = (async () => {
    // A ping costs a host process, and the popup usually made one moments ago
    // for its status chip - so reuse that answer when we have it.
    const origin =
      engineOrigin ?? (await sendToHost({ type: 'ping' })).dashboard ?? DEFAULT_ENGINE_ORIGIN;
    if (!feed) {
      feed = new EngineFeed({
        origin,
        onSnapshot: (jobs) => void onSnapshot(jobs),
        onStatus: (connected) => pushToUi(connected),
      });
    }
    feedStarting = null;
    return feed;
  })();

  return feedStarting;
}

async function onSnapshot(jobs: JobProgress[]): Promise<void> {
  latestJobs = jobs;

  // Anything that just finished is announced before the states are rolled
  // forward, because the transition is the only thing that is news.
  const finished = terminalTransitions(lastStates, jobs);
  lastStates = snapshotStates(jobs);
  for (const job of finished) await announce(job);

  // The engine is working: hold the socket (and therefore the worker) open.
  // When it stops, the hold is dropped and the socket lingers briefly before
  // closing, so a burst of downloads does not thrash the connection.
  if (hasActiveJobs(jobs)) feed?.hold('jobs');
  else feed?.release('jobs');

  pushToUi(feed?.isConnected ?? false);
}

/** One notification per job, per outcome. */
async function announce(job: JobProgress): Promise<void> {
  if (job.state === 'canceled') return; // the user did that on purpose
  const settings = await getSettings();
  if (settings.notifyOnComplete === false) return;
  if (await alreadyNotified(job.id)) return;

  const ok = job.state === 'completed';
  try {
    chrome.notifications.create(`qd-${job.id}`, {
      type: 'basic',
      iconUrl: chrome.runtime.getURL('icons/icon128.png'),
      title: ok ? 'Download complete' : 'Download failed',
      message: ok ? job.filename || job.url : `${job.filename || job.url}\n${job.error ?? ''}`.trim(),
      silent: false,
    });
  } catch (err) {
    console.debug('[quick-download] notification failed', err);
  }
}

// ---------------------------------------------------------------------------
// Fan-out to the UI
// ---------------------------------------------------------------------------

let pushPending = false;

/**
 * Forwards the snapshot to every open popup.
 *
 * Throttled: the engine broadcasts several times a second, and the popup only
 * repaints a handful of progress bars from it. A dropped frame costs nothing
 * because every snapshot is absolute.
 */
function pushToUi(connected: boolean): void {
  if (uiPorts.size === 0 || pushPending) return;
  pushPending = true;
  setTimeout(() => {
    pushPending = false;
    const message: ProgressPush = { type: 'snapshot', jobs: latestJobs, connected };
    for (const port of uiPorts) {
      try {
        port.postMessage(message);
      } catch {
        // The popup went away between the check and the post.
        uiPorts.delete(port);
      }
    }
  }, UI_PUSH_MS);
}

/** ~4 repaints a second is smooth; the engine's full rate is not worth relaying. */
const UI_PUSH_MS = 250;

chrome.runtime.onConnect.addListener((port) => {
  if (port.name !== PROGRESS_PORT) return;
  uiPorts.add(port);

  void (async () => {
    const engine = await ensureFeed();
    // A watching popup is a reason to be connected even with nothing running:
    // the user is looking at the list and expects it to be live.
    engine?.hold('ui');
    try {
      port.postMessage({
        type: 'snapshot',
        jobs: latestJobs,
        connected: engine?.isConnected ?? false,
      } satisfies ProgressPush);
    } catch {
      /* closed already */
    }
  })();

  port.onDisconnect.addListener(() => {
    uiPorts.delete(port);
    // onDisconnect is the precise "the popup is gone" signal that repeated
    // sendMessage calls could never give us.
    if (uiPorts.size === 0) feed?.release('ui');
  });
});

/**
 * Keeps the feed up across the gap between queuing a download and the engine
 * reporting it, which is when there is nothing yet for the 'jobs' hold to see.
 */
function holdForEnqueue(): void {
  void (async () => {
    const engine = await ensureFeed();
    engine?.hold('enqueue');
    setTimeout(() => engine?.release('enqueue'), ENQUEUE_HOLD_MS);
  })();
}

const ENQUEUE_HOLD_MS = 30_000;

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
            quality: message.quality,
          }),
        );
        break;

      case 'clearAll': {
        // Everything goes except the ids the popup says are still downloading:
        // removing a row mid-transfer would orphan a job the user can no longer
        // see, while the engine carries on writing the file.
        const keep = new Set(message.keepIds);
        const remaining = (await getItems()).filter((i) => {
          if (keep.has(i.id)) return true;
          return message.tabId !== undefined && i.tabId !== message.tabId;
        });
        await setItems(remaining);
        await refreshBadge();
        sendResponse({ ok: true });
        break;
      }

      case 'forget': {
        // Auto-cleanup and "Clear finished" both drop items from the store, so
        // they do not reappear the next time the popup is opened.
        const drop = new Set(message.ids);
        const remaining = (await getItems()).filter((i) => !drop.has(i.id));
        await setItems(remaining);
        await refreshBadge();
        sendResponse({ ok: true });
        break;
      }

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

      case 'engineInfo': {
        // ping also starts the daemon if it is not running, and its reply
        // carries the dashboard origin the popup turns into a ws:// URL.
        const pong = await sendToHost({ type: 'ping' });
        if (pong.dashboard) engineOrigin = pong.dashboard;
        const info: EngineInfo = {
          ok: pong.ok,
          version: pong.version,
          toolsReady: pong.toolsReady,
          dashboard: pong.dashboard,
          error: pong.error,
        };
        sendResponse(info);
        break;
      }

      case 'progress': {
        // What a popup asks for the moment it opens, before its port has had a
        // chance to deliver anything.
        const engine = await ensureFeed();
        engine?.hold('ui');
        sendResponse({ jobs: latestJobs, connected: engine?.isConnected ?? false });
        break;
      }

      case 'downloadFromPage': {
        // The floating button on the page. Same path as the context menu, so it
        // inherits the save path, the cookie allowlist and the default quality.
        const response = await startDownload(message.url, {
          pageUrl: message.pageUrl,
          kind: message.streamType,
          title: message.title,
        });
        sendResponse(response);
        break;
      }

      case 'getSettings':
        sendResponse(await getSettings());
        break;

      case 'setSettings': {
        const next = await saveSettings(message.settings);
        if (message.settings.enabled !== undefined) {
          await paintActionIcon(next.enabled);
          if (!next.enabled) await chrome.action.setBadgeText({ text: '' });
        }
        sendResponse(next);
        break;
      }

      default:
        sendResponse({ ok: false, error: 'unknown message' });
    }
  })();

  // true = "I will call sendResponse asynchronously", which keeps the message
  // channel open. Forgetting this is the classic MV3 popup bug.
  return true;
});
