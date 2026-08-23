/**
 * The engine feed, owned by the service worker.
 *
 * This used to live in the popup, which meant progress and completion
 * notifications only existed while a popup happened to be open - the one moment
 * the user is already looking at the download. Since Chrome 116 a service
 * worker stays alive while its WebSocket carries traffic, so the socket can
 * live here instead and the UI becomes a subscriber rather than the owner.
 *
 * That keep-alive is also the reason this file is careful about *closing*. A
 * socket the worker holds open forever pins the worker forever, which is a
 * battery cost on a browser that may sit idle for hours. So the connection is
 * demand-driven: it stays up while a download is running or a popup is
 * watching, and drops shortly after both are gone.
 */

import type { JobProgress } from './types.js';

/** The slice of WebSocket we use, so tests can supply a fake. */
export interface SocketLike {
  onopen: (() => void) | null;
  onmessage: ((event: { data: unknown }) => void) | null;
  onerror: (() => void) | null;
  onclose: (() => void) | null;
  readyState: number;
  send(data: string): void;
  close(code?: number, reason?: string): void;
}

export interface EngineFeedOptions {
  /** Engine origin, e.g. "http://127.0.0.1:9090". */
  origin: string;
  /** A full snapshot arrived. */
  onSnapshot: (jobs: JobProgress[]) => void;
  /** Transport came up or went down. */
  onStatus?: (connected: boolean) => void;
  /** Injectable for tests; defaults to the real WebSocket. */
  createSocket?: (url: string) => SocketLike;
  setTimer?: (fn: () => void, ms: number) => number;
  clearTimer?: (handle: number) => void;
}

const RECONNECT_MIN_MS = 700;
const RECONNECT_MAX_MS = 10_000;

/**
 * How often we send a byte of our own.
 *
 * Comfortably under the worker's 30-second idle timeout, and under the engine's
 * own 30-second ping, so a job that is working without reporting (yt-dlp
 * probing a site, say) cannot let the worker fall asleep mid-download. The
 * engine ignores whatever the client sends, so the content does not matter -
 * only that the socket is doing something.
 */
const KEEPALIVE_MS = 20_000;

/** How long the socket lingers after the last reason to hold it open. */
export const IDLE_LINGER_MS = 25_000;

/** States that mean the engine is still working on a job. */
export const ACTIVE_STATES: ReadonlySet<string> = new Set([
  'queued',
  'probing',
  'downloading',
  'merging',
]);

export const TERMINAL_STATES: ReadonlySet<string> = new Set(['completed', 'failed', 'canceled']);

export function hasActiveJobs(jobs: readonly JobProgress[]): boolean {
  return jobs.some((job) => ACTIVE_STATES.has(job.state));
}

/**
 * Finds the jobs that have just reached a terminal state.
 *
 * The engine rebroadcasts every job it still has in its list, several times a
 * second, so "state === completed" is true for minutes on end. Only the
 * transition is news, which is why the previous state is compared rather than
 * the current one being trusted.
 */
export function terminalTransitions(
  previous: ReadonlyMap<string, string>,
  jobs: readonly JobProgress[],
): JobProgress[] {
  const fresh: JobProgress[] = [];
  for (const job of jobs) {
    if (!TERMINAL_STATES.has(job.state)) continue;
    const before = previous.get(job.id);
    // An unseen job that is already finished is not news either: it was
    // completed before this worker ever started, or before we connected.
    if (before === undefined || before === job.state) continue;
    fresh.push(job);
  }
  return fresh;
}

/** Records the current state of every job, for the next comparison. */
export function snapshotStates(jobs: readonly JobProgress[]): Map<string, string> {
  const map = new Map<string, string>();
  for (const job of jobs) map.set(job.id, job.state);
  return map;
}

export class EngineFeed {
  private socket: SocketLike | null = null;
  private reconnect: number | null = null;
  private keepalive: number | null = null;
  private linger: number | null = null;
  private delay = RECONNECT_MIN_MS;
  private connected = false;

  /** Reasons to stay connected. The socket lives while this is non-empty. */
  private readonly holds = new Set<string>();

  private readonly createSocket: (url: string) => SocketLike;
  private readonly setTimer: (fn: () => void, ms: number) => number;
  private readonly clearTimer: (handle: number) => void;

  constructor(private readonly options: EngineFeedOptions) {
    this.createSocket =
      options.createSocket ?? ((url) => new WebSocket(url) as unknown as SocketLike);
    this.setTimer = options.setTimer ?? ((fn, ms) => setTimeout(fn, ms) as unknown as number);
    this.clearTimer = options.clearTimer ?? ((handle) => clearTimeout(handle));
  }

  get isConnected(): boolean {
    return this.connected;
  }

  /** Reasons currently holding the socket open. Exposed for tests and logging. */
  get reasons(): string[] {
    return [...this.holds];
  }

  /**
   * Declares a reason to keep the feed up: 'ui' while a popup is watching,
   * 'jobs' while the engine is working, 'enqueue' for the moment between
   * queuing a download and the engine reporting it.
   */
  hold(reason: string): void {
    this.holds.add(reason);
    this.cancelLinger();
    this.connect();
  }

  /** Drops a reason. The socket closes once the last one is gone. */
  release(reason: string): void {
    if (!this.holds.delete(reason)) return;
    if (this.holds.size === 0) this.startLinger();
  }

  /** Opens the socket if it is not already open or opening. */
  connect(): void {
    if (this.socket) return;

    let socket: SocketLike;
    try {
      socket = this.createSocket(this.wsUrl());
    } catch {
      this.scheduleReconnect();
      return;
    }
    this.socket = socket;

    socket.onopen = () => {
      this.delay = RECONNECT_MIN_MS;
      this.connected = true;
      this.options.onStatus?.(true);
      this.startKeepalive();
    };

    socket.onmessage = (event) => {
      try {
        const message = JSON.parse(String(event.data)) as { type?: string; jobs?: JobProgress[] };
        if (message.type === 'snapshot') this.options.onSnapshot(message.jobs ?? []);
      } catch {
        /* one malformed frame is not worth tearing the feed down for */
      }
    };

    socket.onerror = () => socket.close();

    socket.onclose = () => {
      this.stopKeepalive();
      const wasConnected = this.connected;
      this.connected = false;
      this.socket = null;
      if (wasConnected) this.options.onStatus?.(false);
      // Only chase a socket somebody still wants.
      if (this.holds.size > 0) this.scheduleReconnect();
    };
  }

  /** Idempotent teardown: socket, timers, the lot. */
  close(): void {
    this.cancelReconnect();
    this.cancelLinger();
    this.stopKeepalive();
    const socket = this.socket;
    this.socket = null;
    this.connected = false;
    if (!socket) return;
    // Drop the handlers first: a queued onclose must not schedule a reconnect
    // after we have decided to stop.
    socket.onopen = null;
    socket.onmessage = null;
    socket.onerror = null;
    socket.onclose = null;
    try {
      socket.close(1000, 'idle');
    } catch {
      /* already closing */
    }
  }

  private wsUrl(): string {
    return this.options.origin.replace(/\/$/, '').replace(/^http/, 'ws') + '/ws';
  }

  private scheduleReconnect(): void {
    if (this.reconnect !== null) return;
    this.reconnect = this.setTimer(() => {
      this.reconnect = null;
      if (this.holds.size > 0) this.connect();
    }, this.delay);
    this.delay = Math.min(this.delay * 2, RECONNECT_MAX_MS);
  }

  private cancelReconnect(): void {
    if (this.reconnect !== null) {
      this.clearTimer(this.reconnect);
      this.reconnect = null;
    }
  }

  private startKeepalive(): void {
    this.stopKeepalive();
    const tick = (): void => {
      if (!this.socket || !this.connected) return;
      try {
        this.socket.send('ping');
      } catch {
        /* the close handler will deal with it */
      }
      this.keepalive = this.setTimer(tick, KEEPALIVE_MS);
    };
    this.keepalive = this.setTimer(tick, KEEPALIVE_MS);
  }

  private stopKeepalive(): void {
    if (this.keepalive !== null) {
      this.clearTimer(this.keepalive);
      this.keepalive = null;
    }
  }

  /**
   * Nothing wants the socket any more, but downloads arrive in bursts: the
   * linger avoids tearing a connection down only to rebuild it a second later.
   */
  private startLinger(): void {
    this.cancelLinger();
    this.linger = this.setTimer(() => {
      this.linger = null;
      if (this.holds.size === 0) this.close();
    }, IDLE_LINGER_MS);
  }

  private cancelLinger(): void {
    if (this.linger !== null) {
      this.clearTimer(this.linger);
      this.linger = null;
    }
  }
}
