/**
 * Right-click integration.
 *
 * The popup is the discovery surface: it lists what the sniffer saw. The
 * context menu is the deliberate one - the user already knows what they want,
 * points at it, and expects the download to start without a UI opening.
 *
 * That difference drives two decisions here:
 *
 *   - The master switch is not consulted. It governs passive sniffing; an
 *     explicit right-click is intent, and silently ignoring it would look like
 *     a bug.
 *   - Every click gets a notification. With no popup open there is nowhere else
 *     for feedback to go, and a download that starts invisibly is
 *     indistinguishable from one that failed.
 */

import { cleanPageUrl } from './filters.js';
import type { HostResponse, StreamType } from './types.js';

// ---------------------------------------------------------------------------
// Menu definition
// ---------------------------------------------------------------------------

export const MENU_PARENT = 'qd-root';
export const MENU_LINK = 'qd-link';
export const MENU_MEDIA = 'qd-media';
export const MENU_IMAGE = 'qd-image';
export const MENU_PAGE = 'qd-page';
export const MENU_DASHBOARD = 'qd-dashboard';

/** Pages the menu appears on. chrome:// and the Web Store are not downloadable. */
const PAGE_PATTERNS = ['http://*/*', 'https://*/*'];

/**
 * Targets we will hand to the engine. A blob: or data: URL exists only inside
 * the renderer, so a local process cannot fetch it - offering the item would
 * promise a download that cannot happen. For an MSE player (whose <video> src
 * is always a blob:) "Download Current Page" is the answer, and it stays.
 */
const TARGET_PATTERNS = ['http://*/*', 'https://*/*'];

/**
 * The menu, as data. Chrome persists context menus across restarts, so this is
 * also the definition installContextMenus replays after removeAll.
 */
export const MENU_ITEMS: ReadonlyArray<chrome.contextMenus.CreateProperties> = [
  {
    id: MENU_PARENT,
    title: 'Quick Download',
    // A parent shows only where at least one child could, so its contexts must
    // be the union of theirs.
    contexts: ['link', 'video', 'audio', 'image', 'page', 'frame'],
    documentUrlPatterns: PAGE_PATTERNS,
  },
  {
    id: MENU_LINK,
    parentId: MENU_PARENT,
    title: 'Download this Link',
    contexts: ['link'],
    targetUrlPatterns: TARGET_PATTERNS,
  },
  {
    id: MENU_MEDIA,
    parentId: MENU_PARENT,
    title: 'Download this Media',
    contexts: ['video', 'audio'],
    targetUrlPatterns: TARGET_PATTERNS,
  },
  {
    id: MENU_IMAGE,
    parentId: MENU_PARENT,
    title: 'Download this Image',
    contexts: ['image'],
    targetUrlPatterns: TARGET_PATTERNS,
  },
  {
    id: MENU_PAGE,
    parentId: MENU_PARENT,
    title: 'Download Current Page',
    contexts: ['page', 'frame'],
  },
  {
    id: MENU_DASHBOARD,
    title: 'Open Quick Download dashboard',
    contexts: ['action'],
  },
];

/**
 * Creates the menu, replacing whatever is there.
 *
 * removeAll first makes this idempotent, which matters: menu ids are unique and
 * a second create() with the same id fails. Being safe to re-run lets us call
 * it from onStartup as well as onInstalled, so a profile whose menus were lost
 * repairs itself on the next browser launch instead of needing a reinstall.
 */
export function installContextMenus(): void {
  chrome.contextMenus.removeAll(() => {
    void chrome.runtime.lastError; // nothing to remove is not an error
    for (const item of MENU_ITEMS) {
      chrome.contextMenus.create(item, () => {
        const err = chrome.runtime.lastError;
        if (err) console.warn('[quick-download] menu %s: %s', item.id, err.message);
      });
    }
  });
}

// ---------------------------------------------------------------------------
// What a click means
// ---------------------------------------------------------------------------

/** The parts of a click we use. Structurally a chrome.contextMenus.OnClickData. */
export interface ContextClick {
  menuItemId: string | number;
  linkUrl?: string;
  srcUrl?: string;
  pageUrl?: string;
  frameUrl?: string;
}

/** The parts of the tab we use. */
export interface ContextTab {
  url?: string;
  title?: string;
}

export interface ContextTarget {
  url: string;
  /** 'site' sends the URL to yt-dlp's extractors instead of the chunk engine. */
  kind?: StreamType;
  /** What to call it in the notification. */
  label: string;
}

/**
 * Turns a click into the URL to download. Pure, so the precedence rules are
 * testable without a browser.
 *
 * Precedence is per menu item rather than global. A <video> wrapped in an <a>
 * produces both srcUrl and linkUrl, and which one the user meant is exactly
 * what the item they clicked tells us.
 */
export function resolveContextTarget(info: ContextClick, tab?: ContextTab): ContextTarget | null {
  switch (info.menuItemId) {
    case MENU_LINK:
      return describe(info.linkUrl, tab);

    case MENU_MEDIA:
    case MENU_IMAGE:
      // srcUrl is the media itself; linkUrl covers a thumbnail linking to it.
      return describe(info.srcUrl || info.linkUrl, tab);

    case MENU_PAGE: {
      // In a frame, the frame is the page the user means: an embedded player
      // is the whole reason to right-click "download this page".
      const inFrame = info.frameUrl && info.frameUrl !== info.pageUrl ? info.frameUrl : '';
      const page = cleanPageUrl(inFrame || tab?.url || info.pageUrl || '');
      if (!page) return null;
      return { url: page, kind: 'site', label: tab?.title || labelFor(page) };
    }

    default:
      return null;
  }
}

function describe(url: string | undefined, tab?: ContextTab): ContextTarget | null {
  if (!url || !/^https?:/i.test(url)) return null;
  return { url, label: labelFor(url) || tab?.title || url };
}

/** A short human name for a URL: its filename, else its host. */
export function labelFor(url: string): string {
  try {
    const parsed = new URL(url);
    const last = parsed.pathname.split('/').filter(Boolean).pop();
    if (last) return decodeURIComponent(last).slice(0, 80);
    return parsed.hostname;
  } catch {
    return url.slice(0, 80);
  }
}

// ---------------------------------------------------------------------------
// Wiring
// ---------------------------------------------------------------------------

export interface ContextMenuDeps {
  /** The one path to the engine, so cookies, savePath and quality all apply. */
  startDownload(
    url: string,
    opts: { pageUrl?: string; kind?: StreamType; title?: string },
  ): Promise<HostResponse>;
  openDashboard(): Promise<HostResponse>;
  /** The user's notification preference, read at click time. */
  notificationsEnabled(): Promise<boolean>;
}

/**
 * Registers the click handler. Called at the top level of the service worker:
 * a listener added later (inside a promise, say) can miss the event that woke
 * the worker in the first place.
 */
export function registerContextMenus(deps: ContextMenuDeps): void {
  chrome.contextMenus.onClicked.addListener((info, tab) => {
    void handleContextClick(info, tab, deps);
  });
}

/** Exported for tests: the whole click, minus the listener registration. */
export async function handleContextClick(
  info: ContextClick,
  tab: ContextTab | undefined,
  deps: ContextMenuDeps,
): Promise<void> {
  if (info.menuItemId === MENU_DASHBOARD) {
    await deps.openDashboard();
    return;
  }

  const target = resolveContextTarget(info, tab);
  if (!target) {
    console.warn('[quick-download] context click with no usable URL', info.menuItemId);
    return;
  }

  const notifyId = `qd-ctx-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 8)}`;
  const notify = await deps.notificationsEnabled();

  // Optimistic, and deliberately so: the round trip through the native host
  // wakes a sleeping daemon and can take a second or two. Feedback that arrives
  // after the user has moved on is not feedback.
  if (notify) showNotification(notifyId, 'Download queued', target.label, true);

  const response = await deps.startDownload(target.url, {
    pageUrl: tab?.url ?? info.pageUrl,
    kind: target.kind,
    title: tab?.title,
  });

  if (response.ok) {
    // Basic notifications linger on some platforms; this one has said its piece.
    setTimeout(() => chrome.notifications.clear(notifyId), QUEUED_NOTIFICATION_MS);
    return;
  }

  console.warn('[quick-download] context download failed:', response.error);
  if (!notify) return;

  // Same id: creating over an existing notification replaces it, so the queued
  // message is corrected in place rather than stacking a second card on top.
  showNotification(notifyId, 'Quick Download could not start', engineHint(response.error), false);
}

/** How long the "queued" card stays up before we take it down. */
export const QUEUED_NOTIFICATION_MS = 4000;

/**
 * Turns a native-messaging failure into something actionable. The common case
 * is not a crash but an engine that was never installed, or a host manifest
 * that does not name this extension id.
 */
export function engineHint(error?: string): string {
  const detail = (error ?? '').trim();
  if (/not found|no such|unreachable|manifest|specified file/i.test(detail) || detail === '') {
    return 'The local engine did not answer. Check that Quick Download is installed and its native host is registered.';
  }
  return detail.slice(0, 200);
}

function showNotification(id: string, title: string, message: string, silent: boolean): void {
  try {
    chrome.notifications.create(id, {
      type: 'basic',
      iconUrl: chrome.runtime.getURL('icons/icon128.png'),
      title,
      message,
      silent,
    });
  } catch (err) {
    // A missing notifications permission must never stop the download itself.
    console.debug('[quick-download] notification failed', err);
  }
}
