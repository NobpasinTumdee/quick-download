/** Shared contracts between the service worker, the popup and the Go host. */

/** Name declared in com.downloader.app.json — must match exactly. */
export const NATIVE_HOST = 'com.downloader.app';

/**
 * How the Go engine should fetch a URL. This mirrors downloader.Kind on the Go
 * side: "direct" goes to the chunked HTTP engine, everything else to yt-dlp.
 */
export type StreamType = 'direct' | 'hls' | 'dash' | 'site';

/**
 * Resolution / format choice, applied by yt-dlp. It has no meaning for the
 * chunked HTTP engine: a direct file URL has exactly one representation.
 */
export type Quality = 'best' | '1080p' | '720p' | 'audio';

export const QUALITY_LABELS: ReadonlyArray<{ value: Quality; label: string }> = [
  { value: 'best', label: 'Best' },
  { value: '1080p', label: '1080p' },
  { value: '720p', label: '720p' },
  { value: 'audio', label: 'Audio only' },
];

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
  /** Format selector; omitted means "best". */
  quality?: Quality;
  /** Absolute directory override for this job. */
  savePath?: string;
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
  /**
   * Page-level artwork (og:image, or the YouTube thumbnail). This is what gives
   * a sniffed stream a preview: its <video> plays from a blob: URL whose frames
   * are usually cross-origin, so the canvas capture cannot be used.
   */
  pageThumbnail?: string;
  error?: string;
}

/**
 * One download as the Go engine reports it. Mirrors the JobSnapshot the daemon
 * broadcasts over the WebSocket - only the fields the popup renders.
 */
export interface JobProgress {
  id: string;
  url: string;
  filename: string;
  state: 'queued' | 'probing' | 'downloading' | 'merging' | 'completed' | 'failed' | 'canceled';
  progress: number;
  speed: number;
  eta: number;
  size: number;
  downloaded: number;
  engine: string;
  kind: string;
  phase?: string;
  error?: string;
}

/** What the popup needs in order to reach the engine's WebSocket. */
export interface EngineInfo {
  ok: boolean;
  version?: string;
  toolsReady?: boolean;
  /** e.g. "http://127.0.0.1:9090" - the popup derives the ws:// URL from this. */
  dashboard?: string;
  error?: string;
}

/** Messages exchanged between the popup and the service worker. */
export type UiMessage =
  | { kind: 'list'; tabId?: number; withThumbnails?: boolean }
  | { kind: 'download'; item: MediaItem; quality?: Quality }
  | { kind: 'forget'; ids: string[] }
  | { kind: 'downloadUrl'; url: string; pageUrl?: string }
  | { kind: 'downloadPage'; tabId: number }
  | { kind: 'clear'; tabId?: number }
  | { kind: 'ping' }
  | { kind: 'openDashboard' }
  | { kind: 'engineInfo' }
  | { kind: 'getSettings' }
  | { kind: 'setSettings'; settings: Partial<Settings> };

/** 'system' follows prefers-color-scheme; the toggle writes an explicit value. */
export type ThemePreference = 'system' | 'dark' | 'light';

export interface Settings {
  /** Master switch. When false the service worker sniffs nothing at all. */
  enabled: boolean;
  /** UI theme for the popup and the side panel. */
  theme: ThemePreference;
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
  /** Absolute folder to save into. Empty means the engine's default. */
  savePath: string;
  /** Default selection in the per-item quality dropdown. */
  defaultQuality: Quality;
  /** Show a Chrome notification when a download finishes. */
  notifyOnComplete: boolean;
  /** Drop finished items from the list a few seconds after they complete. */
  autoCleanup: boolean;
}

export const DEFAULT_SETTINGS: Settings = {
  enabled: true,
  theme: 'system',
  interceptChromeDownloads: false,
  minImageBytes: 100 * 1024,
  captureImages: true,
  captureStreams: true,
  captureThumbnails: true,
  savePath: '',
  defaultQuality: 'best',
  notifyOnComplete: true,
  autoCleanup: true,
};

/** How long a finished item stays visible before auto-cleanup removes it. */
export const AUTO_CLEANUP_DELAY_MS = 5000;
