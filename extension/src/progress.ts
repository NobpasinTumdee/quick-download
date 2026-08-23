/**
 * Matching engine jobs to the items the UI is showing.
 *
 * The socket itself now lives in the service worker (see engineSocket.ts) - a
 * popup is destroyed the moment it loses focus, which made it the wrong owner
 * for a connection that has to outlive it. What is left here is the pure part:
 * turning a snapshot into a lookup the popup can render from.
 */

import type { JobProgress } from './types.js';

/** Jobs keyed by their normalised URL, plus by job id. */
export type ProgressMap = Map<string, JobProgress>;

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

/** Builds the id/URL lookup the popup renders from. */
export function toProgressMap(jobs: readonly JobProgress[]): ProgressMap {
  const map: ProgressMap = new Map();
  for (const job of jobs) {
    map.set(job.id, job);
    const key = normaliseUrl(job.url);
    // Keep the most interesting job when a URL was queued more than once:
    // an active download beats a finished or failed one.
    const existing = map.get(key);
    if (!existing || rank(job) > rank(existing)) map.set(key, job);
  }
  return map;
}

const ACTIVE = new Set(['downloading', 'merging', 'probing', 'queued']);

function rank(job: JobProgress): number {
  if (ACTIVE.has(job.state)) return 3;
  if (job.state === 'completed') return 2;
  return 1;
}
