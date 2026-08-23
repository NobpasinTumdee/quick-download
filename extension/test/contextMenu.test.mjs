/**
 * Tests for the right-click integration: what each menu item means, and what
 * the user is told when there is no popup open to tell them.
 *
 * Run with: npm test
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import {
  MENU_DASHBOARD,
  MENU_IMAGE,
  MENU_ITEMS,
  MENU_LINK,
  MENU_MEDIA,
  MENU_PAGE,
  MENU_PARENT,
  engineHint,
  handleContextClick,
  labelFor,
  resolveContextTarget,
} from '../dist/contextMenu.js';

// ---------------------------------------------------------------------------
// The menu itself
// ---------------------------------------------------------------------------

const byId = (id) => MENU_ITEMS.find((item) => item.id === id);

test('the menu is one parent with the documented children', () => {
  const parent = byId(MENU_PARENT);
  assert.equal(parent.title, 'Quick Download');
  assert.equal(parent.parentId, undefined);

  const expected = [
    [MENU_LINK, 'Download this Link', ['link']],
    [MENU_MEDIA, 'Download this Media', ['video', 'audio']],
    [MENU_IMAGE, 'Download this Image', ['image']],
  ];
  for (const [id, title, contexts] of expected) {
    const item = byId(id);
    assert.equal(item.title, title, id);
    assert.equal(item.parentId, MENU_PARENT, id);
    assert.deepEqual(item.contexts, contexts, id);
  }

  const page = byId(MENU_PAGE);
  assert.equal(page.title, 'Download Current Page');
  assert.ok(page.contexts.includes('page'));
});

test('a parent only appears where a child could', () => {
  // Chrome hides a parent whose contexts do not cover the click, so a missing
  // context here means a whole menu item silently never shows up.
  const parent = byId(MENU_PARENT);
  for (const child of MENU_ITEMS.filter((i) => i.parentId === MENU_PARENT)) {
    for (const ctx of child.contexts) {
      assert.ok(parent.contexts.includes(ctx), `parent is missing the ${ctx} context`);
    }
  }
});

test('menu ids are unique', () => {
  const ids = MENU_ITEMS.map((i) => i.id);
  assert.equal(new Set(ids).size, ids.length, 'a duplicate id makes create() fail');
});

test('the menu is offered only on downloadable pages and targets', () => {
  assert.deepEqual(byId(MENU_PARENT).documentUrlPatterns, ['http://*/*', 'https://*/*']);
  // blob:/data: media cannot be fetched by a process outside the browser.
  for (const id of [MENU_LINK, MENU_MEDIA, MENU_IMAGE]) {
    assert.deepEqual(byId(id).targetUrlPatterns, ['http://*/*', 'https://*/*'], id);
  }
});

// ---------------------------------------------------------------------------
// What a click resolves to
// ---------------------------------------------------------------------------

test('each item takes its URL from its own context', () => {
  // A <video> inside an <a> offers both; the item clicked says which was meant.
  const info = {
    menuItemId: MENU_MEDIA,
    srcUrl: 'https://cdn.example.com/clip.mp4',
    linkUrl: 'https://example.com/watch/123',
    pageUrl: 'https://example.com/watch/123',
  };
  assert.equal(resolveContextTarget(info).url, 'https://cdn.example.com/clip.mp4');
  assert.equal(
    resolveContextTarget({ ...info, menuItemId: MENU_LINK }).url,
    'https://example.com/watch/123',
  );
});

test('a thumbnail linking to the file still resolves', () => {
  const target = resolveContextTarget({
    menuItemId: MENU_IMAGE,
    linkUrl: 'https://example.com/full.jpg',
  });
  assert.equal(target.url, 'https://example.com/full.jpg');
});

test('the page item hands the page to the extractor', () => {
  const target = resolveContextTarget(
    { menuItemId: MENU_PAGE, pageUrl: 'https://www.youtube.com/watch?v=dQw4w9WgXcQ&list=RDx' },
    { url: 'https://www.youtube.com/watch?v=dQw4w9WgXcQ&list=RDx', title: 'Never Gonna Give You Up' },
  );
  assert.equal(target.kind, 'site', 'without this it goes to the chunk engine and fails');
  // The playlist parameter is stripped before the engine ever sees it.
  assert.equal(target.url, 'https://www.youtube.com/watch?v=dQw4w9WgXcQ');
  assert.equal(target.label, 'Never Gonna Give You Up');
});

test('inside a frame, the frame is the page the user means', () => {
  // The whole point of right-clicking an embedded player.
  const target = resolveContextTarget({
    menuItemId: MENU_PAGE,
    pageUrl: 'https://blog.example.com/post/1',
    frameUrl: 'https://www.youtube.com/embed/dQw4w9WgXcQ',
  });
  assert.equal(target.url, 'https://www.youtube.com/embed/dQw4w9WgXcQ');
  assert.equal(target.kind, 'site');
});

test('a frame URL equal to the page is not treated as a frame', () => {
  const target = resolveContextTarget(
    { menuItemId: MENU_PAGE, pageUrl: 'https://example.com/a', frameUrl: 'https://example.com/a' },
    { url: 'https://example.com/a' },
  );
  assert.equal(target.url, 'https://example.com/a');
});

test('URLs the engine cannot fetch are refused', () => {
  for (const url of ['blob:https://example.com/8a7f', 'data:video/mp4;base64,AAAA', 'about:blank']) {
    assert.equal(resolveContextTarget({ menuItemId: MENU_MEDIA, srcUrl: url }), null, url);
  }
  assert.equal(resolveContextTarget({ menuItemId: MENU_LINK }), null, 'no link URL at all');
});

test('an unknown menu id resolves to nothing', () => {
  assert.equal(resolveContextTarget({ menuItemId: 'something-else', srcUrl: 'https://x/a.mp4' }), null);
});

test('labelFor names the file, not the URL', () => {
  assert.equal(labelFor('https://cdn.example.com/media/My%20Clip.mp4'), 'My Clip.mp4');
  assert.equal(labelFor('https://example.com/'), 'example.com');
});

// ---------------------------------------------------------------------------
// Feedback
// ---------------------------------------------------------------------------

/** Records the notifications a click produced, in order. */
function stubChrome() {
  const created = [];
  const cleared = [];
  globalThis.chrome = {
    runtime: { getURL: (p) => `chrome-extension://test/${p}` },
    notifications: {
      create: (id, opts) => created.push({ id, ...opts }),
      clear: (id) => cleared.push(id),
    },
  };
  return { created, cleared };
}

const deps = (over = {}) => ({
  startDownload: async () => ({ ok: true, type: 'download' }),
  openDashboard: async () => ({ ok: true, type: 'open_dashboard' }),
  notificationsEnabled: async () => true,
  ...over,
});

test('a queued download tells the user immediately', async () => {
  const { created } = stubChrome();
  const calls = [];

  await handleContextClick(
    { menuItemId: MENU_MEDIA, srcUrl: 'https://cdn.example.com/Great%20Clip.mp4' },
    { url: 'https://example.com/watch', title: 'Watch' },
    deps({
      startDownload: async (url, opts) => {
        // The notification must already be up: the native round trip can take
        // seconds if it has to wake the daemon.
        assert.equal(created.length, 1, 'feedback waited for the engine');
        calls.push([url, opts]);
        return { ok: true, type: 'download' };
      },
    }),
  );

  assert.deepEqual(calls, [[
    'https://cdn.example.com/Great%20Clip.mp4',
    { pageUrl: 'https://example.com/watch', kind: undefined, title: 'Watch' },
  ]]);
  assert.equal(created[0].title, 'Download queued');
  assert.equal(created[0].message, 'Great Clip.mp4');
});

test('a sleeping or missing engine says so, in place', async () => {
  const { created } = stubChrome();
  await handleContextClick(
    { menuItemId: MENU_LINK, linkUrl: 'https://example.com/a.mp4' },
    undefined,
    deps({
      startDownload: async () => ({
        ok: false,
        type: 'download',
        error: 'Specified native messaging host not found.',
      }),
    }),
  );

  assert.equal(created.length, 2, 'the queued card must be corrected');
  // Same id: the second replaces the first rather than stacking on top of it.
  assert.equal(created[1].id, created[0].id);
  assert.match(created[1].title, /could not start/i);
  assert.match(created[1].message, /installed/i);
});

test('notifications can be turned off without breaking the download', async () => {
  const { created } = stubChrome();
  let started = 0;
  await handleContextClick(
    { menuItemId: MENU_LINK, linkUrl: 'https://example.com/a.mp4' },
    undefined,
    deps({
      notificationsEnabled: async () => false,
      startDownload: async () => {
        started += 1;
        return { ok: false, type: 'download', error: 'nope' };
      },
    }),
  );
  assert.equal(started, 1);
  assert.equal(created.length, 0);
});

test('the dashboard item does not enqueue anything', async () => {
  stubChrome();
  let started = 0;
  let opened = 0;
  await handleContextClick({ menuItemId: MENU_DASHBOARD }, undefined, deps({
    startDownload: async () => { started += 1; return { ok: true, type: 'download' }; },
    openDashboard: async () => { opened += 1; return { ok: true, type: 'open_dashboard' }; },
  }));
  assert.equal(opened, 1);
  assert.equal(started, 0);
});

test('a click with no usable URL is dropped quietly', async () => {
  const { created } = stubChrome();
  let started = 0;
  await handleContextClick({ menuItemId: MENU_MEDIA, srcUrl: 'blob:https://x/1' }, undefined,
    deps({ startDownload: async () => { started += 1; return { ok: true, type: 'download' }; } }));
  assert.equal(started, 0);
  assert.equal(created.length, 0, 'nothing was queued, so nothing to announce');
});

test('engineHint turns native-messaging noise into an instruction', () => {
  assert.match(engineHint('Specified native messaging host not found.'), /native host is registered/i);
  assert.match(engineHint(undefined), /installed/i);
  // A real engine error is worth showing verbatim.
  assert.equal(engineHint('yt-dlp is required for site downloads'), 'yt-dlp is required for site downloads');
});
