package downloader

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/quick-download/backend/internal/config"
)

func TestQualityArgs(t *testing.T) {
	cases := []struct {
		quality     string
		wantFormat  string
		wantMerge   bool
		wantExtract bool
	}{
		{"", "bv*+ba/b", true, false},
		{"best", "bv*+ba/b", true, false},
		{"nonsense", "bv*+ba/b", true, false}, // unknown falls back to best
		{"1080p", "bv*[height<=1080]+ba/b", true, false},
		{"1080", "bv*[height<=1080]+ba/b", true, false},
		{"720p", "bv*[height<=720]+ba/b", true, false},
		{"720", "bv*[height<=720]+ba/b", true, false},
		{"audio", "ba/b", false, true},
		{"AUDIO", "ba/b", false, true},
		{"mp3", "ba/b", false, true},
	}

	for _, tc := range cases {
		args := qualityArgs(tc.quality)
		joined := strings.Join(args, " ")

		// -f must be immediately followed by the selector.
		var format string
		for i, a := range args {
			if a == "-f" && i+1 < len(args) {
				format = args[i+1]
			}
		}
		if format != tc.wantFormat {
			t.Errorf("quality %q: format = %q, want %q", tc.quality, format, tc.wantFormat)
		}
		if got := strings.Contains(joined, "--merge-output-format"); got != tc.wantMerge {
			t.Errorf("quality %q: merge-output-format = %v, want %v", tc.quality, got, tc.wantMerge)
		}
		if got := strings.Contains(joined, "--extract-audio"); got != tc.wantExtract {
			t.Errorf("quality %q: extract-audio = %v, want %v", tc.quality, got, tc.wantExtract)
		}
	}
}

// Audio-only must not ask for an mp4 container: there is no video to merge and
// the extension is decided by --audio-format.
func TestAudioOnlyHasNoVideoContainer(t *testing.T) {
	joined := strings.Join(qualityArgs("audio"), " ")
	if strings.Contains(joined, "mp4") {
		t.Fatalf("audio-only args must not mention mp4: %s", joined)
	}
	if !strings.Contains(joined, "--audio-format mp3") {
		t.Fatalf("expected an mp3 audio format: %s", joined)
	}
}

func TestQualityReachesTheCommandLine(t *testing.T) {
	mgr := NewManager(testConfig(t), nil)
	tools := config.Tools{
		YtDlp:  config.Tool{Path: filepath.Join(t.TempDir(), "yt-dlp"), Found: true},
		Ffmpeg: config.Tool{Path: filepath.Join(t.TempDir(), "ffmpeg"), Found: true},
	}
	dir := t.TempDir()

	job := &Job{
		ID:  "q",
		URL: "https://www.youtube.com/watch?v=abc",
		dir: dir,
		req: Request{Quality: "720p"},
	}
	joined := strings.Join(mgr.ytDlpArgs(job, tools), " ")
	if !strings.Contains(joined, "bv*[height<=720]+ba/b") {
		t.Errorf("720p selector missing from args:\n%s", joined)
	}
	// The output template must point at the job's directory, not the default.
	if !strings.Contains(joined, dir) {
		t.Errorf("output template does not use the job directory %q:\n%s", dir, joined)
	}
}

func TestResolveDownloadDir(t *testing.T) {
	def := t.TempDir()

	t.Run("empty uses the default", func(t *testing.T) {
		got, err := resolveDownloadDir(def, "")
		if err != nil || got != def {
			t.Fatalf("got %q, %v; want %q", got, err, def)
		}
		if got, _ := resolveDownloadDir(def, "   "); got != def {
			t.Fatalf("whitespace should count as empty, got %q", got)
		}
	})

	t.Run("creates a missing absolute directory", func(t *testing.T) {
		target := filepath.Join(t.TempDir(), "Media", "Downloads")
		got, err := resolveDownloadDir(def, target)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != filepath.Clean(target) {
			t.Fatalf("got %q, want %q", got, target)
		}
		if st, err := os.Stat(got); err != nil || !st.IsDir() {
			t.Fatalf("directory was not created: %v", err)
		}
	})

	t.Run("rejects a relative path", func(t *testing.T) {
		// A relative path would resolve against the daemon's working directory,
		// which the user has no way to reason about.
		if _, err := resolveDownloadDir(def, "Downloads/videos"); err == nil {
			t.Fatal("expected an error for a relative path")
		}
	})

	t.Run("no write probe is left behind", func(t *testing.T) {
		target := t.TempDir()
		if _, err := resolveDownloadDir(def, target); err != nil {
			t.Fatal(err)
		}
		entries, _ := os.ReadDir(target)
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".qd-write-test") {
				t.Fatalf("probe file left behind: %s", e.Name())
			}
		}
	})
}

// A custom save path must be honoured end to end by the chunked HTTP engine too,
// not just by yt-dlp.
func TestSavePathUsedByHttpEngine(t *testing.T) {
	body := randomBody(80 * 1024)
	srv := mediaServer(t, body, serverOpts{})
	defer srv.Close()

	cfg := testConfig(t)
	target := filepath.Join(t.TempDir(), "Custom Folder")

	snap := runJob(t, cfg, Request{URL: srv.URL + "/clip.mp4", SavePath: target})
	if snap.State != StateCompleted {
		t.Fatalf("state = %s, error = %s", snap.State, snap.Error)
	}
	if filepath.Dir(snap.Path) != filepath.Clean(target) {
		t.Fatalf("file landed in %q, want %q", filepath.Dir(snap.Path), target)
	}
	if _, err := os.Stat(snap.Path); err != nil {
		t.Fatalf("output file missing: %v", err)
	}
	// Part files must go to the destination volume, not the default temp dir.
	if entries, _ := os.ReadDir(cfg.TempDir); len(entries) != 0 {
		t.Errorf("default temp dir should be untouched, has %d entries", len(entries))
	}
}

func TestBadSavePathFailsFast(t *testing.T) {
	mgr := NewManager(testConfig(t), nil)
	if _, err := mgr.Enqueue(Request{URL: "https://example.com/a.mp4", SavePath: "not/absolute"}); err == nil {
		t.Fatal("expected Enqueue to reject a relative save path")
	}
}

// TestCookiePolicy pins the interaction between the extension's allowlist and
// the engine's YouTube handling. The extension decides whether a cookie exists;
// the engine decides what to do with what it is given.
func TestCookiePolicy(t *testing.T) {
	mgr := NewManager(testConfig(t), nil)
	tools := config.Tools{
		YtDlp:  config.Tool{Path: filepath.Join(t.TempDir(), "yt-dlp"), Found: true},
		Ffmpeg: config.Tool{Path: filepath.Join(t.TempDir(), "ffmpeg"), Found: true},
	}
	dir := t.TempDir()

	withCookie := Request{UserAgent: "UA/1", Referrer: "https://page/", Cookie: "SID=abc"}
	noCookie := Request{UserAgent: "UA/1", Referrer: "https://page/"}

	cases := []struct {
		name       string
		url        string
		req        Request
		wantCookie bool
		wantUA     bool
	}{
		{
			// Default path: the allowlist collected nothing, so YouTube gets a
			// clean request and its anti-bot check stays quiet.
			name: "youtube without an allowlisted cookie sends no identity",
			url:  "https://www.youtube.com/watch?v=abc", req: noCookie,
			wantCookie: false, wantUA: false,
		},
		{
			// The user deliberately allowlisted youtube.com; honour it, and send
			// the matching UA so the identity is internally consistent.
			name: "youtube with an allowlisted cookie sends all three",
			url:  "https://www.youtube.com/watch?v=abc", req: withCookie,
			wantCookie: true, wantUA: true,
		},
		{
			name: "instagram with a cookie sends it",
			url:  "https://www.instagram.com/reel/abc/", req: withCookie,
			wantCookie: true, wantUA: true,
		},
		{
			// Not allowlisted: the extension sent no cookie, but a normal site
			// still needs the UA and Referer to serve its media.
			name: "other sites keep UA and Referer without a cookie",
			url:  "https://cdn.example.com/live/master.m3u8", req: noCookie,
			wantCookie: false, wantUA: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job := &Job{ID: "c", URL: tc.url, dir: dir, req: tc.req}
			joined := strings.Join(mgr.ytDlpArgs(job, tools), " ")

			if got := strings.Contains(joined, "Cookie:SID=abc"); got != tc.wantCookie {
				t.Errorf("cookie present = %v, want %v\n%s", got, tc.wantCookie, joined)
			}
			if got := strings.Contains(joined, "--user-agent"); got != tc.wantUA {
				t.Errorf("user-agent present = %v, want %v\n%s", got, tc.wantUA, joined)
			}
		})
	}
}
