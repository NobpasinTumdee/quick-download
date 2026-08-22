package downloader

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// write creates a file with some content and returns its path.
func write(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("some bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// verifyJob builds a job whose yt-dlp run has just "finished".
func verifyJob(dir, predicted string) *Job {
	j := &Job{ID: "v", URL: "https://example.com/x", engine: EngineYtDlp, dir: dir}
	j.finalPath = predicted
	return j
}

func TestVerifyOutputExactMatch(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(testConfig(t), nil)
	path := write(t, dir, "Clip.mp4")

	job := verifyJob(dir, path)
	if err := mgr.verifyYtDlpOutput(job, nil); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if got := job.Snapshot().Path; got != path {
		t.Errorf("path = %q, want %q", got, path)
	}
}

// The container can change under us: an mp4 merge that fails falls back to mkv.
func TestVerifyOutputFindsChangedExtension(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(testConfig(t), nil)
	actual := write(t, dir, "Clip.mkv")

	job := verifyJob(dir, filepath.Join(dir, "Clip.mp4"))
	if err := mgr.verifyYtDlpOutput(job, nil); err != nil {
		t.Fatalf("expected the .mkv to be found, got %v", err)
	}
	if got := job.Snapshot().Path; got != actual {
		t.Errorf("path was not adopted: got %q, want %q", got, actual)
	}
}

// --extract-audio replaces the downloaded file with a different container and
// deletes the original, so only the stem survives.
func TestVerifyOutputFindsExtractedAudio(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(testConfig(t), nil)
	actual := write(t, dir, "Podcast Episode.mp3")

	job := verifyJob(dir, filepath.Join(dir, "Podcast Episode.webm"))
	if err := mgr.verifyYtDlpOutput(job, nil); err != nil {
		t.Fatalf("expected the .mp3 to be found, got %v", err)
	}
	if got := job.Snapshot().Path; got != actual {
		t.Errorf("path = %q, want %q", got, actual)
	}
}

// The regression this whole file exists for: the default output template is
// "%(title)s [%(id)s].%(ext)s", so nearly every filename contains square
// brackets. Unquoted, "[abc123]" is a glob character class matching ONE of
// those characters, and the fallback silently finds nothing.
func TestVerifyOutputHandlesBracketsInFilenames(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(testConfig(t), nil)
	actual := write(t, dir, "Great Video [dQw4w9WgXcQ].mkv")

	job := verifyJob(dir, filepath.Join(dir, "Great Video [dQw4w9WgXcQ].mp4"))
	if err := mgr.verifyYtDlpOutput(job, nil); err != nil {
		t.Fatalf("brackets broke the stem search: %v", err)
	}
	if got := job.Snapshot().Path; got != actual {
		t.Errorf("path = %q, want %q", got, actual)
	}
}

func TestGlobQuote(t *testing.T) {
	// Each metacharacter becomes a single-character class, which is the only
	// form that works on Windows too (Glob disables backslash escaping there).
	cases := map[string]string{
		"Clip [abc]": "Clip [[]abc]",
		"a*b":        "a[*]b",
		"what?":      "what[?]",
		"plain name": "plain name",
		"[a]*b?[c]":  "[[]a][*]b[?][[]c]",
	}
	for in, want := range cases {
		if got := globQuote(in); got != want {
			t.Errorf("globQuote(%q) = %q, want %q", in, got, want)
		}
	}

	// And it must actually match through filepath.Glob.
	dir := t.TempDir()
	write(t, dir, "Great Video [dQw4w9WgXcQ].mp4")
	matches, err := filepath.Glob(filepath.Join(dir, globQuote("Great Video [dQw4w9WgXcQ]")+".*"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("glob returned %v, err %v", matches, err)
	}
}

// When only the per-track file was announced, the merged output drops the
// format id: "Clip.f137.mp4" is downloaded, "Clip.mp4" is kept.
func TestVerifyOutputStripsPerTrackSuffix(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(testConfig(t), nil)
	actual := write(t, dir, "Clip.mp4")

	job := verifyJob(dir, filepath.Join(dir, "Clip.f137.mp4"))
	if err := mgr.verifyYtDlpOutput(job, nil); err != nil {
		t.Fatalf("expected the merged file to be found, got %v", err)
	}
	if got := job.Snapshot().Path; got != actual {
		t.Errorf("path = %q, want %q", got, actual)
	}
}

func TestVerifyOutputRejectsPhantomCompletion(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(testConfig(t), nil)

	job := verifyJob(dir, filepath.Join(dir, "Missing.mp4"))
	err := mgr.verifyYtDlpOutput(job, nil)
	if err == nil {
		t.Fatal("expected a phantom completion error")
	}
	if !strings.Contains(err.Error(), "phantom completion") {
		t.Errorf("error should name the condition: %v", err)
	}
}

// A half-written file is not a completed download.
func TestVerifyOutputIgnoresPartialFiles(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(testConfig(t), nil)
	write(t, dir, "Clip.mp4.part")
	write(t, dir, "Clip.mp4.ytdl")

	job := verifyJob(dir, filepath.Join(dir, "Clip.mp4"))
	if err := mgr.verifyYtDlpOutput(job, nil); err == nil {
		t.Fatal("a .part file must not count as the finished download")
	}
}

func TestVerifyOutputIgnoresEmptyFiles(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(testConfig(t), nil)
	path := filepath.Join(dir, "Clip.mp4")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	job := verifyJob(dir, path)
	if err := mgr.verifyYtDlpOutput(job, nil); err == nil {
		t.Fatal("a zero-byte file must not count as a completed download")
	}
}

// No Destination line at all means yt-dlp never started a download.
func TestVerifyOutputWithNoPredictedPath(t *testing.T) {
	mgr := NewManager(testConfig(t), nil)
	job := verifyJob(t.TempDir(), "")
	err := mgr.verifyYtDlpOutput(job, nil)
	if err == nil || !strings.Contains(err.Error(), "never reported an output file") {
		t.Fatalf("expected a phantom error naming the cause, got %v", err)
	}
}

// The stderr tail carries yt-dlp's own diagnosis into the message the user sees.
func TestPhantomErrorCarriesStderr(t *testing.T) {
	tail := newRingBuffer(4)
	tail.add("[youtube:tab] Extracting URL")
	tail.add("ERROR: [youtube:tab] Unable to extract playlist")

	err := phantomError(tail, "expected Clip.mp4")
	if !strings.Contains(err.Error(), "Unable to extract playlist") {
		t.Errorf("stderr was not surfaced: %v", err)
	}
	if !strings.Contains(err.Error(), "phantom completion") {
		t.Errorf("condition was not named: %v", err)
	}
}

// --- end to end -------------------------------------------------------------

// A yt-dlp that announces a download, exits 0, and writes nothing at all - the
// exact shape --ignore-errors produces on a failed playlist extraction.
const fakeYtDlpPhantom = `package main

import (
	"fmt"
	"os"
)

func main() {
	dest := os.Getenv("FAKE_DEST")
	fmt.Println("[youtube:tab] Extracting URL")
	fmt.Println("[download] Destination: " + dest)
	fmt.Fprintln(os.Stderr, "ERROR: [youtube:tab] Unable to extract playlist; skipping")
	// --ignore-errors: report success despite having downloaded nothing.
	os.Exit(0)
}
`

// A yt-dlp that writes its output under a different extension than announced.
const fakeYtDlpRenames = `package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	dest := os.Getenv("FAKE_DEST")
	fmt.Println("[download] Destination: " + dest)
	actual := strings.TrimSuffix(dest, ".mp4") + ".mkv"
	_ = os.WriteFile(actual, []byte("video bytes"), 0o644)
	fmt.Println("[download] 100% of 11.00B")
}
`

func TestPhantomCompletionFailsTheJob(t *testing.T) {
	cfg := testConfig(t)
	dir := fakeYtDlp(t, fakeYtDlpPhantom)
	t.Setenv("QD_YTDLP", filepath.Join(dir, exeName("yt-dlp")))
	t.Setenv("QD_FFMPEG", filepath.Join(dir, exeName("ffmpeg")))
	t.Setenv("FAKE_DEST", filepath.Join(cfg.DownloadDir, "Nothing Here.mp4"))

	mgr := NewManager(cfg, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go mgr.Run(ctx)

	job, err := mgr.Enqueue(Request{URL: "https://www.youtube.com/watch?v=abc", Kind: "site"})
	if err != nil {
		t.Fatal(err)
	}

	snap := waitTerminal(t, job, 25*time.Second)
	if snap.State != StateFailed {
		t.Fatalf("state = %s, want failed (exit 0 with no file is not success)", snap.State)
	}
	if !strings.Contains(snap.Error, "phantom completion") {
		t.Errorf("error = %q, want it to name the phantom completion", snap.Error)
	}
	// The snapshot the WebSocket broadcasts must not claim 100%.
	if snap.Progress == 100 {
		t.Errorf("a failed job must not report 100%% progress")
	}
}

func TestRenamedOutputStillCompletes(t *testing.T) {
	cfg := testConfig(t)
	dir := fakeYtDlp(t, fakeYtDlpRenames)
	t.Setenv("QD_YTDLP", filepath.Join(dir, exeName("yt-dlp")))
	t.Setenv("QD_FFMPEG", filepath.Join(dir, exeName("ffmpeg")))
	t.Setenv("FAKE_DEST", filepath.Join(cfg.DownloadDir, "Renamed Clip.mp4"))

	mgr := NewManager(cfg, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go mgr.Run(ctx)

	job, err := mgr.Enqueue(Request{URL: "https://cdn.example.com/live/master.m3u8"})
	if err != nil {
		t.Fatal(err)
	}

	snap := waitTerminal(t, job, 25*time.Second)
	if snap.State != StateCompleted {
		t.Fatalf("state = %s, error = %s", snap.State, snap.Error)
	}
	if filepath.Ext(snap.Path) != ".mkv" {
		t.Errorf("path = %q, want the .mkv that was actually written", snap.Path)
	}
	if snap.Filename != "Renamed Clip.mkv" {
		t.Errorf("filename = %q, want %q", snap.Filename, "Renamed Clip.mkv")
	}
}
