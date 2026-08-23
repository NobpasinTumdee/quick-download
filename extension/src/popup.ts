/**
 * Popup / side panel controller.
 *
 * The same document serves both surfaces: the side panel loads it with
 * `?panel=1`, which widens the layout and hides the "open in side panel"
 * button. Everything below is shared.
 */

import { parseDomainList } from './filters.js';
import { normaliseUrl, toProgressMap, type ProgressMap } from './progress.js';
import {
  AUTO_CLEANUP_DELAY_MS,
  QUALITY_LABELS,
  type EngineInfo,
  type HostResponse,
  type JobProgress,
  type MediaItem,
  type MediaKind,
  PROGRESS_PORT,
  type ProgressPush,
  type Quality,
  type Settings,
  type ThemePreference,
  type UiMessage,
} from './types.js';

const $ = <T extends HTMLElement>(id: string): T => document.getElementById(id) as T;

const el = {
  list: $<HTMLUListElement>('list'),
  empty: $<HTMLParagraphElement>('empty'),
  engine: $<HTMLSpanElement>('engine'),
  warning: $<HTMLDivElement>('tools-warning'),
  paused: $<HTMLDivElement>('paused'),
  tpl: $<HTMLTemplateElement>('item-tpl'),
  master: $<HTMLInputElement>('master-toggle'),
  theme: $<HTMLButtonElement>('theme-btn'),
  panel: $<HTMLButtonElement>('panel-btn'),
};

const IS_PANEL = new URLSearchParams(location.search).get('panel') === '1';

let scope: 'tab' | 'all' = 'tab';
let currentTabId: number | undefined;
let items: MediaItem[] = [];
let progress: ProgressMap = new Map();
let progressPort: chrome.runtime.Port | null = null;
let closing = false;

/** Job ids for downloads started from this popup, so they match immediately. */
const startedJobs = new Map<string, string>();

/** Live settings, refreshed whenever storage changes. */
let settings: Settings | undefined;

/** Pending auto-cleanup timers, keyed by item id, so they can be cancelled. */
const cleanupTimers = new Map<string, ReturnType<typeof setTimeout>>();

/** Promise wrapper around chrome.runtime.sendMessage. */
function ask<T>(message: UiMessage): Promise<T> {
  return new Promise((resolve) => {
    chrome.runtime.sendMessage(message, (response) => {
      void chrome.runtime.lastError; // swallow "no receiver" noise
      resolve(response as T);
    });
  });
}

// ------------------------------------------------------------------ formatting

function bytes(n: number): string {
  if (!n || !Number.isFinite(n)) return '';
  const units = ['B', 'KB', 'MB', 'GB'];
  const i = Math.min(units.length - 1, Math.floor(Math.log(n) / Math.log(1024)));
  const v = n / 1024 ** i;
  return `${v >= 100 || i === 0 ? Math.round(v) : v.toFixed(1)} ${units[i]}`;
}

function clock(seconds?: number): string {
  if (!seconds || !Number.isFinite(seconds) || seconds < 0) return '';
  const s = Math.round(seconds);
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const sec = s % 60;
  const pad = (n: number) => String(n).padStart(2, '0');
  return h > 0 ? `${h}:${pad(m)}:${pad(sec)}` : `${m}:${pad(sec)}`;
}

const ICONS: Record<MediaKind, string> = {
  video: '🎬',
  audio: '🎵',
  image: '🖼️',
  stream: '📡',
  other: '📦',
};

function subtitle(item: MediaItem): string {
  const parts: string[] = [];
  if (item.label) parts.push(item.label);
  else if (item.mime) parts.push(item.mime);
  else parts.push(item.kind);

  if (item.width && item.height) parts.push(`${item.width}×${item.height}`);
  const size = bytes(item.size);
  if (size) parts.push(size);
  try {
    parts.push(new URL(item.url).hostname);
  } catch {
    /* not a parseable URL */
  }
  return parts.join(' · ');
}

// ------------------------------------------------------------------- rendering

/** Finds the engine job matching an item, by started-job id then by URL. */
function jobFor(item: MediaItem): JobProgress | undefined {
  const startedId = startedJobs.get(item.id);
  if (startedId) {
    const byId = progress.get(startedId);
    if (byId) return byId;
  }
  return progress.get(normaliseUrl(item.url));
}

const ACTIVE_STATES = new Set(['queued', 'probing', 'downloading', 'merging']);

function render(): void {
  el.list.replaceChildren();
  el.empty.hidden = items.length > 0;

  for (const item of items) {
    const node = el.tpl.content.firstElementChild!.cloneNode(true) as HTMLLIElement;
    const q = <T extends HTMLElement>(sel: string) => node.querySelector(sel) as T;

    const thumbBox = q<HTMLDivElement>('.thumb');
    const img = q<HTMLImageElement>('[data-thumb]');
    q<HTMLSpanElement>('[data-fallback]').textContent = ICONS[item.kind] ?? ICONS.other;

    if (item.thumbnail) {
      // Only reveal the <img> once it decodes: a dead poster URL or an expired
      // YouTube still would otherwise leave a broken-image glyph behind.
      img.addEventListener('load', () => thumbBox.classList.add('has-image'), { once: true });
      img.addEventListener('error', () => img.removeAttribute('src'), { once: true });
      img.src = item.thumbnail;
    }

    const badge = q<HTMLSpanElement>('[data-badge]');
    if (item.streamType !== 'direct') {
      badge.textContent = item.streamType === 'site' ? 'page' : item.streamType;
      badge.hidden = false;
    }

    const duration = q<HTMLSpanElement>('[data-duration]');
    const clockText = clock(item.duration);
    if (clockText) {
      duration.textContent = clockText;
      duration.hidden = false;
    }

    q<HTMLParagraphElement>('[data-name]').textContent = item.filename;
    q<HTMLParagraphElement>('[data-name]').title = item.url;
    q<HTMLParagraphElement>('[data-sub]').textContent = subtitle(item);

    // The quality picker only appears for items yt-dlp handles. A direct file
    // URL has exactly one representation, so offering resolutions there would
    // promise something the engine cannot deliver.
    const quality = q<HTMLSelectElement>('[data-quality]');
    if (item.kind === 'stream') {
      quality.replaceChildren(
        ...QUALITY_LABELS.map(({ value, label }) => {
          const option = document.createElement('option');
          option.value = value;
          option.textContent = label;
          return option;
        }),
      );
      quality.value = settings?.defaultQuality ?? 'best';
      quality.hidden = false;
    }

    const btn = q<HTMLButtonElement>('[data-download]');
    btn.addEventListener('click', async () => {
      btn.disabled = true;
      btn.textContent = '…';
      const res = await ask<HostResponse>({
        kind: 'download',
        item,
        quality: quality.hidden ? undefined : (quality.value as Quality),
      });
      if (res?.ok) {
        if (res.jobId) startedJobs.set(item.id, res.jobId);
        btn.textContent = 'Queued';
      } else {
        btn.textContent = 'Failed';
        btn.disabled = false;
        if (res?.error) btn.title = res.error;
      }
    });

    paintProgress(node, item);
    el.list.append(node);
  }
}

/** Draws the live bar for one item, if the engine is working on it. */
function paintProgress(node: HTMLElement, item: MediaItem): void {
  const box = node.querySelector('[data-progress]') as HTMLDivElement;
  const job = jobFor(item);
  if (!job) {
    box.hidden = true;
    return;
  }

  box.hidden = false;
  box.dataset.state = job.state;

  const pct = Math.max(0, Math.min(100, job.progress || 0));
  (node.querySelector('[data-fill]') as HTMLElement).style.width = `${pct.toFixed(1)}%`;

  const state = node.querySelector('[data-pstate]') as HTMLElement;
  state.textContent = job.state === 'downloading' && job.phase ? job.phase : job.state;

  (node.querySelector('[data-ppercent]') as HTMLElement).textContent = ACTIVE_STATES.has(job.state)
    ? `${pct.toFixed(0)}%`
    : '';
  (node.querySelector('[data-pspeed]') as HTMLElement).textContent =
    job.state === 'downloading' && job.speed > 0 ? `${bytes(job.speed)}/s` : '';
  (node.querySelector('[data-peta]') as HTMLElement).textContent =
    job.state === 'downloading' && job.eta > 0 ? `ETA ${clock(job.eta)}` : '';

  const btn = node.querySelector('[data-download]') as HTMLButtonElement;
  if (ACTIVE_STATES.has(job.state)) {
    btn.disabled = true;
    btn.textContent = 'Downloading';
  } else if (job.state === 'completed') {
    btn.disabled = true;
    btn.textContent = 'Done';
  } else if (job.state === 'failed') {
    btn.disabled = false;
    btn.textContent = 'Retry';
    btn.title = job.error ?? '';
  }
}

/**
 * Progress updates arrive several times a second. Re-rendering the whole list
 * that often would fight the user's scrolling, so only the bars are repainted.
 */
function repaintProgressOnly(): void {
  const nodes = el.list.querySelectorAll('li');
  nodes.forEach((node, index) => {
    const item = items[index];
    if (!item) return;
    paintProgress(node as HTMLElement, item);
    handleCompletion(item, node as HTMLElement);
  });
}

/**
 * Schedules auto-cleanup for a finished item.
 *
 * The notification that goes with it is raised by the service worker, which now
 * owns the engine socket. Doing it here as well would announce every download
 * twice whenever a popup happened to be open - and announce nothing at all,
 * which was the old behaviour, whenever one was not.
 */
function handleCompletion(item: MediaItem, node: HTMLElement): void {
  const job = jobFor(item);
  if (!job || job.state !== 'completed') return;
  if (settings?.autoCleanup !== false) scheduleCleanup(item, node);
}

function scheduleCleanup(item: MediaItem, node: HTMLElement): void {
  if (cleanupTimers.has(item.id)) return;
  const timer = setTimeout(() => {
    cleanupTimers.delete(item.id);
    // Animate out, then drop it from both the list and the store.
    node.classList.add('leaving');
    setTimeout(() => void forget([item.id]), 260);
  }, AUTO_CLEANUP_DELAY_MS);
  cleanupTimers.set(item.id, timer);
}

/** Removes items from the store and re-renders. */
async function forget(ids: string[]): Promise<void> {
  if (!ids.length) return;
  for (const id of ids) {
    const timer = cleanupTimers.get(id);
    if (timer !== undefined) {
      clearTimeout(timer);
      cleanupTimers.delete(id);
    }
    startedJobs.delete(id);
  }
  await ask({ kind: 'forget', ids });
  items = items.filter((i) => !ids.includes(i.id));
  render();
}

/** Cancels every pending cleanup timer; used when the popup goes away. */
function clearCleanupTimers(): void {
  for (const timer of cleanupTimers.values()) clearTimeout(timer);
  cleanupTimers.clear();
}

// --------------------------------------------------------------------- theming

function resolveTheme(preference: ThemePreference): 'dark' | 'light' {
  if (preference === 'dark' || preference === 'light') return preference;
  return matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark';
}

function applyTheme(preference: ThemePreference): void {
  const resolved = resolveTheme(preference);
  // The CSS carries both palettes; setting the attribute picks one explicitly
  // and overrides the prefers-color-scheme default.
  document.documentElement.dataset.theme = resolved;
  el.theme.textContent = resolved === 'dark' ? '🌙' : '☀️';
  el.theme.title = resolved === 'dark' ? 'Switch to light theme' : 'Switch to dark theme';
}

// ----------------------------------------------------------------- data loading

async function refresh(withThumbnails = false): Promise<void> {
  const result = await ask<MediaItem[]>({
    kind: 'list',
    tabId: scope === 'tab' ? currentTabId : undefined,
    withThumbnails: withThumbnails && scope === 'tab',
  });
  items = result ?? [];
  render();
}

/**
 * Subscribes to the worker's engine feed.
 *
 * The popup no longer opens a WebSocket of its own. It was the wrong owner: a
 * popup is destroyed the moment it loses focus, so the connection - and every
 * notification that depended on it - died with it. The worker holds the socket
 * now, and this port is both how snapshots arrive and how the worker knows a UI
 * is watching (its onDisconnect is what releases the socket).
 */
function subscribeToProgress(): void {
  if (closing) return;

  let port: chrome.runtime.Port;
  try {
    port = chrome.runtime.connect({ name: PROGRESS_PORT });
  } catch {
    // The worker is being replaced (an extension update, say).
    setTimeout(subscribeToProgress, RESUBSCRIBE_MS);
    return;
  }
  progressPort = port;

  port.onMessage.addListener((message: ProgressPush) => {
    if (message?.type !== 'snapshot') return;
    progress = toProgressMap(message.jobs ?? []);
    showEngineConnection(message.connected);
    repaintProgressOnly();
  });

  port.onDisconnect.addListener(() => {
    progressPort = null;
    // A worker that went idle is normal, not an error: reconnecting wakes it.
    if (!closing) setTimeout(subscribeToProgress, RESUBSCRIBE_MS);
  });
}

const RESUBSCRIBE_MS = 600;

/** Reflects the worker's socket state in the engine chip. */
function showEngineConnection(connected: boolean): void {
  if (!el.engine.textContent) return;
  el.engine.classList.toggle('online', connected);
  el.engine.classList.toggle('offline', !connected);
}

/** Asks the worker for whatever it already has, so the first paint is not empty. */
async function primeProgress(): Promise<void> {
  const snapshot = await ask<{ jobs?: JobProgress[]; connected?: boolean }>({ kind: 'progress' });
  if (!snapshot) return;
  progress = toProgressMap(snapshot.jobs ?? []);
  showEngineConnection(snapshot.connected === true);
}

async function checkEngine(): Promise<void> {
  const info = await ask<EngineInfo>({ kind: 'engineInfo' });
  const ok = !!info?.ok;
  el.engine.className = `engine ${ok ? 'online' : 'offline'}`;
  el.engine.textContent = ok ? `engine v${info.version ?? '?'}` : 'engine offline';
  if (!ok && info?.error) el.engine.title = info.error;

  el.warning.hidden = !ok || info.toolsReady !== false;

  if (ok) await primeProgress();
}

function applyEnabled(enabled: boolean): void {
  el.master.checked = enabled;
  document.body.classList.toggle('disabled', !enabled);
  el.paused.hidden = enabled;
}

async function loadSettings(): Promise<Settings | undefined> {
  const s = await ask<Settings>({ kind: 'getSettings' });
  if (!s) return undefined;
  settings = s;
  applyEnabled(s.enabled !== false);
  applyTheme(s.theme ?? 'system');
  $<HTMLInputElement>('set-intercept').checked = s.interceptChromeDownloads;
  $<HTMLInputElement>('set-images').checked = s.captureImages;
  $<HTMLInputElement>('set-streams').checked = s.captureStreams;
  $<HTMLInputElement>('set-thumbs').checked = s.captureThumbnails;
  $<HTMLSelectElement>('set-minimage').value = String(s.minImageBytes);
  $<HTMLSelectElement>('set-quality').value = s.defaultQuality ?? 'best';
  $<HTMLInputElement>('set-notify').checked = s.notifyOnComplete !== false;
  $<HTMLInputElement>('set-autoclean').checked = s.autoCleanup !== false;
  $<HTMLInputElement>('set-savepath').value = s.savePath ?? '';
  showPathStatus(s.savePath ?? '');
  $<HTMLInputElement>('set-smart').checked = s.smartFilter !== false;
  $<HTMLInputElement>('set-minsize').value = String(s.minFileSizeKB ?? 0);
  $<HTMLTextAreaElement>('set-cookies').value = (s.cookieAllowlist ?? []).join('\n');
  showCookieStatus(s.cookieAllowlist ?? []);
  return s;
}

/** Spells out what the allowlist means, since it governs credential sharing. */
function showCookieStatus(domains: readonly string[]): void {
  const status = $<HTMLSpanElement>('cookie-status');
  if (!domains.length) {
    status.className = 'path-status good';
    status.textContent = 'No cookies are sent anywhere.';
    return;
  }
  status.className = 'path-status';
  status.textContent = `Cookies are sent only to: ${domains.join(', ')}.`;
}

/**
 * The engine is the authority on whether a path works - it has to create and
 * write to it - so this is only a shape check to catch obvious typos early.
 */
function showPathStatus(path: string): void {
  const status = $<HTMLSpanElement>('path-status');
  const value = path.trim();
  if (!value) {
    status.className = 'path-status';
    status.textContent = '';
    return;
  }
  const absolute = /^[a-zA-Z]:[\\/]/.test(value) || value.startsWith('/') || value.startsWith('\\\\');
  status.className = `path-status ${absolute ? 'good' : 'bad'}`;
  status.textContent = absolute
    ? 'Saved. The engine creates this folder if it is missing.'
    : 'Use an absolute path, e.g. D:\\Media\\Downloads';
}

// ------------------------------------------------------------------- listeners

document.querySelectorAll<HTMLButtonElement>('.tab').forEach((tab) => {
  tab.addEventListener('click', () => {
    document.querySelectorAll('.tab').forEach((t) => {
      t.classList.remove('active');
      t.setAttribute('aria-selected', 'false');
    });
    tab.classList.add('active');
    tab.setAttribute('aria-selected', 'true');
    scope = tab.dataset.scope === 'all' ? 'all' : 'tab';
    void refresh();
  });
});

$<HTMLButtonElement>('refresh').addEventListener('click', async (event) => {
  const btn = event.currentTarget as HTMLButtonElement;
  btn.disabled = true;
  await refresh(true);
  btn.disabled = false;
});

$<HTMLButtonElement>('clear').addEventListener('click', async () => {
  // Anything the engine is still working on stays: hiding a row mid-transfer
  // would leave a job running that the user can no longer see or cancel.
  const active = items.filter((item) => {
    const job = jobFor(item);
    return job !== undefined && ACTIVE_STATES.has(job.state);
  });
  const keepIds = active.map((item) => item.id);

  clearCleanupTimers();
  for (const id of [...startedJobs.keys()]) {
    if (!keepIds.includes(id)) startedJobs.delete(id);
  }

  await ask({
    kind: 'clearAll',
    tabId: scope === 'tab' ? currentTabId : undefined,
    keepIds,
  });
  items = active;
  render();
});

$<HTMLButtonElement>('dashboard').addEventListener('click', () => {
  void ask({ kind: 'openDashboard' });
});

el.master.addEventListener('change', async () => {
  const enabled = el.master.checked;
  applyEnabled(enabled);
  await ask({ kind: 'setSettings', settings: { enabled } });
});

el.theme.addEventListener('click', async () => {
  const next: ThemePreference =
    document.documentElement.dataset.theme === 'dark' ? 'light' : 'dark';
  applyTheme(next);
  await ask({ kind: 'setSettings', settings: { theme: next } });
});

el.panel.addEventListener('click', async () => {
  try {
    const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
    // sidePanel.open() requires a user gesture, which this click is. It must be
    // called before any await that would end the gesture's grace period, so the
    // tab lookup above is the only thing preceding it.
    if (tab?.windowId !== undefined) {
      await chrome.sidePanel.open({ windowId: tab.windowId });
    }
    window.close();
  } catch (err) {
    el.panel.title = `Side panel unavailable: ${String(err)}`;
  }
});

$<HTMLFormElement>('manual').addEventListener('submit', async (event) => {
  event.preventDefault();
  const input = $<HTMLInputElement>('manual-url');
  const url = input.value.trim();
  if (!url) return;
  const res = await ask<HostResponse>({ kind: 'downloadUrl', url });
  input.value = '';
  if (!res?.ok) el.engine.title = res?.error ?? 'failed';
});

const toggles: Array<[string, keyof Settings]> = [
  ['set-intercept', 'interceptChromeDownloads'],
  ['set-images', 'captureImages'],
  ['set-streams', 'captureStreams'],
  ['set-thumbs', 'captureThumbnails'],
];
for (const [id, key] of toggles) {
  $<HTMLInputElement>(id).addEventListener('change', (e) => {
    void ask({
      kind: 'setSettings',
      settings: { [key]: (e.target as HTMLInputElement).checked } as Partial<Settings>,
    });
  });
}

$<HTMLButtonElement>('clear-finished').addEventListener('click', async () => {
  // Anything the engine reports as finished, including items whose auto-cleanup
  // never ran because the popup was closed at the time.
  const done = items
    .filter((item) => {
      const job = jobFor(item);
      return job && (job.state === 'completed' || job.state === 'canceled');
    })
    .map((item) => item.id);
  await forget(done);
});

$<HTMLTextAreaElement>('set-cookies').addEventListener('change', (e) => {
  const cookieAllowlist = parseDomainList((e.target as HTMLTextAreaElement).value);
  if (settings) settings.cookieAllowlist = cookieAllowlist;
  // Write the cleaned list back so the user sees exactly what was stored.
  (e.target as HTMLTextAreaElement).value = cookieAllowlist.join('\n');
  showCookieStatus(cookieAllowlist);
  void ask({ kind: 'setSettings', settings: { cookieAllowlist } });
});

$<HTMLInputElement>('set-smart').addEventListener('change', (e) => {
  const smartFilter = (e.target as HTMLInputElement).checked;
  if (settings) settings.smartFilter = smartFilter;
  void ask({ kind: 'setSettings', settings: { smartFilter } });
});

$<HTMLInputElement>('set-minsize').addEventListener('change', (e) => {
  const input = e.target as HTMLInputElement;
  const minFileSizeKB = Math.max(0, Math.round(Number(input.value) || 0));
  input.value = String(minFileSizeKB);
  if (settings) settings.minFileSizeKB = minFileSizeKB;
  void ask({ kind: 'setSettings', settings: { minFileSizeKB } });
});

$<HTMLSelectElement>('set-quality').addEventListener('change', (e) => {
  const defaultQuality = (e.target as HTMLSelectElement).value as Quality;
  if (settings) settings.defaultQuality = defaultQuality;
  void ask({ kind: 'setSettings', settings: { defaultQuality } });
  render();
});

$<HTMLInputElement>('set-savepath').addEventListener('change', (e) => {
  const savePath = (e.target as HTMLInputElement).value.trim();
  if (settings) settings.savePath = savePath;
  showPathStatus(savePath);
  void ask({ kind: 'setSettings', settings: { savePath } });
});

$<HTMLInputElement>('set-savepath').addEventListener('input', (e) => {
  showPathStatus((e.target as HTMLInputElement).value);
});

for (const [id, key] of [
  ['set-notify', 'notifyOnComplete'],
  ['set-autoclean', 'autoCleanup'],
] as Array<[string, keyof Settings]>) {
  $<HTMLInputElement>(id).addEventListener('change', (e) => {
    const value = (e.target as HTMLInputElement).checked;
    if (settings) (settings as unknown as Record<string, unknown>)[key] = value;
    void ask({ kind: 'setSettings', settings: { [key]: value } as Partial<Settings> });
  });
}

// Pending cleanup timers must not outlive the document, and a port that
// reconnects on disconnect must be told this disconnect was the last one.
addEventListener('pagehide', () => {
  closing = true;
  clearCleanupTimers();
  progressPort?.disconnect();
  progressPort = null;
});

$<HTMLSelectElement>('set-minimage').addEventListener('change', (e) => {
  void ask({
    kind: 'setSettings',
    settings: { minImageBytes: Number((e.target as HTMLSelectElement).value) },
  });
});

// Keep both surfaces in step: flipping the switch in the popup updates an open
// side panel, and vice versa.
chrome.storage.onChanged.addListener((changes, area) => {
  if (area !== 'local' || !changes.settings) return;
  const next = changes.settings.newValue as Settings | undefined;
  if (!next) return;
  settings = next;
  applyEnabled(next.enabled !== false);
  applyTheme(next.theme ?? 'system');
});

// ---------------------------------------------------------------------- boot

void (async () => {
  if (IS_PANEL) document.documentElement.classList.add('panel');

  const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
  currentTabId = tab?.id;

  const loaded = await loadSettings();
  await checkEngine();
  subscribeToProgress();
  // A first paint that includes a thumbnail scan is the whole point of the
  // popup — but not while sniffing is switched off.
  await refresh(loaded?.enabled !== false);
})();
