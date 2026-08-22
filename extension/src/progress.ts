/**
 * Live progress feed for the popup and side panel.
 *
 * The Go engine broadcasts a full snapshot of every job over a WebSocket a few
 * times per second. This module owns that connection and hands the UI a plain
 * "url/id -> JobProgress" lookup.
 *
 * Lifetime is the whole point of keeping it separate. A popup is destroyed the
 * moment it loses focus, and an abandoned socket plus its reconnect timer would
 * leak a little more of the service worker's budget every time the user opened
 * it. `stop()` is idempotent and tears down the socket, the timer and every
 * listener, and it is wired to pagehide so it runs even when the popup is
 * dismissed without a click.
 */

import type { JobProgress } from './types.js';

/** Jobs keyed by their normalised URL, plus by job id. */
export type ProgressMap = Map<string, JobProgress>;

export interface ProgressFeedOptions {
  /** Engine origin, e.g. "http://127.0.0.1:9090". */
  dashboard: string;
  /** Called whenever a new snapshot arrives. */
  onUpdate: (jobs: ProgressMap) => void;
  /** Called when the transport state changes, for the status dot. */
  onStatus?: (connected: boolean) => void;
}

const RECONNECT_MIN_MS = 700;
const RECONNECT_MAX_MS = 8000;
const POLL_MS = 1500;

/**
 * Normalises a URL for matching a sniffed item against an engine job.
 *
 * The two can differ legitimately: the engine cleans YouTube URLs (dropping
 * `&list=`), and trailing slashes vary. Comparing raw strings would show no
 * progress on exactly the downloads the user most wants to watch.
 */
export function normaliseUrl(raw: string): string {
  try {
    const u = new URL(raw);
    const host = u.hostname.toLowerCase().replace(/^www\./, '');
    if (host === 'youtu.be') {
      const id = u.pathname.replace(/^\//, '').split('/')[0];
      if (id) return `youtube:${id}`;
    }
    if (host === 'youtube.com' || host.endsWith('.youtube.com')) {
      const v = u.searchParams.get('v');
      if (v) return `youtube:${v}`;
      const m = u.pathname.match(/^\/(?:shorts|live|embed|v)\/([\w-]+)/);
      if (m) return `youtube:${m[1]}`;
    }
    // Everything else: origin + path + query, minus a trailing slash.
    return (u.origin + u.pathname).replace(/\/$/, '') + u.search;
  } catch {
    return raw;
  }
}

export class ProgressFeed {
  private socket: WebSocket | null = null;
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private pollTimer: ReturnType<typeof setInterval> | null = null;
  private delay = RECONNECT_MIN_MS;
  private stopped = false;
  private readonly abort = new AbortController();

  constructor(private readonly options: ProgressFeedOptions) {}

  start(): void {
    if (this.stopped) return;

    // A popup can be dismissed without any click at all, so the teardown has to
    // hang off the document lifecycle rather than a button.
    addEventListener('pagehide', () => this.stop(), { signal: this.abort.signal });
    addEventListener('beforeunload', () => this.stop(), { signal: this.abort.signal });

    this.connect();
  }

  /** Idempotent teardown: socket, timers and listeners all go. */
  stop(): void {
    if (this.stopped) return;
    this.stopped = true;

    if (this.reconnectTimer !== null) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
    this.stopPolling();

    if (this.socket) {
      // Drop the handlers before closing so a queued onclose cannot schedule a
      // reconnect after teardown.
      this.socket.onopen = null;
      this.socket.onmessage = null;
      this.socket.onerror = null;
      this.socket.onclose = null;
      if (this.socket.readyState === WebSocket.OPEN || this.socket.readyState === WebSocket.CONNECTING) {
        this.socket.close(1000, 'popup closed');
      }
      this.socket = null;
    }

    this.abort.abort();
  }

  private wsUrl(): string {
    const origin = this.options.dashboard.replace(/\/$/, '');
    return origin.replace(/^http/, 'ws') + '/ws';
  }

  private connect(): void {
    if (this.stopped) return;
    let socket: WebSocket;
    try {
      socket = new WebSocket(this.wsUrl());
    } catch {
      this.scheduleReconnect();
      return;
    }
    this.socket = socket;

    socket.onopen = () => {
      if (this.stopped) return;
      this.delay = RECONNECT_MIN_MS;
      this.stopPolling();
      this.options.onStatus?.(true);
    };

    socket.onmessage = (event) => {
      if (this.stopped) return;
      try {
        const message = JSON.parse(String(event.data)) as { type?: string; jobs?: JobProgress[] };
        if (message.type === 'snapshot') this.publish(message.jobs ?? []);
      } catch {
        /* a malformed frame is not worth tearing the feed down for */
      }
    };

    socket.onerror = () => socket.close();

    socket.onclose = () => {
      if (this.stopped) return;
      this.options.onStatus?.(false);
      this.startPolling();
      this.scheduleReconnect();
    };
  }

  private scheduleReconnect(): void {
    if (this.stopped || this.reconnectTimer !== null) return;
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null;
      this.connect();
    }, this.delay);
    this.delay = Math.min(this.delay * 2, RECONNECT_MAX_MS);
  }

  /** Fallback for when the socket cannot be established at all. */
  private startPolling(): void {
    if (this.stopped || this.pollTimer !== null) return;
    const tick = async (): Promise<void> => {
      try {
        const res = await fetch(`${this.options.dashboard.replace(/\/$/, '')}/api/downloads`, {
          signal: this.abort.signal,
        });
        const data = (await res.json()) as { jobs?: JobProgress[] };
        this.publish(data.jobs ?? []);
        this.options.onStatus?.(true);
      } catch {
        this.options.onStatus?.(false);
      }
    };
    void tick();
    this.pollTimer = setInterval(() => void tick(), POLL_MS);
  }

  private stopPolling(): void {
    if (this.pollTimer !== null) {
      clearInterval(this.pollTimer);
      this.pollTimer = null;
    }
  }

  private publish(jobs: JobProgress[]): void {
    const map: ProgressMap = new Map();
    for (const job of jobs) {
      map.set(job.id, job);
      const key = normaliseUrl(job.url);
      // Keep the most interesting job when a URL was queued more than once:
      // an active download beats a finished or failed one.
      const existing = map.get(key);
      if (!existing || rank(job) > rank(existing)) map.set(key, job);
    }
    this.options.onUpdate(map);
  }
}

const ACTIVE = new Set(['downloading', 'merging', 'probing', 'queued']);

function rank(job: JobProgress): number {
  if (ACTIVE.has(job.state)) return 3;
  if (job.state === 'completed') return 2;
  return 1;
}
