/**
 * Tests for the service worker's engine feed: what keeps the socket (and
 * therefore the worker) alive, what lets it go, and what counts as news.
 *
 * Run with: npm test
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import {
  EngineFeed,
  IDLE_LINGER_MS,
  hasActiveJobs,
  snapshotStates,
  terminalTransitions,
} from '../dist/engineSocket.js';
import { toProgressMap } from '../dist/progress.js';

const job = (over = {}) => ({
  id: 'j1',
  url: 'https://example.com/a.mp4',
  filename: 'a.mp4',
  state: 'downloading',
  progress: 10,
  speed: 0,
  eta: 0,
  size: 0,
  downloaded: 0,
  engine: 'http',
  kind: 'direct',
  ...over,
});

// ---------------------------------------------------------------------------
// What counts as news
// ---------------------------------------------------------------------------

test('only the transition into a terminal state is announced', () => {
  const before = snapshotStates([job({ state: 'downloading' })]);
  const done = [job({ state: 'completed' })];

  assert.equal(terminalTransitions(before, done).length, 1);
  // The engine rebroadcasts a finished job several times a second; the second
  // look at the same state must produce nothing.
  assert.equal(terminalTransitions(snapshotStates(done), done).length, 0);
});

test('a job first seen already finished is history, not news', () => {
  // The worker was asleep, or the popup connected after the fact. Announcing
  // it now would fire a notification for a download from an hour ago.
  assert.deepEqual(terminalTransitions(new Map(), [job({ state: 'completed' })]), []);
});

test('failure is announced as well as completion', () => {
  const before = snapshotStates([job({ state: 'downloading' })]);
  const failed = terminalTransitions(before, [job({ state: 'failed', error: 'boom' })]);
  assert.equal(failed.length, 1);
  assert.equal(failed[0].error, 'boom');
});

test('hasActiveJobs decides whether the engine is still working', () => {
  assert.equal(hasActiveJobs([job({ state: 'completed' })]), false);
  assert.equal(hasActiveJobs([job({ state: 'completed' }), job({ id: 'j2', state: 'queued' })]), true);
  assert.equal(hasActiveJobs([]), false);
});

// ---------------------------------------------------------------------------
// The map the popup renders from
// ---------------------------------------------------------------------------

test('a job is findable by id and by URL', () => {
  const map = toProgressMap([job({ id: 'abc', url: 'https://example.com/clip.mp4' })]);
  assert.equal(map.get('abc').id, 'abc');
  assert.equal(map.get('https://example.com/clip.mp4').id, 'abc');
});

test('an active job wins over a finished one for the same URL', () => {
  // Re-downloading something already in the list must show the live bar, not
  // the stale "completed" from last time.
  const map = toProgressMap([
    job({ id: 'old', state: 'completed' }),
    job({ id: 'new', state: 'downloading' }),
  ]);
  assert.equal(map.get('https://example.com/a.mp4').id, 'new');
});

// ---------------------------------------------------------------------------
// Connection lifetime
// ---------------------------------------------------------------------------

/** A controllable clock: timers fire only when the test says so. */
function fakeTimers() {
  let next = 1;
  const pending = new Map();
  return {
    set: (fn, ms) => {
      const id = next++;
      pending.set(id, { fn, at: ms });
      return id;
    },
    clear: (id) => pending.delete(id),
    /** Runs every timer scheduled for <= ms, oldest first. */
    advance(ms) {
      for (const [id, entry] of [...pending]) {
        if (entry.at <= ms) {
          pending.delete(id);
          entry.fn();
        }
      }
    },
    get size() {
      return pending.size;
    },
  };
}

function fakeSocket() {
  const sent = [];
  const socket = {
    onopen: null,
    onmessage: null,
    onerror: null,
    onclose: null,
    readyState: 0,
    closed: false,
    send: (data) => sent.push(data),
    close() {
      this.closed = true;
      this.onclose?.();
    },
    sent,
  };
  return socket;
}

function harness() {
  const timers = fakeTimers();
  const sockets = [];
  const snapshots = [];
  const status = [];
  const feed = new EngineFeed({
    origin: 'http://127.0.0.1:9090',
    onSnapshot: (jobs) => snapshots.push(jobs),
    onStatus: (up) => status.push(up),
    createSocket: (url) => {
      const socket = fakeSocket();
      socket.url = url;
      sockets.push(socket);
      return socket;
    },
    setTimer: timers.set,
    clearTimer: timers.clear,
  });
  return { feed, timers, sockets, snapshots, status };
}

test('the ws:// URL is derived from the engine origin', () => {
  const { feed, sockets } = harness();
  feed.hold('ui');
  assert.equal(sockets[0].url, 'ws://127.0.0.1:9090/ws');
});

test('a hold opens the socket and snapshots flow', () => {
  const { feed, sockets, snapshots, status } = harness();
  feed.hold('ui');
  assert.equal(sockets.length, 1);

  sockets[0].onopen();
  assert.deepEqual(status, [true]);
  assert.equal(feed.isConnected, true);

  sockets[0].onmessage({ data: JSON.stringify({ type: 'snapshot', jobs: [job()] }) });
  assert.equal(snapshots.length, 1);
  assert.equal(snapshots[0][0].id, 'j1');
});

test('a malformed frame does not tear the feed down', () => {
  const { feed, sockets, snapshots } = harness();
  feed.hold('ui');
  sockets[0].onopen();
  sockets[0].onmessage({ data: 'not json at all' });
  sockets[0].onmessage({ data: JSON.stringify({ type: 'pong' }) });
  assert.equal(snapshots.length, 0);
  assert.equal(feed.isConnected, true, 'still up');
});

test('holds are counted, not just set: the last one out closes the socket', () => {
  const { feed, sockets, timers } = harness();
  feed.hold('ui');
  feed.hold('jobs');
  sockets[0].onopen();

  feed.release('ui');
  timers.advance(IDLE_LINGER_MS);
  assert.equal(sockets[0].closed, false, 'a download is still running');

  feed.release('jobs');
  timers.advance(IDLE_LINGER_MS);
  assert.equal(sockets[0].closed, true);
  assert.deepEqual(feed.reasons, []);
});

test('the socket lingers briefly rather than closing on the last release', () => {
  // Downloads arrive in bursts. Tearing the connection down to rebuild it a
  // second later costs more than holding it for a few seconds.
  const { feed, sockets, timers } = harness();
  feed.hold('jobs');
  sockets[0].onopen();
  feed.release('jobs');

  timers.advance(IDLE_LINGER_MS - 1);
  assert.equal(sockets[0].closed, false);

  feed.hold('ui'); // something wants it again before the linger expires
  timers.advance(IDLE_LINGER_MS);
  assert.equal(sockets[0].closed, false, 'the linger must have been cancelled');
});

test('an unexpected close reconnects while something still holds the feed', () => {
  const { feed, sockets, timers, status } = harness();
  feed.hold('jobs');
  sockets[0].onopen();

  sockets[0].onclose(); // the engine restarted
  assert.deepEqual(status, [true, false]);

  timers.advance(10_000);
  assert.equal(sockets.length, 2, 'a new socket was opened');
});

test('a close with no holds left is not chased', () => {
  const { feed, sockets, timers } = harness();
  feed.hold('ui');
  sockets[0].onopen();
  feed.release('ui');
  timers.advance(IDLE_LINGER_MS);

  timers.advance(60_000);
  assert.equal(sockets.length, 1, 'nothing wanted it, so nothing reconnected');
});

test('close() is idempotent and silences a queued onclose', () => {
  const { feed, sockets, timers } = harness();
  feed.hold('jobs');
  sockets[0].onopen();

  feed.close();
  feed.close();
  assert.equal(sockets[0].closed, true);
  assert.equal(sockets[0].onclose, null, 'handlers are dropped before closing');

  timers.advance(60_000);
  assert.equal(sockets.length, 1, 'no reconnect was scheduled');
});

test('the feed sends a keepalive so a quiet job cannot let the worker sleep', () => {
  // Chrome keeps the worker alive on WebSocket traffic. yt-dlp can spend half a
  // minute probing a site without a single progress line, which is exactly long
  // enough for the 30s idle timeout to fire.
  const { feed, sockets, timers } = harness();
  feed.hold('jobs');
  sockets[0].onopen();

  timers.advance(20_000);
  assert.deepEqual(sockets[0].sent, ['ping']);

  timers.advance(20_000);
  assert.equal(sockets[0].sent.length, 2, 'it keeps going while the socket is up');
});

test('the keepalive stops with the socket', () => {
  const { feed, sockets, timers } = harness();
  feed.hold('jobs');
  sockets[0].onopen();
  feed.close();

  timers.advance(60_000);
  assert.deepEqual(sockets[0].sent, [], 'a closed socket is not written to');
});

test('opening twice does not open twice', () => {
  const { feed, sockets } = harness();
  feed.hold('ui');
  feed.hold('jobs');
  feed.connect();
  assert.equal(sockets.length, 1);
});
