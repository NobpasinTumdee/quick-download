// Package config holds every tunable of the engine in one place so that the
// native-messaging host process and the daemon process always agree on the
// port, the download folder and the log location.
package config

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
)

const (
	// AppName is used for the config/log folder name.
	AppName = "quick-download"
	// Version is reported by /api/health and `quick-download --version`.
	Version = "1.0.0"
	// HostName must match the "name" field of the native messaging manifest.
	HostName = "com.downloader.app"
)

// Config is the resolved runtime configuration.
type Config struct {
	Host              string // loopback interface only - never 0.0.0.0
	Port              int    // local API/WebSocket port
	DownloadDir       string // where finished files land
	TempDir           string // where .part files live while downloading
	LogDir            string // rotating-ish plain log files
	MaxChunks         int    // upper bound of parallel connections per file
	MinChunkSize      int64  // do not split below this size
	MaxConcurrentJobs int    // how many files download at the same time
	MaxRetries        int    // per-chunk retry attempts
	UserAgent         string // fallback UA when the extension does not send one
}

// Load builds the configuration from defaults + environment overrides.
// Environment overrides are handy because Chrome starts the host process for
// us and we cannot pass command line flags from the extension.
func Load() *Config {
	c := &Config{
		Host:              "127.0.0.1",
		Port:              9090,
		MaxChunks:         8,
		MinChunkSize:      1 << 20, // 1 MiB
		MaxConcurrentJobs: 3,
		MaxRetries:        4,
		UserAgent:         "QuickDownload/" + Version,
	}

	if v := os.Getenv("QD_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 && p < 65536 {
			c.Port = p
		}
	}
	if v := os.Getenv("QD_CHUNKS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.MaxChunks = clamp(n, 1, 16)
		}
	}
	if v := os.Getenv("QD_JOBS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.MaxConcurrentJobs = clamp(n, 1, 10)
		}
	}

	c.DownloadDir = os.Getenv("QD_DIR")
	if c.DownloadDir == "" {
		c.DownloadDir = defaultDownloadDir()
	}
	c.TempDir = filepath.Join(c.DownloadDir, ".qd-parts")
	c.LogDir = defaultLogDir()

	_ = os.MkdirAll(c.DownloadDir, 0o755)
	_ = os.MkdirAll(c.TempDir, 0o755)
	_ = os.MkdirAll(c.LogDir, 0o755)
	return c
}

// Addr is the listen address of the local server.
func (c *Config) Addr() string { return fmt.Sprintf("%s:%d", c.Host, c.Port) }

// BaseURL is what the GUI and the host process talk to.
func (c *Config) BaseURL() string { return fmt.Sprintf("http://127.0.0.1:%d", c.Port) }

// OpenLog opens (append mode) a log file for the given role: "host" or "daemon".
// The native messaging host MUST NOT write anything to stdout that is not a
// framed message, so every diagnostic goes to this file instead.
func (c *Config) OpenLog(role string) (*os.File, error) {
	p := filepath.Join(c.LogDir, AppName+"-"+role+".log")
	return os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

func defaultDownloadDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), AppName)
	}
	return filepath.Join(home, "Downloads")
}

func defaultLogDir() string {
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, AppName)
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ---------------------------------------------------------------------------
// External tools (yt-dlp / ffmpeg)
// ---------------------------------------------------------------------------

// Tool is one discovered external binary.
type Tool struct {
	Name  string `json:"name"`
	Path  string `json:"path"` // absolute path, empty when not found
	Found bool   `json:"found"`
}

// Tools reports where yt-dlp and ffmpeg live.
type Tools struct {
	YtDlp  Tool `json:"ytdlp"`
	Ffmpeg Tool `json:"ffmpeg"`
	// SearchedIn lists the directories probed, so the UI can tell the user
	// exactly where to drop the binaries.
	SearchedIn []string `json:"searchedIn"`
}

// Ready is true when streaming downloads are possible. ffmpeg is not optional
// in practice: HLS/DASH always needs muxing.
func (t Tools) Ready() bool { return t.YtDlp.Found && t.Ffmpeg.Found }

// exeName appends .exe on Windows.
func exeName(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".exe"
	}
	return base
}

// toolSearchDirs is the lookup order: next to our own binary first, then a
// tools/ subfolder, then the download dir's sibling. PATH is consulted last.
func (c *Config) toolSearchDirs() []string {
	var dirs []string
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		base := filepath.Dir(exe)
		dirs = append(dirs, base, filepath.Join(base, "tools"))
	}
	if wd, err := os.Getwd(); err == nil {
		dirs = append(dirs, wd, filepath.Join(wd, "bin"), filepath.Join(wd, "tools"))
	}
	return dirs
}

// findTool resolves one binary: explicit env override, then our own folders,
// then PATH. Resolution is deliberately done on every call so dropping
// yt-dlp.exe into bin/ takes effect without restarting the daemon.
func (c *Config) findTool(base, envVar string, dirs []string) Tool {
	t := Tool{Name: base}

	if custom := os.Getenv(envVar); custom != "" {
		if st, err := os.Stat(custom); err == nil && !st.IsDir() {
			t.Path, t.Found = custom, true
			return t
		}
	}

	name := exeName(base)
	for _, dir := range dirs {
		candidate := filepath.Join(dir, name)
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
			if abs, err := filepath.Abs(candidate); err == nil {
				candidate = abs
			}
			t.Path, t.Found = candidate, true
			return t
		}
	}

	if p, err := exec.LookPath(name); err == nil {
		t.Path, t.Found = p, true
	}
	return t
}

// Tools discovers the external binaries used for streaming downloads.
func (c *Config) Tools() Tools {
	dirs := c.toolSearchDirs()
	return Tools{
		YtDlp:      c.findTool("yt-dlp", "QD_YTDLP", dirs),
		Ffmpeg:     c.findTool("ffmpeg", "QD_FFMPEG", dirs),
		SearchedIn: dirs,
	}
}
