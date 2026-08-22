/**
 * Tests for the sniffing filters.
 *
 * Run with: npm test   (node --test, no dependencies)
 *
 * These exercise the COMPILED output in dist/, so `npm run build` must have run
 * first — npm test does that for you.
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import {
  classify,
  cleanPageUrl,
  isExtractorOnlyHost,
  isNoiseUrl,
  isSiteHost,
  isWatchPageUrl,
  isYouTubeHost,
  sniffDecision,
} from '../dist/filters.js';

// ---------------------------------------------------------------------------
// The URLs that actually showed up in the popup and started all this
// ---------------------------------------------------------------------------

test('the reported YouTube garbage URLs are rejected', () => {
  const garbage = [
    'https://accounts.youtube.com/RotateCookiesPage?origin=https://www.youtube.com&yt_pid=1',
    'https://www.youtube.com/api/stats/atr?ns=yt&el=detailpage&cpn=abc123',
    'https://www.youtube.com/api/stats/watchtime?ns=yt&el=detailpage',
    'https://www.youtube.com/api/stats/qoe?fmt=243&cpn=xyz',
    'https://www.youtube.com/youtubei/v1/player?key=AIzaSy&prettyPrint=false',
    'https://www.youtube.com/youtubei/v1/log_event?alt=json',
    'https://www.youtube.com/generate_204',
    'https://www.youtube.com/error_204?t=jserror',
    'https://www.youtube.com/ptracking?html5=1&video_id=abc',
    'https://www.youtube.com/pagead/interaction/?ai=xyz',
    'https://accounts.google.com/ServiceLogin?continue=https://youtube.com',
    'https://play.google.com/log?format=json&hasfast=true',
  ];

  for (const url of garbage) {
    const decision = sniffDecision(url, 'application/json');
    assert.equal(decision.keep, false, `should have been rejected: ${url}`);
  }
});

test('YouTube media CDN URLs are rejected — they are single-use and IP-locked', () => {
  const cdn = [
    'https://rr3---sn-4g5ednsz.googlevideo.com/videoplayback?expire=1234&ei=abc&mime=video/mp4',
    'https://manifest.googlevideo.com/api/manifest/hls_variant/foo/index.m3u8',
  ];
  for (const url of cdn) {
    assert.equal(isExtractorOnlyHost(url), true, url);
    assert.equal(sniffDecision(url, 'video/mp4').keep, false, url);
  }
});

// ---------------------------------------------------------------------------
// Real media must still get through
// ---------------------------------------------------------------------------

test('genuine media is still captured', () => {
  const cases = [
    ['https://cdn.example.com/clip.mp4', 'video/mp4', 'direct', 'video'],
    ['https://cdn.example.com/photo.jpg', 'image/jpeg', 'direct', 'image'],
    ['https://cdn.example.com/live/master.m3u8', 'application/vnd.apple.mpegurl', 'hls', 'stream'],
    ['https://cdn.example.com/vod/manifest.mpd', 'application/dash+xml', 'dash', 'stream'],
    // A manifest whose URL gives nothing away — only the MIME type does.
    ['https://cdn.example.com/playlist', 'application/x-mpegURL', 'hls', 'stream'],
  ];
  for (const [url, mime, streamType, kind] of cases) {
    const d = sniffDecision(url, mime);
    assert.equal(d.keep, true, `should have been kept: ${url}`);
    assert.equal(d.streamType, streamType, url);
    assert.equal(d.kind, kind, url);
  }
});

test('stream segments are still rejected', () => {
  for (const url of [
    'https://cdn.example.com/seg00001.ts',
    'https://cdn.example.com/chunk_5.m4s',
    'https://cdn.example.com/sub.vtt',
  ]) {
    assert.equal(sniffDecision(url, 'video/mp2t').keep, false, url);
  }
});

test('a non-YouTube site keeps working normally', () => {
  // vimeo serves real progressive files; they must not be filtered as noise.
  const url = 'https://vod-progressive.akamaized.net/exp=123/video.mp4';
  assert.equal(isNoiseUrl(url), false);
  assert.equal(sniffDecision(url, 'video/mp4').keep, true);
});

// ---------------------------------------------------------------------------
// Watch pages vs. everything else on the same domain
// ---------------------------------------------------------------------------

test('only real watch pages count as site downloads', () => {
  const pages = [
    'https://www.youtube.com/watch?v=dQw4w9WgXcQ',
    'https://www.youtube.com/watch?v=dQw4w9WgXcQ&list=RDdQw4w9WgXcQ&index=1',
    'https://youtu.be/dQw4w9WgXcQ',
    'https://www.youtube.com/shorts/abc123XYZ',
    'https://www.youtube.com/live/abc123XYZ',
    'https://vimeo.com/123456789',
    'https://x.com/user/status/1234567890',
    'https://www.tiktok.com/@user/video/7300000000000000000',
    'https://www.instagram.com/reel/Cabc123/',
  ];
  for (const url of pages) {
    assert.equal(isWatchPageUrl(url), true, `should be a watch page: ${url}`);
    assert.equal(classify(url, 'text/html'), 'site', url);
  }

  // Same domains, but not a video page — this is the bug that was fixed.
  const notPages = [
    'https://www.youtube.com/',
    'https://www.youtube.com/feed/subscriptions',
    'https://www.youtube.com/results?search_query=cats',
    'https://www.youtube.com/@channel',
    'https://accounts.youtube.com/RotateCookiesPage?origin=https://www.youtube.com',
    'https://www.youtube.com/api/stats/atr',
    'https://x.com/home',
    'https://vimeo.com/',
  ];
  for (const url of notPages) {
    assert.equal(isWatchPageUrl(url), false, `should NOT be a watch page: ${url}`);
    assert.notEqual(classify(url, 'text/html'), 'site', url);
  }
});

test('site hosts are still recognised as extractor sites', () => {
  assert.equal(isSiteHost('https://www.youtube.com/anything'), true);
  assert.equal(isSiteHost('https://m.youtube.com/watch?v=x'), true);
  assert.equal(isSiteHost('https://example.com/clip.mp4'), false);
});

test('lookalike hostnames do not match', () => {
  // The classic suffix-matching bug: youtube.com.evil.test must not pass.
  assert.equal(isYouTubeHost('https://youtube.com.evil.test/watch?v=x'), false);
  assert.equal(isYouTubeHost('https://notyoutube.com/watch?v=x'), false);
  assert.equal(isYouTubeHost('https://www.youtube.com/watch?v=x'), true);
  assert.equal(isYouTubeHost('https://music.youtube.com/watch?v=x'), true);
  assert.equal(isYouTubeHost('https://youtu.be/x'), true);
});

// ---------------------------------------------------------------------------
// URL cleaning — the &list= problem
// ---------------------------------------------------------------------------

test('playlist and tracking parameters are stripped from YouTube URLs', () => {
  const cases = [
    // The exact shape that triggered "Playlists that require authentication".
    [
      'https://www.youtube.com/watch?v=dQw4w9WgXcQ&list=RDdQw4w9WgXcQ&start_radio=1&index=1',
      'https://www.youtube.com/watch?v=dQw4w9WgXcQ',
    ],
    [
      'https://www.youtube.com/watch?v=abc123&t=42s&pp=ygUJdGVzdA%3D%3D&si=xyz',
      'https://www.youtube.com/watch?v=abc123',
    ],
    ['https://youtu.be/abc123?list=PLxyz&t=10', 'https://www.youtube.com/watch?v=abc123'],
    ['https://m.youtube.com/watch?v=abc123&list=PL1', 'https://www.youtube.com/watch?v=abc123'],
    ['https://www.youtube.com/shorts/abc123?feature=share', 'https://www.youtube.com/shorts/abc123'],
    // Already clean: unchanged.
    ['https://www.youtube.com/watch?v=abc123', 'https://www.youtube.com/watch?v=abc123'],
  ];
  for (const [input, want] of cases) {
    assert.equal(cleanPageUrl(input), want, input);
  }
});

test('a deliberate playlist URL is left intact', () => {
  // No video id to fall back on — stripping list would destroy the URL.
  const url = 'https://www.youtube.com/playlist?list=PLabcdef';
  assert.equal(cleanPageUrl(url), url);
});

test('non-YouTube URLs are never rewritten', () => {
  for (const url of [
    'https://vimeo.com/123456?autoplay=1',
    'https://cdn.example.com/clip.mp4?token=keep-me&list=important',
    'not a url at all',
  ]) {
    assert.equal(cleanPageUrl(url), url);
  }
});
