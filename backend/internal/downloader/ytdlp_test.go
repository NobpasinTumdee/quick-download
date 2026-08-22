package downloader

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/quick-download/backend/internal/config"
	"time"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		url, mime, hint string
		want            Kind
	}{
		// Manifest extensions
		{"https://cdn.example/live/master.m3u8", "", "", KindHLS},
		{"https://cdn.example/v/manifest.mpd", "", "", KindDASH},
		{"https://cdn.example/v/index.m3u8?token=abc", "", "", KindHLS},

		// MIME type wins when the path has no useful extension
		{"https://cdn.example/playlist", "application/vnd.apple.mpegurl", "", KindHLS},
		{"https://cdn.example/playlist", "application/x-mpegURL; charset=utf-8", "", KindHLS},
		{"https://cdn.example/m", "application/dash+xml", "", KindDASH},

		// Extractor-only sites
		{"https://www.youtube.com/watch?v=abc123", "", "", KindSite},
		{"https://youtu.be/abc123", "", "", KindSite},
		{"https://x.com/user/status/1", "", "", KindSite},
		{"https://vimeo.com/12345", "", "", KindSite},
		{"https://music.bilibili.com/x", "", "", KindSite},

		// Plain files stay on the fast HTTP engine
		{"https://cdn.example/clip.mp4", "video/mp4", "", KindDirect},
		{"https://cdn.example/photo.jpg", "image/jpeg", "", KindDirect},
		{"https://cdn.example/archive.zip", "", "", KindDirect},

		// The extension's hint beats everything: it saw the live headers
		{"https://cdn.example/clip.mp4", "video/mp4", "hls", KindHLS},
		{"https://www.youtube.com/watch?v=abc", "", "direct", KindDirect},

		// Unknown shape defaults to the HTTP engine, which probes safely
		{"https://cdn.example/stream/12345", "", "", KindDirect},
	}

	for _, tc := range cases {
		got := Classify(tc.url, tc.mime, tc.hint)
		if got != tc.want {
			t.Errorf("Classify(%q, %q, %q) = %q, want %q", tc.url, tc.mime, tc.hint, got, tc.want)
		}
	}
}

func TestKindEngine(t *testing.T) {
	if KindDirect.Engine() != EngineHTTP {
		t.Error("direct downloads must use the HTTP engine")
	}
	for _, k := range []Kind{KindHLS, KindDASH, KindSite} {
		if k.Engine() != EngineYtDlp {
			t.Errorf("%s must use yt-dlp", k)
		}
		if !k.Streaming() {
			t.Errorf("%s must report as streaming", k)
		}
	}
}

func TestIsSegment(t *testing.T) {
	segments := []string{
		"https://cdn.example/seg00001.ts",
		"https://cdn.example/chunk_5.m4s",
		"https://cdn.example/audio.cmfa",
	}
	for _, u := range segments {
		if !IsSegment(u) {
			t.Errorf("%s should be recognised as a segment", u)
		}
	}
	for _, u := range []string{"https://cdn.example/master.m3u8", "https://cdn.example/clip.mp4"} {
		if IsSegment(u) {
			t.Errorf("%s should NOT be a segment", u)
		}
	}
}

func TestParseNum(t *testing.T) {
	cases := map[string]float64{
		"1024": 1024, "0": 0, "12.5": 12.5,
		"NA": -1, "None": -1, "": -1, "garbage": -1,
	}
	for in, want := range cases {
		if got := parseNum(in); got != want {
			t.Errorf("parseNum(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestApplyProgressTemplate covers the three ways yt-dlp expresses progress.
func TestApplyProgressTemplate(t *testing.T) {
	mgr := NewManager(testConfig(t), nil)

	t.Run("byte counts", func(t *testing.T) {
		job := newStreamJob()
		if !mgr.applyProgressTemplate(job, "downloading|5242880|10485760|NA|1048576|5|NA|NA") {
			t.Fatal("line should have parsed")
		}
		snap := job.Snapshot()
		if snap.Progress != 50 {
			t.Errorf("progress = %v, want 50", snap.Progress)
		}
		if snap.Downloaded != 5242880 || snap.Size != 10485760 {
			t.Errorf("bytes = %d/%d", snap.Downloaded, snap.Size)
		}
		if snap.Speed != 1048576 {
			t.Errorf("speed = %v, want 1048576", snap.Speed)
		}
		if snap.ETA != 5 {
			t.Errorf("eta = %v, want 5", snap.ETA)
		}
	})

	t.Run("estimate when total is unknown", func(t *testing.T) {
		job := newStreamJob()
		mgr.applyProgressTemplate(job, "downloading|250|NA|1000|100|9|NA|NA")
		if got := job.Snapshot().Progress; got != 25 {
			t.Errorf("progress = %v, want 25 (from total_bytes_estimate)", got)
		}
	})

	t.Run("fragment counts for HLS", func(t *testing.T) {
		// Live HLS usually knows neither total nor estimate, only fragments.
		job := newStreamJob()
		mgr.applyProgressTemplate(job, "downloading|4096|NA|NA|2048|NA|25|100")
		snap := job.Snapshot()
		if snap.Progress != 25 {
			t.Errorf("progress = %v, want 25 (from fragment index)", snap.Progress)
		}
		if snap.Phase != "downloading segments" {
			t.Errorf("phase = %q, want %q", snap.Phase, "downloading segments")
		}
	})

	t.Run("malformed line is ignored", func(t *testing.T) {
		job := newStreamJob()
		if mgr.applyProgressTemplate(job, "downloading|1|2") {
			t.Error("a short line must not be accepted")
		}
	})
}

// TestConsumeYtDlpLine covers filename discovery and the legacy fallback.
func TestConsumeYtDlpLine(t *testing.T) {
	mgr := NewManager(testConfig(t), nil)

	t.Run("destination then merger", func(t *testing.T) {
		job := newStreamJob()
		// yt-dlp downloads video, then audio, then merges. Only the merged
		// path is the file the user keeps.
		mgr.consumeYtDlpLine(job, `[download] Destination: C:\dl\Clip [abc].f137.mp4`)
		if got := job.Snapshot().Filename; got != "Clip [abc].f137.mp4" {
			t.Errorf("filename = %q", got)
		}
		mgr.consumeYtDlpLine(job, `[download] Destination: C:\dl\Clip [abc].f140.m4a`)
		mgr.consumeYtDlpLine(job, `[Merger] Merging formats into "C:\dl\Clip [abc].mp4"`)

		snap := job.Snapshot()
		if snap.Filename != "Clip [abc].mp4" {
			t.Errorf("after merge filename = %q, want %q", snap.Filename, "Clip [abc].mp4")
		}
		if snap.Phase != "merging" {
			t.Errorf("phase = %q, want merging", snap.Phase)
		}

		// A stray Destination line arriving late must not clobber the merged name.
		mgr.consumeYtDlpLine(job, `[download] Destination: C:\dl\something-else.mp4`)
		if got := job.Snapshot().Filename; got != "Clip [abc].mp4" {
			t.Errorf("merged filename was overwritten: %q", got)
		}
	})

	t.Run("already downloaded", func(t *testing.T) {
		job := newStreamJob()
		mgr.consumeYtDlpLine(job, `[download] C:\dl\Cached.mp4 has already been downloaded`)
		snap := job.Snapshot()
		if snap.Filename != "Cached.mp4" {
			t.Errorf("filename = %q", snap.Filename)
		}
		if snap.Progress != 100 {
			t.Errorf("progress = %v, want 100", snap.Progress)
		}
	})

	t.Run("legacy percent only when template is silent", func(t *testing.T) {
		job := newStreamJob()
		mgr.consumeYtDlpLine(job, `[download]  42.7% of ~50.00MiB at 1.20MiB/s ETA 00:30`)
		if got := job.Snapshot().Progress; got != 42.7 {
			t.Errorf("progress = %v, want 42.7", got)
		}

		// Once template progress arrives, the scraped percent must be ignored.
		mgr.applyProgressTemplate(job, "downloading|10|100|NA|NA|NA|NA|NA")
		mgr.consumeYtDlpLine(job, `[download]  99.9% of ~50.00MiB`)
		if got := job.Snapshot().Progress; got != 10 {
			t.Errorf("progress = %v, want 10 (template wins over scraping)", got)
		}
	})
}

func TestScanLinesHandlesCarriageReturns(t *testing.T) {
	// yt-dlp redraws its progress bar with \r; without \r splitting we would
	// see one gigantic line and no live progress at all.
	input := strings.NewReader("first\r50%\r75%\nsecond\r\nthird")
	var got []string
	scanLines(input, func(line string) { got = append(got, line) })

	want := []string{"first", "50%", "75%", "second", "third"}
	if len(got) != len(want) {
		t.Fatalf("got %d lines %q, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRedactArgsHidesCookies(t *testing.T) {
	args := []string{"--add-header", "Cookie:session=supersecret", "--", "https://x/y"}
	got := strings.Join(redactArgs(args), " ")
	if strings.Contains(got, "supersecret") {
		t.Fatalf("cookie value leaked into the log line: %s", got)
	}
	if !strings.Contains(got, "Cookie:<redacted>") {
		t.Fatalf("expected a redaction marker, got %s", got)
	}
}

func TestMissingToolsProduceActionableError(t *testing.T) {
	cfg := testConfig(t)
	// Point the lookup at an empty directory so nothing can be found.
	t.Setenv("QD_YTDLP", filepath.Join(t.TempDir(), "definitely-not-here"))
	t.Setenv("PATH", t.TempDir())

	mgr := NewManager(cfg, nil)
	job := newStreamJob()
	err := mgr.runYtDlp(context.Background(), job, KindHLS)
	if err == nil {
		t.Fatal("expected an error when yt-dlp is missing")
	}
	if !strings.Contains(err.Error(), "yt-dlp") {
		t.Errorf("error should name the missing tool: %v", err)
	}
}

// --- end-to-end against a fake yt-dlp -------------------------------------

// fakeYtDlp builds a stand-in binary that prints a realistic yt-dlp transcript.
// It lets us exercise the whole pipe -> parse -> snapshot path without
// depending on a real yt-dlp install or network access.
func fakeYtDlp(t *testing.T, script string) (dir string) {
	t.Helper()
	dir = t.TempDir()
	src := filepath.Join(dir, "main.go")

	if err := os.WriteFile(src, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module fakeytdlp\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, exeName("yt-dlp"))
	build := exec.Command("go", "build", "-o", out, ".")
	build.Dir = dir
	if combined, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the fake yt-dlp failed: %v\n%s", err, combined)
	}

	// runYtDlp also insists on ffmpeg being present.
	ffmpeg := filepath.Join(dir, exeName("ffmpeg"))
	if err := os.WriteFile(ffmpeg, []byte("not a real binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func exeName(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".exe"
	}
	return base
}

const fakeYtDlpSuccess = `package main

import (
	"fmt"
	"os"
	"time"
)

// Mimics a real yt-dlp run: separate video and audio streams, then a merge.
func main() {
	dest := os.Getenv("FAKE_DEST")
	fmt.Println("[youtube] Extracting URL: " + os.Args[len(os.Args)-1])
	fmt.Println("[info] abc123: Downloading 1 format(s): 137+140")
	fmt.Println("[download] Destination: " + dest + ".f137.mp4")
	for i := 0; i <= 100; i += 25 {
		fmt.Printf("@QDP@downloading|%d|100|NA|1048576|%d|NA|NA\n", i, (100-i)/25)
		time.Sleep(10 * time.Millisecond)
	}
	fmt.Println("@QDP@finished|100|100|NA|NA|0|NA|NA")
	fmt.Println("[download] Destination: " + dest + ".f140.m4a")
	fmt.Println("@QDP@downloading|50|100|NA|1048576|1|NA|NA")
	fmt.Println("@QDPP@started|Merger")
	fmt.Println("[Merger] Merging formats into \"" + dest + "\"")
	time.Sleep(10 * time.Millisecond)
}
`

const fakeYtDlpFailure = `package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "[generic] Extracting URL")
	fmt.Fprintln(os.Stderr, "ERROR: Unsupported URL: https://example.com/nope")
	os.Exit(1)
}
`

func TestYtDlpEndToEndSuccess(t *testing.T) {
	cfg := testConfig(t)
	dir := fakeYtDlp(t, fakeYtDlpSuccess)
	t.Setenv("QD_YTDLP", filepath.Join(dir, exeName("yt-dlp")))
	t.Setenv("QD_FFMPEG", filepath.Join(dir, exeName("ffmpeg")))

	dest := filepath.Join(cfg.DownloadDir, "Great Video [abc123].mp4")
	t.Setenv("FAKE_DEST", dest)

	mgr := NewManager(cfg, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go mgr.Run(ctx)

	job, err := mgr.Enqueue(Request{
		URL:  "https://www.youtube.com/watch?v=abc123",
		Kind: "site",
	})
	if err != nil {
		t.Fatal(err)
	}

	snap := waitTerminal(t, job, 25*time.Second)
	if snap.State != StateCompleted {
		t.Fatalf("state = %s, error = %s", snap.State, snap.Error)
	}
	if snap.Engine != EngineYtDlp {
		t.Errorf("engine = %q, want yt-dlp", snap.Engine)
	}
	if snap.Kind != string(KindSite) {
		t.Errorf("kind = %q, want site", snap.Kind)
	}
	if snap.Progress != 100 {
		t.Errorf("progress = %v, want 100", snap.Progress)
	}
	if snap.Filename != filepath.Base(dest) {
		t.Errorf("filename = %q, want %q", snap.Filename, filepath.Base(dest))
	}
	if snap.Connections != 0 {
		t.Errorf("streaming jobs have no byte-range connections, got %d", snap.Connections)
	}
}

func TestYtDlpEndToEndFailureSurfacesStderr(t *testing.T) {
	cfg := testConfig(t)
	dir := fakeYtDlp(t, fakeYtDlpFailure)
	t.Setenv("QD_YTDLP", filepath.Join(dir, exeName("yt-dlp")))
	t.Setenv("QD_FFMPEG", filepath.Join(dir, exeName("ffmpeg")))

	mgr := NewManager(cfg, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go mgr.Run(ctx)

	job, err := mgr.Enqueue(Request{URL: "https://cdn.example/live.m3u8"})
	if err != nil {
		t.Fatal(err)
	}

	snap := waitTerminal(t, job, 25*time.Second)
	if snap.State != StateFailed {
		t.Fatalf("state = %s, want failed", snap.State)
	}
	// The user must see yt-dlp's own diagnosis, not just "exit status 1".
	if !strings.Contains(snap.Error, "Unsupported URL") {
		t.Errorf("error should carry the yt-dlp message, got %q", snap.Error)
	}
}

func TestStreamingJobUsesYtDlpEngine(t *testing.T) {
	mgr := NewManager(testConfig(t), nil)
	for _, tc := range []struct{ url, engine string }{
		{"https://cdn.example/master.m3u8", EngineYtDlp},
		{"https://cdn.example/manifest.mpd", EngineYtDlp},
		{"https://youtube.com/watch?v=x", EngineYtDlp},
		{"https://cdn.example/clip.mp4", EngineHTTP},
	} {
		job, err := mgr.Enqueue(Request{URL: tc.url})
		if err != nil {
			t.Fatal(err)
		}
		if got := job.Snapshot().Engine; got != tc.engine {
			t.Errorf("%s -> engine %q, want %q", tc.url, got, tc.engine)
		}
	}
}

// --- helpers ---------------------------------------------------------------

func newStreamJob() *Job {
	j := &Job{
		ID:     "test",
		URL:    "https://cdn.example/master.m3u8",
		engine: EngineYtDlp,
		kind:   KindHLS,
		state:  StateDownloading,
	}
	j.ext.percent = -1
	j.ext.eta = -1
	return j
}

func waitTerminal(t *testing.T, job *Job, within time.Duration) JobSnapshot {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		snap := job.Snapshot()
		switch snap.State {
		case StateCompleted, StateFailed, StateCanceled:
			return snap
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job did not finish in time, last state = %s", job.Snapshot().State)
	return JobSnapshot{}
}

func TestCountTracks(t *testing.T) {
	cases := map[string]int{"137+140": 2, "22": 1, "bv+ba+extra": 3, "best": 1}
	for spec, want := range cases {
		if got := countTracks(spec); got != want {
			t.Errorf("countTracks(%q) = %d, want %d", spec, got, want)
		}
	}
}

// TestMultiStreamProgressIsMonotonic is the regression guard for the ugliest
// artefact of driving yt-dlp: it downloads video and audio as two separate
// passes, and its percent restarts at 0 for the second one. The bar must fold
// both passes into a single number that never moves backwards.
func TestMultiStreamProgressIsMonotonic(t *testing.T) {
	mgr := NewManager(testConfig(t), nil)
	job := newStreamJob()

	// yt-dlp announces two tracks up front.
	mgr.consumeYtDlpLine(job, "[info] abc123: Downloading 1 format(s): 137+140")

	// Track 1: video, 0 -> 100%.
	mgr.consumeYtDlpLine(job, `[download] Destination: C:\dl\clip.f137.mp4`)
	var last float64
	for _, pct := range []int{0, 25, 50, 75, 100} {
		mgr.applyProgressTemplate(job, progressLine(pct))
		got := job.Snapshot().Progress
		if got < last {
			t.Fatalf("progress went backwards during track 1: %v after %v", got, last)
		}
		last = got
	}
	// Half the work done means half the bar.
	if last != 50 {
		t.Errorf("after the first of two tracks progress = %v, want 50", last)
	}

	// Track 2: audio, percent restarts at 0 — the bar must not.
	mgr.consumeYtDlpLine(job, `[download] Destination: C:\dl\clip.f140.m4a`)
	for _, pct := range []int{0, 20, 60, 100} {
		mgr.applyProgressTemplate(job, progressLine(pct))
		got := job.Snapshot().Progress
		if got < last {
			t.Fatalf("progress went backwards on the second track: %v after %v", got, last)
		}
		last = got
	}
	if last != 100 {
		t.Errorf("after both tracks progress = %v, want 100", last)
	}
}

func TestSingleTrackProgressUnaffected(t *testing.T) {
	mgr := NewManager(testConfig(t), nil)
	job := newStreamJob()
	mgr.consumeYtDlpLine(job, "[info] abc: Downloading 1 format(s): 22")
	mgr.consumeYtDlpLine(job, `[download] Destination: C:\dl\clip.mp4`)

	mgr.applyProgressTemplate(job, progressLine(40))
	if got := job.Snapshot().Progress; got != 40 {
		t.Errorf("single-track progress = %v, want 40", got)
	}
	// A single track knows the real file size, so it must be published.
	if got := job.Snapshot().Size; got != 1000 {
		t.Errorf("size = %d, want 1000", got)
	}
}

// progressLine builds a template line at the given per-track percentage.
func progressLine(pct int) string {
	return fmt.Sprintf("downloading|%d|1000|NA|1048576|3|NA|NA", pct*10)
}

func TestIsYouTubeURL(t *testing.T) {
	yes := []string{
		"https://www.youtube.com/watch?v=abc",
		"https://youtube.com/watch?v=abc",
		"https://m.youtube.com/watch?v=abc",
		"https://music.youtube.com/watch?v=abc",
		"https://accounts.youtube.com/RotateCookiesPage",
		"https://youtu.be/abc",
		"https://www.youtube-nocookie.com/embed/abc",
	}
	for _, u := range yes {
		if !isYouTubeURL(u) {
			t.Errorf("isYouTubeURL(%q) = false, want true", u)
		}
	}
	// Suffix matching must not be fooled by lookalike hostnames.
	no := []string{
		"https://youtube.com.evil.test/watch?v=abc",
		"https://notyoutube.com/watch?v=abc",
		"https://myyoutu.be/abc",
		"https://vimeo.com/123",
		"https://cdn.example.com/clip.mp4",
	}
	for _, u := range no {
		if isYouTubeURL(u) {
			t.Errorf("isYouTubeURL(%q) = true, want false", u)
		}
	}
}

// TestCleanMediaURL covers the "&list=" problem: a watch URL copied while a mix
// is playing routes yt-dlp through youtube:tab, which fails with
// "Playlists that require authentication".
func TestCleanMediaURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{
			"https://www.youtube.com/watch?v=dQw4w9WgXcQ&list=RDdQw4w9WgXcQ&start_radio=1&index=1",
			"https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		},
		{
			"https://www.youtube.com/watch?v=abc123&t=42s&pp=ygUJdGVzdA%3D%3D&si=xyz",
			"https://www.youtube.com/watch?v=abc123",
		},
		{"https://youtu.be/abc123?list=PLxyz&t=10", "https://www.youtube.com/watch?v=abc123"},
		{"https://m.youtube.com/watch?v=abc123&list=PL1", "https://www.youtube.com/watch?v=abc123"},
		{"https://www.youtube.com/shorts/abc123?feature=share", "https://www.youtube.com/shorts/abc123"},
		{"https://www.youtube.com/watch?v=abc123", "https://www.youtube.com/watch?v=abc123"},

		// A deliberate playlist URL has no video to fall back on: leave it be.
		{"https://www.youtube.com/playlist?list=PLabc", "https://www.youtube.com/playlist?list=PLabc"},

		// Never touch other sites - their query strings carry real tokens.
		{"https://cdn.example.com/clip.mp4?token=keep&list=important", "https://cdn.example.com/clip.mp4?token=keep&list=important"},
		{"https://vimeo.com/123456?autoplay=1", "https://vimeo.com/123456?autoplay=1"},
	}
	for _, tc := range cases {
		if got := cleanMediaURL(tc.in); got != tc.want {
			t.Errorf("cleanMediaURL(%q)\n  got  %q\n  want %q", tc.in, got, tc.want)
		}
	}
}

// TestBrowserHeadersOnlyForNonYouTube is the regression guard for
// "ERROR: [youtube] ... The page needs to be reloaded": forwarding the
// browser's cookie/UA to yt-dlp is what triggers YouTube's anti-bot check.
func TestBrowserHeadersOnlyForNonYouTube(t *testing.T) {
	cfg := testConfig(t)
	mgr := NewManager(cfg, nil)
	tools := config.Tools{
		YtDlp:  config.Tool{Name: "yt-dlp", Path: filepath.Join(t.TempDir(), "yt-dlp"), Found: true},
		Ffmpeg: config.Tool{Name: "ffmpeg", Path: filepath.Join(t.TempDir(), "ffmpeg"), Found: true},
	}
	req := Request{
		UserAgent: "Mozilla/5.0 TestBrowser",
		Referrer:  "https://example.com/watch",
		Cookie:    "SID=secret; HSID=alsosecret",
	}

	t.Run("youtube sends none of them", func(t *testing.T) {
		job := &Job{ID: "y", URL: "https://www.youtube.com/watch?v=abc&list=RDabc", req: req}
		args := strings.Join(mgr.ytDlpArgs(job, tools), " ")
		for _, forbidden := range []string{"--user-agent", "--referer", "--add-header", "secret"} {
			if strings.Contains(args, forbidden) {
				t.Errorf("YouTube args must not contain %q:\n%s", forbidden, args)
			}
		}
		// The playlist parameter must be gone by the time yt-dlp sees it.
		if strings.Contains(args, "list=") {
			t.Errorf("playlist parameter survived into the args:\n%s", args)
		}
		if !strings.HasSuffix(args, "-- https://www.youtube.com/watch?v=abc") {
			t.Errorf("unexpected target URL:\n%s", args)
		}
	})

	t.Run("other sites still get all three", func(t *testing.T) {
		job := &Job{ID: "o", URL: "https://cdn.example.com/live/master.m3u8", req: req}
		args := mgr.ytDlpArgs(job, tools)
		joined := strings.Join(args, " ")
		for _, required := range []string{"--user-agent", "--referer", "--add-header"} {
			if !strings.Contains(joined, required) {
				t.Errorf("non-YouTube args must contain %q:\n%s", required, joined)
			}
		}
		if !strings.Contains(joined, "Cookie:SID=secret; HSID=alsosecret") {
			t.Errorf("cookie header missing for a non-YouTube site:\n%s", joined)
		}
	})

	t.Run("resilience flags are always present", func(t *testing.T) {
		job := &Job{ID: "f", URL: "https://www.youtube.com/watch?v=abc", req: req}
		joined := strings.Join(mgr.ytDlpArgs(job, tools), " ")
		for _, required := range []string{"--ignore-errors", "--extractor-args youtubetab:skip=authcheck"} {
			if !strings.Contains(joined, required) {
				t.Errorf("expected %q in args:\n%s", required, joined)
			}
		}
	})
}
