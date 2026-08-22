/** Shared contracts between the service worker, the popup and the Go host. */

/** Name declared in com.downloader.app.json — must match exactly. */
export const NATIVE_HOST = 'com.downloader.app';

/**
 * How the Go engine should fetch a URL. This mirrors downloader.Kind on the Go
 * side: "direct" goes to the chunked HTTP engine, everything else to yt-dlp.
 */
export type StreamType = 'direct' | 'hls' | 'dash' | 'site';

/** What the item is, for icons and grouping in the UI. */
export type MediaKind = 'video' | 'audio' | 'image' | 'stream' | 'other';

/** Message sent to the Go native messaging host. */
export interface HostRequest {
  type: 'download' | 'ping' | 'status' | 'open_dashboard';
  url?: string;
  filename?: string;
  referrer?: string;
  cookie?: string;
  userAgent?: string;
  /** Engine hint — the extension saw the live Content-Type, the URL alone lies. */
  kind?: StreamType;
  mime?: string;
  title?: string;
  requestId?: string;
}

/** Reply from the Go native messaging host. */
export interface HostResponse {
  ok: boolean;
  type: string;
  error?: string;
  jobId?: string;
  requestId?: string;
  dashboard?: string;
  version?: string;
  /** False when yt-dlp/ffmpeg are missing, so the popup can warn up front. */
  toolsReady?: boolean;
}

/** One media file or stream spotted on a page. */
export interface MediaItem {
  id: string;
  url: string;
  filename: string;
  kind: MediaKind;
  streamType: StreamType;
  mime: string;
  size: number;
  tabId: number;
  pageUrl: string;
  pageTitle: string;
  seenAt: number;
  /** Remote URL or data: URL. Absent until a thumbnail scan runs. */
  thumbnail?: string;
  /** Seconds, when known (from the <video> element). */
  duration?: number;
  width?: number;
  height?: number;
  /** Short human label: "HLS stream", "1080p", "page". */
  label?: string;
}

/** What content.ts sends back for one media element on the page. */
export interface ScannedMedia {
  tag: 'video' | 'audio' | 'image';
  src: string;
  poster?: string;
  /** JPEG data: URL captured from a canvas, when the frame was readable. */
  thumbnail?: string;
  title?: string;
  duration?: number;
  width?: number;
  height?: number;
}

export interface ScanResult {
  ok: boolean;
  pageTitle: string;
  pageUrl: string;
  media: ScannedMedia[];
  error?: string;
}

/** Messages exchanged between the popup and the service worker. */
export type UiMessage =
  | { kind: 'list'; tabId?: number; withThumbnails?: boolean }
  | { kind: 'download'; item: MediaItem }
  | { kind: 'downloadUrl'; url: string; pageUrl?: string }
  | { kind: 'downloadPage'; tabId: number }
  | { kind: 'clear'; tabId?: number }
  | { kind: 'ping' }
  | { kind: 'openDashboard' }
  | { kind: 'getSettings' }
  | { kind: 'setSettings'; settings: Partial<Settings> };

export interface Settings {
  /** Take over downloads Chrome itself starts (IDM-style interception). */
  interceptChromeDownloads: boolean;
  /** Ignore images below this size, in bytes: kills icon/spinner noise. */
  minImageBytes: number;
  /** Detect images at all. */
  captureImages: boolean;
  /** Sniff HLS/DASH manifests. */
  captureStreams: boolean;
  /** Grab thumbnails by injecting the page scanner when the popup opens. */
  captureThumbnails: boolean;
}

export const DEFAULT_SETTINGS: Settings = {
  interceptChromeDownloads: false,
  minImageBytes: 100 * 1024,
  captureImages: true,
  captureStreams: true,
  captureThumbnails: true,
};
