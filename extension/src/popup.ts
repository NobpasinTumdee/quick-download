/** Popup: thumbnail list of everything detected, plus one-click download. */

import type { HostResponse, MediaItem, MediaKind, Settings, UiMessage } from './types.js';

const $ = <T extends HTMLElement>(id: string): T => document.getElementById(id) as T;

const el = {
  list: $<HTMLUListElement>('list'),
  empty: $<HTMLParagraphElement>('empty'),
  engine: $<HTMLSpanElement>('engine'),
  warning: $<HTMLDivElement>('tools-warning'),
  tpl: $<HTMLTemplateElement>('item-tpl'),
};

let scope: 'tab' | 'all' = 'tab';
let currentTabId: number | undefined;

/** Promise wrapper around chrome.runtime.sendMessage. */
function ask<T>(message: UiMessage): Promise<T> {
  return new Promise((resolve) => {
    chrome.runtime.sendMessage(message, (response) => {
      void chrome.runtime.lastError; // swallow "no receiver" noise
      resolve(response as T);
    });
  });
}

function bytes(n: number): string {
  if (!n) return '';
  const units = ['B', 'KB', 'MB', 'GB'];
  const i = Math.min(units.length - 1, Math.floor(Math.log(n) / Math.log(1024)));
  const v = n / 1024 ** i;
  return `${v >= 100 || i === 0 ? Math.round(v) : v.toFixed(1)} ${units[i]}`;
}

function clock(seconds?: number): string {
  if (!seconds || !Number.isFinite(seconds)) return '';
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

/** Short right-hand description under the filename. */
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
    /* not a parseable URL, skip the host */
  }
  return parts.join(' · ');
}

function render(items: MediaItem[]): void {
  el.list.replaceChildren();
  el.empty.hidden = items.length > 0;

  for (const item of items) {
    const node = el.tpl.content.firstElementChild!.cloneNode(true) as HTMLLIElement;
    const q = <T extends HTMLElement>(sel: string) => node.querySelector(sel) as T;

    const thumbBox = q<HTMLDivElement>('.thumb');
    const img = q<HTMLImageElement>('[data-thumb]');
    q<HTMLSpanElement>('[data-fallback]').textContent = ICONS[item.kind] ?? ICONS.other;

    if (item.thumbnail) {
      // Only reveal the <img> once it actually decodes: a dead poster URL or a
      // stale data: URL would otherwise leave a broken-image glyph behind.
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
      btn.textContent = res?.ok ? 'Queued' : 'Failed';
      if (!res?.ok && res?.error) btn.title = res.error;
    });

    el.list.append(node);
  }
}

async function refresh(withThumbnails = false): Promise<void> {
  const items = await ask<MediaItem[]>({
    kind: 'list',
    tabId: scope === 'tab' ? currentTabId : undefined,
    // Scanning the DOM only makes sense for the tab we are looking at.
    withThumbnails: withThumbnails && scope === 'tab',
  });
  render(items ?? []);
}

async function checkEngine(): Promise<void> {
  const res = await ask<HostResponse>({ kind: 'ping' });
  const ok = !!res?.ok;
  el.engine.className = `engine ${ok ? 'online' : 'offline'}`;
  el.engine.textContent = ok ? `engine v${res.version ?? '?'}` : 'engine offline';
  if (!ok && res?.error) el.engine.title = res.error;

  // Only warn about missing tools once we know the engine is actually up.
  el.warning.hidden = !ok || res.toolsReady !== false;
}

async function loadSettings(): Promise<void> {
  const s = await ask<Settings>({ kind: 'getSettings' });
  if (!s) return;
  $<HTMLInputElement>('set-intercept').checked = s.interceptChromeDownloads;
  $<HTMLInputElement>('set-images').checked = s.captureImages;
  $<HTMLInputElement>('set-streams').checked = s.captureStreams;
  $<HTMLInputElement>('set-thumbs').checked = s.captureThumbnails;
  $<HTMLSelectElement>('set-minimage').value = String(s.minImageBytes);
}

// ------------------------------------------------------------------ listeners

document.querySelectorAll<HTMLButtonElement>('.tab').forEach((tab) => {
  tab.addEventListener('click', () => {
    document.querySelectorAll('.tab').forEach((t) => t.classList.remove('active'));
    tab.classList.add('active');
    scope = tab.dataset.scope === 'all' ? 'all' : 'tab';
    void refresh();
  });
});

$<HTMLButtonElement>('refresh').addEventListener('click', async (event) => {
  const btn = event.currentTarget as HTMLButtonElement;
  btn.disabled = true;
  btn.textContent = 'Scanning…';
  await refresh(true);
  btn.disabled = false;
  btn.textContent = 'Rescan';
});

$<HTMLButtonElement>('clear').addEventListener('click', async () => {
  await ask({ kind: 'clear', tabId: scope === 'tab' ? currentTabId : undefined });
  void refresh();
});

$<HTMLButtonElement>('dashboard').addEventListener('click', () => {
  void ask({ kind: 'openDashboard' });
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

// ---------------------------------------------------------------------- boot

void (async () => {
  const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
  currentTabId = tab?.id;
  await Promise.all([loadSettings(), checkEngine()]);
  // First paint includes a thumbnail scan: that is the whole point of the popup.
  await refresh(true);
})();
