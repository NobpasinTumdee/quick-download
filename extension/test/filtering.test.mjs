/**
 * Tests for the size floor, the smart domain filter and the cookie allowlist.
 *
 * Run with: npm test
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import { hostInList, isAdHost, parseDomainList, sniffDecision } from '../dist/filters.js';
import {
  DEFAULT_SETTINGS,
  FLOATING_BUTTON_MARGIN_MAX,
  FLOATING_BUTTON_MARGIN_MIN,
  FLOATING_BUTTON_POSITIONS,
} from '../dist/types.js';

const KB = 1024;

/** Settings as the popup ships them by default. */
const strict = {
  smartFilter: true,
  minBytes: 500 * KB,
  imageMinBytes: 100 * KB,
  captureImages: true,
  captureStreams: true,
};
const permissive = { ...strict, smartFilter: false };

// ---------------------------------------------------------------------------
// Defaults
// ---------------------------------------------------------------------------

test('defaults match the documented behaviour', () => {
  assert.deepEqual(DEFAULT_SETTINGS.cookieAllowlist, ['instagram.com', 'facebook.com']);
  assert.equal(DEFAULT_SETTINGS.minFileSizeKB, 500);
  assert.equal(DEFAULT_SETTINGS.smartFilter, true);
});

test('the floating button ships on, top right, 10px from the corner', () => {
  assert.equal(DEFAULT_SETTINGS.floatingButtonEnabled, true);
  assert.equal(DEFAULT_SETTINGS.floatingButtonPosition, 'top-right');
  assert.equal(DEFAULT_SETTINGS.floatingButtonMargin, 10);
});

test('every corner the popup offers is one the content script accepts', () => {
  // The <select> in popup.html and the whitelist in content.ts are written out
  // separately - the content script cannot import, so it re-declares them.
  // A corner in one list and not the other silently does nothing.
  const offered = FLOATING_BUTTON_POSITIONS.map((p) => p.value).sort();
  assert.deepEqual(offered, ['bottom-left', 'bottom-right', 'top-left', 'top-right']);
  assert.ok(offered.includes(DEFAULT_SETTINGS.floatingButtonPosition));
});

test('the margin bounds leave the button on the video', () => {
  assert.equal(FLOATING_BUTTON_MARGIN_MIN, 0);
  assert.ok(FLOATING_BUTTON_MARGIN_MAX <= 200, 'a larger margin can push it off a small player');
  assert.ok(DEFAULT_SETTINGS.floatingButtonMargin >= FLOATING_BUTTON_MARGIN_MIN);
  assert.ok(DEFAULT_SETTINGS.floatingButtonMargin <= FLOATING_BUTTON_MARGIN_MAX);
});

// ---------------------------------------------------------------------------
// Size floor
// ---------------------------------------------------------------------------

test('media below the size floor is ignored', () => {
  const small = sniffDecision('https://cdn.example.com/clip.mp4', 'video/mp4', 200 * KB, strict);
  assert.equal(small.keep, false);
  assert.match(small.reason, /500 KB/);

  const big = sniffDecision('https://cdn.example.com/clip.mp4', 'video/mp4', 900 * KB, strict);
  assert.equal(big.keep, true);
});

test('an unknown size is not treated as a small file', () => {
  // Chunked responses carry no Content-Length. Dropping every unsized video
  // would lose exactly the long ones worth keeping.
  const d = sniffDecision('https://cdn.example.com/clip.mp4', 'video/mp4', 0, strict);
  assert.equal(d.keep, true, d.reason);
});

test('images of unknown size are rejected', () => {
  // An image that will not declare its size is almost always a tracking pixel.
  const d = sniffDecision('https://cdn.example.com/pic.jpg', 'image/jpeg', 0, strict);
  assert.equal(d.keep, false);
});

test('the image floor stacks on top of the global floor', () => {
  const opts = { ...strict, minBytes: 50 * KB, imageMinBytes: 300 * KB };
  assert.equal(sniffDecision('https://x.test/a.jpg', 'image/jpeg', 100 * KB, opts).keep, false);
  assert.equal(sniffDecision('https://x.test/a.jpg', 'image/jpeg', 400 * KB, opts).keep, true);
  // The stricter of the two wins, whichever way round they are set.
  const flipped = { ...strict, minBytes: 300 * KB, imageMinBytes: 50 * KB };
  assert.equal(sniffDecision('https://x.test/a.jpg', 'image/jpeg', 100 * KB, flipped).keep, false);
});

test('the size floor never applies to streaming manifests', () => {
  // A manifest is a few hundred bytes of text standing for a whole movie.
  // Applying a file-size floor to it would filter out every stream there is.
  for (const [url, mime] of [
    ['https://cdn.example.com/live/master.m3u8', 'application/vnd.apple.mpegurl'],
    ['https://cdn.example.com/vod/manifest.mpd', 'application/dash+xml'],
  ]) {
    const d = sniffDecision(url, mime, 900, strict);
    assert.equal(d.keep, true, `${url}: ${d.reason}`);
    assert.equal(d.kind, 'stream');
  }
});

// ---------------------------------------------------------------------------
// Smart domain filter
// ---------------------------------------------------------------------------

test('smart filter rejects ad and tracking networks', () => {
  const ads = [
    'https://securepubads.g.doubleclick.net/video.mp4',
    'https://cdn.taboola.com/promo.mp4',
    'https://x.adnxs.com/creative.mp4',
    'https://ads.pubmatic.com/spot.mp4',
  ];
  for (const url of ads) {
    assert.equal(isAdHost(url), true, url);
    assert.equal(sniffDecision(url, 'video/mp4', 5e6, strict).keep, false, url);
  }
});

test('smart filter OFF lets those domains through but keeps the size floor', () => {
  const url = 'https://cdn.taboola.com/promo.mp4';
  assert.equal(sniffDecision(url, 'video/mp4', 5e6, permissive).keep, true);
  // Still filtered when it is small - the size rule is always active.
  const small = sniffDecision(url, 'video/mp4', 50 * KB, permissive);
  assert.equal(small.keep, false);
  assert.match(small.reason, /KB floor/);
});

test('smart filter OFF still rejects non-media and segments', () => {
  // Structural rules, not a matter of taste: telemetry answers with JSON and a
  // lone segment cannot be downloaded on its own.
  assert.equal(sniffDecision('https://www.youtube.com/api/stats/atr', 'application/json', 900, permissive).keep, false);
  assert.equal(sniffDecision('https://cdn.example.com/seg001.ts', 'video/mp2t', 9e6, permissive).keep, false);
});

test('Instagram and Facebook CDNs are never blanket-blocked', () => {
  // Blocking them would break the very downloads the cookie allowlist enables.
  // Small background fragments are handled by the size floor instead.
  const url = 'https://scontent.cdninstagram.com/v/t50.2886-16/reel.mp4';
  assert.equal(sniffDecision(url, 'video/mp4', 4e6, strict).keep, true);
  assert.equal(sniffDecision(url, 'video/mp4', 60 * KB, strict).keep, false);
});

// ---------------------------------------------------------------------------
// Cookie allowlist
// ---------------------------------------------------------------------------

test('the allowlist matches a domain and its subdomains, not lookalikes', () => {
  const list = ['instagram.com', 'facebook.com'];
  assert.equal(hostInList('https://www.instagram.com/reel/abc/', list), true);
  assert.equal(hostInList('https://scontent.cdninstagram.com/x.mp4', list), false);
  assert.equal(hostInList('https://web.facebook.com/watch/', list), true);
  assert.equal(hostInList('https://www.youtube.com/watch?v=x', list), false);
  // The classic suffix bug.
  assert.equal(hostInList('https://instagram.com.evil.test/', list), false);
  assert.equal(hostInList('https://notinstagram.com/', list), false);
});

test('an empty allowlist allows nothing', () => {
  assert.equal(hostInList('https://www.instagram.com/', []), false);
});

test('the domain list is normalised as the user types it', () => {
  const parsed = parseDomainList(`
    https://www.Instagram.com/
    facebook.com, x.com
    *.example.org
    garbage
  `);
  assert.deepEqual(parsed, ['instagram.com', 'facebook.com', 'x.com', '*.example.org']);
  // A wildcard entry still matches through hostInList.
  assert.equal(hostInList('https://cdn.example.org/a.mp4', ['*.example.org']), true);
});
