package downloader

import (
	"net/url"
	"path"
	"strings"
)

// Kind is what a URL actually points at. It decides which engine runs.
type Kind string

const (
	// KindDirect is a plain file served over HTTP: the chunked engine handles it.
	KindDirect Kind = "direct"
	// KindHLS is an Apple HLS playlist (.m3u8): thousands of small segments.
	KindHLS Kind = "hls"
	// KindDASH is an MPEG-DASH manifest (.mpd).
	KindDASH Kind = "dash"
	// KindSite is a page URL on a site yt-dlp knows how to extract from.
	KindSite Kind = "site"
)

// Engine names, as reported to the GUI.
const (
	EngineHTTP  = "http"
	EngineYtDlp = "yt-dlp"
)

// Engine maps a kind onto the engine that can download it.
func (k Kind) Engine() string {
	if k == KindDirect {
		return EngineHTTP
	}
	return EngineYtDlp
}

// Streaming reports whether this kind needs yt-dlp + ffmpeg.
func (k Kind) Streaming() bool { return k != KindDirect }

// MIME types that identify streaming manifests.
var manifestMIME = map[string]Kind{
	"application/vnd.apple.mpegurl": KindHLS,
	"application/x-mpegurl":         KindHLS,
	"application/mpegurl":           KindHLS,
	"audio/mpegurl":                 KindHLS,
	"audio/x-mpegurl":               KindHLS,
	"vnd.apple.mpegurl":             KindHLS,
	"application/dash+xml":          KindDASH,
	"video/vnd.mpeg.dash.mpd":       KindDASH,
}

// siteHosts are domains where the URL is a watch page, not a media file, so
// only an extractor can find the real streams. This list is a fast path for
// the common cases — yt-dlp itself supports thousands of sites, so anything
// the user explicitly sends with kind="site" is honoured too.
var siteHosts = []string{
	"youtube.com", "youtu.be", "youtube-nocookie.com",
	"vimeo.com", "dailymotion.com", "twitch.tv",
	"twitter.com", "x.com", "tiktok.com",
	"instagram.com", "facebook.com", "fb.watch",
	"reddit.com", "soundcloud.com", "bilibili.com",
	"nicovideo.jp", "odysee.com", "rumble.com",
	"streamable.com", "bitchute.com", "ted.com",
}

// directExt are extensions the chunked HTTP engine downloads well.
var directExt = map[string]bool{
	".mp4": true, ".webm": true, ".m4v": true, ".mov": true, ".mkv": true,
	".avi": true, ".flv": true, ".wmv": true, ".mpg": true, ".mpeg": true,
	".m4a": true, ".mp3": true, ".aac": true, ".ogg": true, ".opus": true,
	".wav": true, ".flac": true,
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true,
	".avif": true, ".bmp": true, ".svg": true, ".tif": true, ".tiff": true,
	".pdf": true, ".zip": true, ".rar": true, ".7z": true, ".gz": true,
	".iso": true, ".exe": true, ".msi": true, ".dmg": true, ".apk": true,
}

// Classify decides which engine should handle a URL.
//
// Precedence:
//  1. an explicit hint from the extension (it saw the Content-Type live),
//  2. the response MIME type, when we have one,
//  3. the URL path extension,
//  4. a known extractor-only host,
//  5. otherwise treat it as a direct file — the HTTP engine is the safe default.
func Classify(rawURL, mime, hint string) Kind {
	switch Kind(strings.ToLower(strings.TrimSpace(hint))) {
	case KindHLS:
		return KindHLS
	case KindDASH:
		return KindDASH
	case KindSite:
		return KindSite
	case KindDirect:
		return KindDirect
	}

	if mime != "" {
		clean := strings.ToLower(strings.TrimSpace(strings.Split(mime, ";")[0]))
		if k, ok := manifestMIME[clean]; ok {
			return k
		}
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return KindDirect
	}

	switch strings.ToLower(path.Ext(u.Path)) {
	case ".m3u8", ".m3u":
		return KindHLS
	case ".mpd":
		return KindDASH
	}

	host := strings.ToLower(u.Hostname())
	host = strings.TrimPrefix(host, "www.")
	for _, site := range siteHosts {
		if host == site || strings.HasSuffix(host, "."+site) {
			return KindSite
		}
	}

	// A recognised media extension is unambiguous.
	if directExt[strings.ToLower(path.Ext(u.Path))] {
		return KindDirect
	}

	// Extension-less URL with no MIME hint: the HTTP engine probes it anyway
	// and falls back gracefully, so this is the conservative choice.
	return KindDirect
}

// IsSegment reports whether a URL is an individual HLS/DASH segment rather
// than a manifest. Segments are useless on their own — the sniffer must ignore
// them or the popup fills with thousands of 4-second fragments.
func IsSegment(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	switch strings.ToLower(path.Ext(u.Path)) {
	case ".ts", ".m4s", ".cmfv", ".cmfa", ".aac", ".vtt":
		return true
	}
	return false
}
