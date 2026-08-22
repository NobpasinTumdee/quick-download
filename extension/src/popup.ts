/**
 * Popup / side panel controller.
 *
 * The same document serves both surfaces: the side panel loads it with
 * `?panel=1`, which widens the layout and hides the "open in side panel"
 * button. Everything below is shared.
 */

import { ProgressFeed, normaliseUrl, type ProgressMap } from './progress.js';
import type {
  EngineInfo,
  HostResponse,
  JobProgress,
  MediaItem,
  MediaKind,
  Settings,
  ThemePreference,
  UiMessage,
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
let feed: ProgressFeed | null = null;

/** Job ids for downloads started from this popup, so they match immediately. */
const startedJobs = new Map<string, string>();

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

    const btn = q<HTMLButtonElement>('[data-download]');
    btn.addEventListener('click', async () => {
      btn.disabled = true;
      btn.textContent = '…';
      const res = await ask<HostResponse>({ kind: 'download', item });
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
    if (item) paintProgress(node as HTMLElement, item);
  });
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

async function connectFeed(dashboard: string): Promise<void> {
  feed?.stop();
  feed = new ProgressFeed({
    dashboard,
    onUpdate: (jobs) => {
      progress = jobs;
      repaintProgressOnly();
    },
  });
  feed.start();
}

async function checkEngine(): Promise<void> {
  const info = await ask<EngineInfo>({ kind: 'engineInfo' });
  const ok = !!info?.ok;
  el.engine.className = `engine ${ok ? 'online' : 'offline'}`;
  el.engine.textContent = ok ? `engine v${info.version ?? '?'}` : 'engine offline';
  if (!ok && info?.error) el.engine.title = info.error;

  el.warning.hidden = !ok || info.toolsReady !== false;

  if (ok) await connectFeed(info.dashboard ?? 'http://127.0.0.1:9090');
}

function applyEnabled(enabled: boolean): void {
  el.master.checked = enabled;
  document.body.classList.toggle('disabled', !enabled);
  el.paused.hidden = enabled;
}

async function loadSettings(): Promise<Settings | undefined> {
  const s = await ask<Settings>({ kind: 'getSettings' });
  if (!s) return undefined;
  applyEnabled(s.enabled !== false);
  applyTheme(s.theme ?? 'system');
  $<HTMLInputElement>('set-intercept').checked = s.interceptChromeDownloads;
  $<HTMLInputElement>('set-images').checked = s.captureImages;
  $<HTMLInputElement>('set-streams').checked = s.captureStreams;
  $<HTMLInputElement>('set-thumbs').checked = s.captureThumbnails;
  $<HTMLSelectElement>('set-minimage').value = String(s.minImageBytes);
  return s;
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
  await ask({ kind: 'clear', tabId: scope === 'tab' ? currentTabId : undefined });
  startedJobs.clear();
  void refresh();
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
  applyEnabled(next.enabled !== false);
  applyTheme(next.theme ?? 'system');
});

// ---------------------------------------------------------------------- boot

void (async () => {
  if (IS_PANEL) document.documentElement.classList.add('panel');

  const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
  currentTabId = tab?.id;

  const settings = await loadSettings();
  await checkEngine();
  // A first paint that includes a thumbnail scan is the whole point of the
  // popup — but not while sniffing is switched off.
  await refresh(settings?.enabled !== false);
})();
