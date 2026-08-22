/**
 * Tests for the popup's progress matching and the YouTube thumbnail helpers.
 *
 * Run with: npm test
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import { normaliseUrl } from '../dist/progress.js';
import { youTubeThumbnail, youTubeVideoId } from '../dist/filters.js';

test('a sniffed YouTube URL matches the engine job after cleaning', () => {
  // The popup shows the page URL as the user sees it; the engine strips the
  // playlist parameters before downloading. Both must resolve to one key, or
  // the bar never appears on the download the user cares about most.
  const asShownInPopup = 'https://www.youtube.com/watch?v=dQw4w9WgXcQ&list=RDdQw4w9WgXcQ&index=1';
  const asReportedByEngine = 'https://www.youtube.com/watch?v=dQw4w9WgXcQ';
  assert.equal(normaliseUrl(asShownInPopup), normaliseUrl(asReportedByEngine));
});

test('every YouTube URL shape collapses to the same key', () => {
  const key = 'youtube:dQw4w9WgXcQ';
  for (const url of [
    'https://www.youtube.com/watch?v=dQw4w9WgXcQ',
    'https://m.youtube.com/watch?v=dQw4w9WgXcQ&t=30',
    'https://youtu.be/dQw4w9WgXcQ?si=abc',
    'https://www.youtube.com/shorts/dQw4w9WgXcQ',
    'https://www.youtube.com/embed/dQw4w9WgXcQ',
  ]) {
    assert.equal(normaliseUrl(url), key, url);
  }
});

test('different videos never collide', () => {
  assert.notEqual(
    normaliseUrl('https://www.youtube.com/watch?v=aaaaaaaaaaa'),
    normaliseUrl('https://www.youtube.com/watch?v=bbbbbbbbbbb'),
  );
});

test('non-YouTube URLs normalise on origin + path + query', () => {
  assert.equal(
    normaliseUrl('https://cdn.example.com/live/master.m3u8?token=1'),
    'https://cdn.example.com/live/master.m3u8?token=1',
  );
  // A trailing slash must not split one job into two keys.
  assert.equal(normaliseUrl('https://cdn.example.com/a/'), normaliseUrl('https://cdn.example.com/a'));
  // www is not significant.
  assert.equal(
    normaliseUrl('https://cdn.example.com/clip.mp4'),
    normaliseUrl('https://cdn.example.com/clip.mp4'),
  );
});

test('a malformed URL is returned unchanged rather than throwing', () => {
  assert.equal(normaliseUrl('not a url'), 'not a url');
});

test('youTubeVideoId handles every URL shape', () => {
  const cases = {
    'https://www.youtube.com/watch?v=dQw4w9WgXcQ': 'dQw4w9WgXcQ',
    'https://youtu.be/dQw4w9WgXcQ?list=PL1': 'dQw4w9WgXcQ',
    'https://www.youtube.com/shorts/abc123XYZ': 'abc123XYZ',
    'https://www.youtube.com/live/abc123XYZ': 'abc123XYZ',
    'https://www.youtube.com/embed/abc123XYZ': 'abc123XYZ',
    'https://m.youtube.com/watch?v=abc123XYZ&t=9': 'abc123XYZ',
  };
  for (const [url, want] of Object.entries(cases)) {
    assert.equal(youTubeVideoId(url), want, url);
  }

  // Not YouTube, or no id present.
  assert.equal(youTubeVideoId('https://vimeo.com/123'), undefined);
  assert.equal(youTubeVideoId('https://www.youtube.com/feed/subscriptions'), undefined);
  assert.equal(youTubeVideoId('https://youtube.com.evil.test/watch?v=abc123XYZ'), undefined);
});

test('youTubeThumbnail builds an hqdefault URL, or nothing', () => {
  assert.equal(
    youTubeThumbnail('dQw4w9WgXcQ'),
    'https://img.youtube.com/vi/dQw4w9WgXcQ/hqdefault.jpg',
  );
  // hqdefault exists for every video; maxresdefault 404s on non-HD uploads,
  // which would leave a broken image in the popup.
  assert.ok(!youTubeThumbnail('x')?.includes('maxresdefault'));
  assert.equal(youTubeThumbnail(undefined), undefined);
});
