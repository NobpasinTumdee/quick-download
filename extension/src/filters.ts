/**
 * URL filtering and classification.
 *
 * Split out of background.ts so the rules are unit-testable: this is where the
 * difference between "a video you can download" and "the 200 telemetry beacons
 * a video page fires" is decided, and getting it wrong fills the popup with
 * junk.
 *
 * Pure functions only — no chrome.* access — so `node --test` can exercise them.
 */

import type { MediaKind, StreamType } from './types.js';

/** Streaming manifests — the handle to an entire adaptive stream. */
export const HLS_EXT_RE = /\.(m3u8|m3u)(\?|#|$)/i;
export const DASH_EXT_RE = /\.mpd(\?|#|$)/i;

export const MEDIA_EXT_RE =
  /\.(mp4|webm|m4v|mov|mkv|avi|flv|m4a|mp3|aac|ogg|opus|wav|flac)(\?|#|$)/i;
export const IMAGE_EXT_RE = /\.(jpe?g|png|gif|webp|avif|bmp|svg|tiff?)(\?|#|$)/i;

/**
 * Individual stream segments. A one-hour HLS video is ~900 of these; putting
 * them in the list would be useless and would blow the storage quota.
 */
export const SEGMENT_EXT_RE = /\.(ts|m4s|cmfv|cmfa|vtt|key)(\?|#|$)/i;

/** Content-Types that identify a manifest even when the URL looks like nothing. */
export const HLS_MIME = new Set([
  'application/vnd.apple.mpegurl',
  'application/x-mpegurl',
  'application/mpegurl',
  'audio/mpegurl',
  'audio/x-mpegurl',
  'vnd.apple.mpegurl',
]);
export const DASH_MIME = new Set(['application/dash+xml', 'video/vnd.mpeg.dash.mpd']);

/** Sites where the page URL itself is the download handle (yt-dlp extracts). */
export const SITE_HOSTS = [
  'youtube.com', 'youtu.be', 'youtube-nocookie.com',
  'vimeo.com', 'dailymotion.com', 'twitch.tv',
  'twitter.com', 'x.com', 'tiktok.com',
  'instagram.com', 'facebook.com', 'fb.watch',
  'reddit.com', 'soundcloud.com', 'bilibili.com',
  'nicovideo.jp', 'odysee.com', 'rumble.com',
  'streamable.com', 'bitchute.com', 'ted.com',
];

/**
 * Hosts that only ever serve telemetry, auth or ad traffic. Nothing here is
 * ever downloadable, on any site.
 */
const NOISE_HOSTS = [
  'accounts.google.com', 'accounts.youtube.com',
  'google-analytics.com', 'googletagmanager.com', 'analytics.google.com',
  'doubleclick.net', 'googlesyndication.com', 'googleadservices.com',
  'scorecardresearch.com', 'adservice.google.com',
  'play.google.com', 'ogs.google.com', 'gstatic.com',
  'sentry.io', 'bugsnag.com', 'segment.io', 'amplitude.com',
];

/**
 * Request paths that are API/telemetry endpoints rather than media.
 *
 * The YouTube entries are the ones that motivated this: a watch page fires
 * `/youtubei/v1/*`, `/api/stats/*`, `/ptracking`, `/generate_204` and
 * `accounts.youtube.com/RotateCookiesPage` continuously, and every one of them
 * used to land in the popup as a "site" download.
 */
const NOISE_PATH_RE = new RegExp(
  [
    '/api/stats/',        // youtube: /api/stats/atr, /api/stats/watchtime, qoe
    '/youtubei/v1/',      // youtube internal JSON API
    '/rotatecookiespage', // youtube account cookie refresh
    '/generate_204',
    '/gen_204',
    '/error_204',
    '/csi_204',
    '/log_event',
    '/ptracking',
    '/pagead/',
    '/videogoodput',
    '/att/get',
    '/youtubei/',
    '/beacon',
    '/telemetry',
    '/analytics',
  ].join('|'),
  'i',
);

/**
 * Hosts whose media URLs exist but are useless on their own: single-use,
 * IP-locked and short-lived. Only the extractor can turn the page into a file,
 * so these must never appear as separate entries.
 *
 * googlevideo.com is YouTube's media CDN — the very `videoplayback` URLs a
 * naive sniffer would proudly capture, which then 403 minutes later.
 */
const EXTRACTOR_ONLY_HOSTS = ['googlevideo.com', 'c.youtube.com'];

function hostOf(url: string): string {
  try {
    return new URL(url).hostname.toLowerCase().replace(/^www\./, '');
  } catch {
    return '';
  }
}

function hostMatches(host: string, list: string[]): boolean {
  return list.some((h) => host === h || host.endsWith(`.${h}`));
}

/** True when a URL is telemetry, auth or advertising traffic. */
export function isNoiseUrl(url: string): boolean {
  const host = hostOf(url);
  if (!host) return true;
  if (hostMatches(host, NOISE_HOSTS)) return true;

  try {
    // Match on path + query: some endpoints are identified by a query key.
    const u = new URL(url);
    return NOISE_PATH_RE.test(u.pathname + u.search);
  } catch {
    return true;
  }
}

/** True when only the extractor can use this host's media URLs. */
export function isExtractorOnlyHost(url: string): boolean {
  return hostMatches(hostOf(url), EXTRACTOR_ONLY_HOSTS);
}

/** True for hosts where the page URL itself is the download handle. */
export function isSiteHost(url: string): boolean {
  return hostMatches(hostOf(url), SITE_HOSTS);
}

export function isYouTubeHost(url: string): boolean {
  return hostMatches(hostOf(url), ['youtube.com', 'youtu.be', 'youtube-nocookie.com']);
}

/**
 * True when a URL is a *page* showing one video, as opposed to any other
 * request that merely happens to hit the same domain.
 *
 * This is the fix for the core bug: membership of SITE_HOSTS alone used to be
 * enough to call something downloadable, so every XHR on youtube.com became an
 * entry in the popup.
 */
export function isWatchPageUrl(url: string): boolean {
  let u: URL;
  try {
    u = new URL(url);
  } catch {
    return false;
  }
  if (u.protocol !== 'http:' && u.protocol !== 'https:') return false;
  if (isNoiseUrl(url)) return false;

  const host = hostOf(url);
  const path = u.pathname;

  if (hostMatches(host, ['youtube.com', 'youtube-nocookie.com'])) {
    if (path === '/watch' && u.searchParams.get('v')) return true;
    return /^\/(shorts|live|embed|v)\/[\w-]+/.test(path);
  }
  if (host === 'youtu.be') return /^\/[\w-]{6,}/.test(path);

  if (host === 'vimeo.com') return /^\/\d+/.test(path) || /^\/[\w-]+\/[\w-]+/.test(path);
  if (host === 'dailymotion.com') return /^\/video\//.test(path);
  if (host === 'twitch.tv') return /^\/videos\/\d+/.test(path) || /^\/[\w-]+\/clip\//.test(path);
  if (hostMatches(host, ['twitter.com', 'x.com'])) return /\/status\/\d+/.test(path);
  if (host === 'tiktok.com') return /\/video\/\d+/.test(path) || /^\/t\//.test(path);
  if (host === 'instagram.com') return /^\/(p|reel|tv)\//.test(path);
  if (hostMatches(host, ['facebook.com', 'fb.watch'])) return /\/(videos?|watch|reel)\//.test(path) || host === 'fb.watch';
  if (host === 'reddit.com') return /\/comments\//.test(path);
  if (host === 'soundcloud.com') return /^\/[\w-]+\/[\w-]+/.test(path);
  if (host === 'bilibili.com') return /^\/video\//.test(path);
  if (host === 'nicovideo.jp') return /^\/watch\//.test(path);
  if (hostMatches(host, ['odysee.com', 'rumble.com', 'streamable.com', 'bitchute.com'])) {
    return path.length > 1;
  }
  if (host === 'ted.com') return /^\/talks\//.test(path);

  return false;
}

/**
 * Reduces a video page URL to the canonical form the downloader should receive.
 *
 * For YouTube this strips `&list=`, `&index=`, `&t=`, `&pp=` and friends. A
 * `list` parameter makes yt-dlp route through its *playlist* extractor, which
 * then fails with "Playlists that require authentication" on mixes and
 * auto-generated radio lists — even though the user only wanted the one video
 * they were watching.
 */
export function cleanPageUrl(url: string): string {
  let u: URL;
  try {
    u = new URL(url);
  } catch {
    return url;
  }
  if (!isYouTubeHost(url)) return url;

  u.hash = '';
  const host = hostOf(url);

  // youtu.be/<id> -> the canonical watch URL.
  if (host === 'youtu.be') {
    const id = u.pathname.replace(/^\//, '').split('/')[0];
    return id ? `https://www.youtube.com/watch?v=${id}` : url;
  }

  const v = u.searchParams.get('v');
  if (u.pathname === '/watch' && v) {
    // Keep the video id and nothing else.
    return `https://www.youtube.com/watch?v=${v}`;
  }

  // /shorts/<id>, /live/<id>, /embed/<id>: the path is the whole identity.
  if (/^\/(shorts|live|embed|v)\/[\w-]+/.test(u.pathname)) {
    u.search = '';
    return u.toString();
  }

  // Anything else (a real /playlist or /channel URL) is left alone.
  return url;
}

/** Mirrors downloader.Classify on the Go side. */
export function classify(url: string, mime: string): StreamType {
  const clean = mime.split(';')[0].trim().toLowerCase();
  if (HLS_MIME.has(clean)) return 'hls';
  if (DASH_MIME.has(clean)) return 'dash';
  if (HLS_EXT_RE.test(url)) return 'hls';
  if (DASH_EXT_RE.test(url)) return 'dash';
  // Only a genuine watch PAGE is a "site" download — not every request that
  // happens to hit the same domain.
  if (isWatchPageUrl(url)) return 'site';
  return 'direct';
}

export function kindFor(streamType: StreamType, mime: string, url: string): MediaKind {
  if (streamType !== 'direct') return 'stream';
  if (mime.startsWith('video/')) return 'video';
  if (mime.startsWith('audio/')) return 'audio';
  if (mime.startsWith('image/')) return 'image';
  if (MEDIA_EXT_RE.test(url)) return 'video';
  if (IMAGE_EXT_RE.test(url)) return 'image';
  return 'other';
}

/**
 * The single decision the sniffer asks about every response.
 *
 * Returning a reason (rather than a bare boolean) keeps the "why did my video
 * not show up" question answerable from the service worker log.
 */
export function sniffDecision(
  url: string,
  mime: string,
): { keep: false; reason: string } | { keep: true; streamType: StreamType; kind: MediaKind } {
  if (!/^https?:/i.test(url)) return { keep: false, reason: 'not http(s)' };
  if (isNoiseUrl(url)) return { keep: false, reason: 'telemetry/auth/ad endpoint' };
  if (SEGMENT_EXT_RE.test(url)) return { keep: false, reason: 'stream segment' };
  if (isExtractorOnlyHost(url)) return { keep: false, reason: 'extractor-only CDN' };

  const streamType = classify(url, mime);

  // A watch page arrives here as a main_frame request. The page entry is built
  // from the tab itself (with its title and favicon), so capturing the request
  // as well would just duplicate it.
  if (streamType === 'site') return { keep: false, reason: 'page URL, offered via the tab entry' };

  const kind = kindFor(streamType, mime, url);
  if (streamType === 'direct' && kind === 'other') {
    return { keep: false, reason: 'not media' };
  }
  return { keep: true, streamType, kind };
}


/**
 * Extracts the video id from any YouTube URL shape: /watch?v=, youtu.be/,
 * /shorts/, /live/, /embed/.
 */
export function youTubeVideoId(raw: string): string | undefined {
  if (!isYouTubeHost(raw)) return undefined;
  let u: URL;
  try {
    u = new URL(raw);
  } catch {
    return undefined;
  }
  if (hostOf(raw) === 'youtu.be') {
    const id = u.pathname.replace(/^\//, '').split('/')[0];
    return /^[\w-]{6,}$/.test(id) ? id : undefined;
  }
  const v = u.searchParams.get('v');
  if (v && /^[\w-]{6,}$/.test(v)) return v;
  const m = u.pathname.match(/^\/(?:shorts|live|embed|v)\/([\w-]{6,})/);
  return m ? m[1] : undefined;
}

/**
 * YouTube's predictable still for a video id. hqdefault exists for every video,
 * unlike maxresdefault which 404s on anything not uploaded in HD.
 */
export function youTubeThumbnail(id: string | undefined): string | undefined {
  return id ? `https://img.youtube.com/vi/${id}/hqdefault.jpg` : undefined;
}
