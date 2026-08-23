package downloader

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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

func TestWildcardEqual(t *testing.T) {
	// "?" is what Python writes for a character the console code page cannot
	// encode - one "?" per lost character, so the lengths still line up.
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"Caf? Clip", "Caf\u00e9 Clip", true},
		{"??? ??? [abc]", "\u0e2a\u0e27\u0e31 \u0e2a\u0e14\u0e35 [abc]", true},
		{"Clip", "clip", true},
		{"Caf? Clip", "Cafe Clip Extra", false},
		{"Caf? Clip", "Caf\u00e9 Movie", false},
	}
	for _, c := range cases {
		if got := wildcardEqual(c.pattern, c.name); got != c.want {
			t.Errorf("wildcardEqual(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}

func TestFoldName(t *testing.T) {
	if foldName("My Video: Part 1 (HD)") != "myvideopart1hd" {
		t.Errorf("foldName dropped the wrong characters: %q", foldName("My Video: Part 1 (HD)"))
	}
}

func TestOutputIDs(t *testing.T) {
	ids := outputIDs([]string{"Great Video [dQw4w9WgXcQ]", "Great Video [dQw4w9WgXcQ].f137"}, "")
	if len(ids) != 1 || ids[0] != "dQw4w9WgXcQ" {
		t.Fatalf("ids = %v, want one id with the per-track suffix stripped", ids)
	}
	// With nothing usable announced, the page URL still carries the id.
	fromURL := outputIDs(nil, "https://www.youtube.com/watch?v=abcdef12345&t=30")
	if len(fromURL) != 1 || fromURL[0] != "abcdef12345" {
		t.Fatalf("ids from URL = %v", fromURL)
	}
}

func TestNormalizePath(t *testing.T) {
	// An absolute directory on any platform: on Windows a leading separator
	// without a drive letter is not absolute, and would be joined twice.
	dir := filepath.Join(t.TempDir(), "media")
	// A bare filename is relative to the job directory, quotes are noise, and
	// the separators must come back native so name comparisons line up.
	if got := normalizePath(dir, `  "Clip.mp4"  `); got != filepath.Join(dir, "Clip.mp4") {
		t.Errorf("normalizePath = %q", got)
	}
	if got := normalizePath(dir, filepath.Join(dir, "sub", "..", "Clip.mp4")); got != filepath.Join(dir, "Clip.mp4") {
		t.Errorf("normalizePath did not clean: %q", got)
	}
	if got := normalizePath(dir, "   "); got != "" {
		t.Errorf("blank path = %q, want empty", got)
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

// A yt-dlp whose stdout could not encode the title: it writes the real file but
// announces a name full of "?" - the shape of the false "failed" this ladder
// exists to prevent.
const fakeYtDlpMangledName = `package main

import (
	"fmt"
	"os"
	"strings"
	"unicode/utf8"
)

func main() {
	dest := os.Getenv("FAKE_DEST")
	_ = os.WriteFile(dest, []byte("real video bytes"), 0o644)

	// Replace every non-ASCII rune with "?", exactly as Python does when the
	// console code page cannot represent it.
	var b strings.Builder
	for _, r := range dest {
		if r < utf8.RuneSelf {
			b.WriteRune(r)
		} else {
			b.WriteByte('?')
		}
	}
	fmt.Println("[download] Destination: " + b.String())
	fmt.Println("[download] 100% of 16.00B")
}
`

func TestMangledDestinationStillCompletes(t *testing.T) {
	cfg := testConfig(t)
	dir := fakeYtDlp(t, fakeYtDlpMangledName)
	t.Setenv("QD_YTDLP", filepath.Join(dir, exeName("yt-dlp")))
	t.Setenv("QD_FFMPEG", filepath.Join(dir, exeName("ffmpeg")))
	actual := filepath.Join(cfg.DownloadDir, "สวัสดีครับ [dQw4w9WgXcQ].mp4")
	t.Setenv("FAKE_DEST", actual)

	mgr := NewManager(cfg, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go mgr.Run(ctx)

	job, err := mgr.Enqueue(Request{URL: "https://www.youtube.com/watch?v=dQw4w9WgXcQ", Kind: "site"})
	if err != nil {
		t.Fatal(err)
	}

	snap := waitTerminal(t, job, 25*time.Second)
	if snap.State != StateCompleted {
		t.Fatalf("state = %s, error = %s (a playable file was written)", snap.State, snap.Error)
	}
	if snap.Path != actual {
		t.Errorf("path = %q, want the file that was actually written %q", snap.Path, actual)
	}
}

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

// --- resolution ladder -------------------------------------------------------

// The regression that started this: yt-dlp's stdout goes through the console
// code page, so a title it cannot encode arrives as "?" and the path we parsed
// names a file that cannot exist. The id in brackets survives that.
func TestVerifyOutputResolvesByVideoID(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(testConfig(t), nil)
	actual := write(t, dir, "\u0e2a\u0e27\u0e31\u0e2a\u0e14\u0e35\u0e04\u0e23\u0e31\u0e1a [dQw4w9WgXcQ].mkv")

	// Different length AND different extension: only the id can find this.
	job := verifyJob(dir, filepath.Join(dir, "?????? ????? [dQw4w9WgXcQ].mp4"))
	if err := mgr.verifyYtDlpOutput(job, nil); err != nil {
		t.Fatalf("the id fallback did not find the file: %v", err)
	}
	if got := job.Snapshot().Path; got != actual {
		t.Errorf("path = %q, want %q", got, actual)
	}
}

// The id also comes from the page URL when yt-dlp announced nothing usable.
func TestVerifyOutputResolvesByIDFromURL(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(testConfig(t), nil)
	actual := write(t, dir, "Some Title [dQw4w9WgXcQ].webm")

	job := verifyJob(dir, filepath.Join(dir, "Totally Other Name.mp4"))
	job.URL = "https://www.youtube.com/watch?v=dQw4w9WgXcQ"
	if err := mgr.verifyYtDlpOutput(job, nil); err != nil {
		t.Fatalf("the URL id fallback did not find the file: %v", err)
	}
	if got := job.Snapshot().Path; got != actual {
		t.Errorf("path = %q, want %q", got, actual)
	}
}

// A custom filename has no id in it, so a lossy name falls to the wildcard rung.
func TestVerifyOutputResolvesEncodingLossyName(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(testConfig(t), nil)
	actual := write(t, dir, "Caf\u00e9 Session.mp4")

	job := verifyJob(dir, filepath.Join(dir, "Caf? Session.mp4"))
	if err := mgr.verifyYtDlpOutput(job, nil); err != nil {
		t.Fatalf("the wildcard rung did not find the file: %v", err)
	}
	if got := job.Snapshot().Path; got != actual {
		t.Errorf("path = %q, want %q", got, actual)
	}
}

// "%(title).150B" truncates, and sanitising drops punctuation, so the two names
// only share a prefix once folded.
func TestVerifyOutputResolvesTruncatedTitle(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(testConfig(t), nil)
	actual := write(t, dir, "A Very Long Title About Something Interesting Indeed.mkv")

	job := verifyJob(dir, filepath.Join(dir, "A Very Long Title About Some.mp4"))
	if err := mgr.verifyYtDlpOutput(job, nil); err != nil {
		t.Fatalf("the fuzzy rung did not find the file: %v", err)
	}
	if got := job.Snapshot().Path; got != actual {
		t.Errorf("path = %q, want %q", got, actual)
	}
}

// A short shared prefix is not evidence: "Clip 1.mp4" must not adopt "Clip 2".
func TestVerifyOutputFuzzyNeedsEnoughSignal(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(testConfig(t), nil)
	write(t, dir, "Other.txt")

	job := verifyJob(dir, filepath.Join(dir, "Clip 1.mp4"))
	if err := mgr.verifyYtDlpOutput(job, nil); err == nil {
		t.Fatal("an unrelated file must not be adopted")
	}
}

// The merged file wins over the per-track parts when -k kept them all.
func TestVerifyOutputPrefersMergedOverPerTrack(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(testConfig(t), nil)
	write(t, dir, "Clip [abc123].f137.mp4")
	write(t, dir, "Clip [abc123].f140.m4a")
	merged := write(t, dir, "Clip [abc123].mkv")

	job := verifyJob(dir, filepath.Join(dir, "Clip [abc123].mp4"))
	if err := mgr.verifyYtDlpOutput(job, nil); err != nil {
		t.Fatalf("expected the merged file, got %v", err)
	}
	if got := job.Snapshot().Path; got != merged {
		t.Errorf("path = %q, want the merged %q", got, merged)
	}
}

// Last resort: nothing about the name matches, but exactly one media file
// appeared in the job directory while the run was going on.
func TestVerifyOutputAdoptsTheFileWrittenDuringTheRun(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(testConfig(t), nil)
	job := verifyJob(dir, filepath.Join(dir, "Nothing Like It.mp4"))
	job.startedAt = time.Now()
	actual := write(t, dir, "wholly-different-name.mkv")
	// A non-media sibling must not confuse it.
	write(t, dir, "wholly-different-name.info.json")

	if err := mgr.verifyYtDlpOutput(job, nil); err != nil {
		t.Fatalf("expected the fresh file to be adopted: %v", err)
	}
	if got := job.Snapshot().Path; got != actual {
		t.Errorf("path = %q, want %q", got, actual)
	}
}

// ...but only when it is unambiguous. Two candidates could just as easily be
// the browser's own downloads, and adopting the wrong one is worse than failing.
func TestVerifyOutputWillNotGuessBetweenTwoFreshFiles(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(testConfig(t), nil)
	job := verifyJob(dir, filepath.Join(dir, "Nothing Like It.mp4"))
	job.startedAt = time.Now()
	write(t, dir, "one-random-file.mkv")
	write(t, dir, "another-random-file.webm")

	if err := mgr.verifyYtDlpOutput(job, nil); err == nil {
		t.Fatal("an ambiguous directory must not be guessed at")
	}
}

// A file that was already there before the run started is not our download.
func TestVerifyOutputIgnoresPreexistingFiles(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(testConfig(t), nil)
	write(t, dir, "unrelated-old-video.mkv")

	job := verifyJob(dir, filepath.Join(dir, "Nothing Like It.mp4"))
	job.startedAt = time.Now().Add(2 * time.Hour) // the run began well after
	if err := mgr.verifyYtDlpOutput(job, nil); err == nil {
		t.Fatal("a file predating the run must not be adopted")
	}
}

// Every path yt-dlp mentions is remembered, so a name that was superseded for
// display can still be the one that exists on disk.
func TestVerifyOutputUsesAnEarlierAnnouncedName(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(testConfig(t), nil)
	actual := write(t, dir, "Real Clip [abc123].webm")

	job := verifyJob(dir, "")
	job.dir = dir
	// The download wrote the .webm; the merger then announced an mp4 it never
	// managed to produce.
	job.setFinalPath(filepath.Join(dir, "Real Clip [abc123].webm"), false)
	job.setFinalPath(filepath.Join(dir, "Merged Elsewhere.mp4"), true)

	if err := mgr.verifyYtDlpOutput(job, nil); err != nil {
		t.Fatalf("the earlier name was forgotten: %v", err)
	}
	if got := job.Snapshot().Path; got != actual {
		t.Errorf("path = %q, want %q", got, actual)
	}
}

// A post-processor that moves the file out of the job directory still has its
// destination searched.
func TestVerifyOutputSearchesAnnouncedDirectories(t *testing.T) {
	jobDir := t.TempDir()
	other := t.TempDir()
	mgr := NewManager(testConfig(t), nil)
	actual := write(t, other, "Moved Clip [abc123].mkv")

	job := verifyJob(jobDir, filepath.Join(other, "Moved Clip [abc123].mp4"))
	if err := mgr.verifyYtDlpOutput(job, nil); err != nil {
		t.Fatalf("the announced directory was not searched: %v", err)
	}
	if got := job.Snapshot().Path; got != actual {
		t.Errorf("path = %q, want %q", got, actual)
	}
}

// --- stdout parsing ----------------------------------------------------------

func TestMoveFilesAndFixupLinesSetTheFinalPath(t *testing.T) {
	mgr := NewManager(testConfig(t), nil)
	dir := t.TempDir()

	job := verifyJob(dir, "")
	job.dir = dir
	mgr.consumeYtDlpLine(job, `[download] Destination: `+filepath.Join(dir, "Clip [abc].f137.mp4"))
	mgr.consumeYtDlpLine(job, `[Merger] Merging formats into "`+filepath.Join(dir, "Clip [abc].mp4")+`"`)
	mgr.consumeYtDlpLine(job, `[MoveFiles] Moving file "`+filepath.Join(dir, "Clip [abc].mp4")+`" to "`+filepath.Join(dir, "final", "Clip [abc].mp4")+`"`)
	if got := job.Snapshot().Path; got != filepath.Join(dir, "final", "Clip [abc].mp4") {
		t.Errorf("MoveFiles destination ignored: %q", got)
	}

	mgr.consumeYtDlpLine(job, `[FixupM3u8] Fixing MPEG-TS in MP4 container of "`+filepath.Join(dir, "Fixed [abc].mp4")+`"`)
	if got := job.Snapshot().Path; got != filepath.Join(dir, "Fixed [abc].mp4") {
		t.Errorf("Fixup destination ignored: %q", got)
	}
}

// Any tag other than [download] is a post-processor, so new ones are picked up
// without having to be listed here first.
func TestUnknownPostProcessorDestinationWins(t *testing.T) {
	mgr := NewManager(testConfig(t), nil)
	dir := t.TempDir()

	job := verifyJob(dir, "")
	job.dir = dir
	mgr.consumeYtDlpLine(job, `[download] Destination: `+filepath.Join(dir, "Clip.webm"))
	mgr.consumeYtDlpLine(job, `[SomeFuturePostProcessor] Destination: `+filepath.Join(dir, "Clip.opus"))
	if got := job.Snapshot().Path; got != filepath.Join(dir, "Clip.opus") {
		t.Errorf("path = %q, want the post-processor destination", got)
	}
}

// Paths are canonicalised as they are parsed, so everything downstream - the
// popup's filename, the verification below - deals in one spelling.
func TestAnnouncedPathsAreNormalized(t *testing.T) {
	mgr := NewManager(testConfig(t), nil)
	dir := t.TempDir()

	job := verifyJob(dir, "")
	job.dir = dir
	mgr.consumeYtDlpLine(job, "[download] Destination: Clip.mp4") // bare, relative
	if got := job.Snapshot().Path; got != filepath.Join(dir, "Clip.mp4") {
		t.Errorf("relative destination = %q, want it resolved against the job dir", got)
	}
	if got := job.Snapshot().Filename; got != "Clip.mp4" {
		t.Errorf("filename = %q", got)
	}
}

// --- lossy code pages --------------------------------------------------------
//
// The corruption these cover is not hypothetical: on a cp874 (Thai) Windows,
// yt-dlp.exe prints "\u7c73\u6d25\u7384\u5e2b IRIS OUT \u7b2c76\u56de.mp4" as " IRIS OUT 76.mp4" -
// every Japanese character silently deleted, because it encodes its output with
// the locale code page and errors="ignore". The file on disk is fine; only the
// name we were told is wrong.

func TestAsciiFold(t *testing.T) {
	// The whole point: a name and its code-page-mangled shadow fold alike.
	full := "\u7c73\u6d25\u7384\u5e2b IRIS OUT \uff08\u7b2c76\u56deNHK\uff09"
	dropped := " IRIS OUT 76NHK"            // errors="ignore" deletes what it cannot encode
	replaced := "???? IRIS OUT ??76??NHK??" // errors="replace" substitutes instead
	if asciiFold(full) != asciiFold(dropped) || asciiFold(full) != asciiFold(replaced) {
		t.Fatalf("folds differ: %q / %q / %q", asciiFold(full), asciiFold(dropped), asciiFold(replaced))
	}
	if asciiFold(full) != "irisout76nhk" {
		t.Errorf("asciiFold = %q", asciiFold(full))
	}
	// It must not fold two genuinely different titles together.
	if asciiFold("Episode 1") == asciiFold("Episode 2") {
		t.Error("asciiFold lost the part that distinguishes the files")
	}
}

func TestWildcardEqualAcceptsReplacementChar(t *testing.T) {
	if !wildcardEqual("\uFFFD\uFFFD Clip", "\u3053\u3093 Clip") {
		t.Error("U+FFFD must stand in for a lost character, like ?")
	}
}

// The reported failure, resolved by the ASCII that survived the code page.
func TestVerifyOutputResolvesDroppedNonASCII(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(testConfig(t), nil)
	actual := write(t, dir, "\u7c73\u6d25\u7384\u5e2b IRIS OUT \u7b2c76\u56de Kenshi Yonezu - YouTube.mp4")

	// What yt-dlp printed on a cp874 console: the Japanese is simply gone, so
	// neither the length nor a Unicode-aware fold can line the two names up.
	job := verifyJob(dir, filepath.Join(dir, " IRIS OUT 76 Kenshi Yonezu - YouTube.mp4"))
	if err := mgr.verifyYtDlpOutput(job, nil); err != nil {
		t.Fatalf("a real download was called a phantom: %v", err)
	}
	if got := job.Snapshot().Path; got != actual {
		t.Errorf("path = %q, want %q", got, actual)
	}
}

// A title with no ASCII in it at all leaves nothing to fold, so the time window
// is the only thing left - and it has to work.
func TestVerifyOutputResolvesFullyNonASCIITitle(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(testConfig(t), nil)
	job := verifyJob(dir, filepath.Join(dir, ".mp4")) // everything was dropped
	job.startedAt = time.Now()
	actual := write(t, dir, "\u3053\u3093\u306b\u3061\u306f\u4e16\u754c.mkv")

	if err := mgr.verifyYtDlpOutput(job, nil); err != nil {
		t.Fatalf("the time window did not adopt the file: %v", err)
	}
	if got := job.Snapshot().Path; got != actual {
		t.Errorf("path = %q, want %q", got, actual)
	}
}

// Leftover per-track files must not make the time window "ambiguous": they are
// not the download, and refusing to choose would fail a good job.
func TestVerifyOutputWindowIgnoresPerTrackLeftovers(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(testConfig(t), nil)
	job := verifyJob(dir, filepath.Join(dir, ".mp4"))
	job.startedAt = time.Now()
	write(t, dir, "\u3053\u3093\u306b\u3061\u306f.f137.mp4")
	write(t, dir, "\u3053\u3093\u306b\u3061\u306f.f140.m4a")
	actual := write(t, dir, "\u3053\u3093\u306b\u3061\u306f.mkv")

	if err := mgr.verifyYtDlpOutput(job, nil); err != nil {
		t.Fatalf("per-track leftovers blocked the adoption: %v", err)
	}
	if got := job.Snapshot().Path; got != actual {
		t.Errorf("path = %q, want the merged %q", got, actual)
	}
}

// A .part file is not a download, however fresh it is.
func TestVerifyOutputWindowSkipsTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(testConfig(t), nil)
	job := verifyJob(dir, filepath.Join(dir, ".mp4"))
	job.startedAt = time.Now()
	write(t, dir, "half-written.mp4.part")
	write(t, dir, "half-written.mp4.ytdl")
	write(t, dir, "scratch.temp")

	if err := mgr.verifyYtDlpOutput(job, nil); err == nil {
		t.Fatal("a .part file must never be adopted as the finished download")
	}
}

// --- the fix at the source ---------------------------------------------------

func TestYtDlpForcesUTF8Output(t *testing.T) {
	cfg := testConfig(t)
	mgr := NewManager(cfg, nil)
	job := &Job{ID: "e", URL: "https://www.youtube.com/watch?v=abc", dir: cfg.DownloadDir}
	args := mgr.ytDlpArgs(job, cfg.Tools())

	for i, a := range args {
		if a == "--encoding" && i+1 < len(args) && args[i+1] == "utf-8" {
			return
		}
	}
	t.Fatalf("--encoding utf-8 is missing; without it a non-ASCII title is\n"+
		"destroyed on its way through the pipe:\n%s", strings.Join(args, " "))
}

// A byte-limited truncation must not slice a multi-byte character in half: the
// broken tail becomes U+FFFD in the command line we hand to yt-dlp, and names a
// file nothing will ever match.
func TestSanitizeFilenameTruncatesOnRuneBoundaries(t *testing.T) {
	long := strings.Repeat("\u3042", 200) + ".mp4" // 600+ bytes of hiragana
	got := sanitizeFilename(long)
	if !utf8.ValidString(got) {
		t.Fatalf("truncation produced invalid UTF-8: %q", got)
	}
	if len(got) > 180 {
		t.Errorf("length = %d bytes, want <= 180", len(got))
	}
	if !strings.HasSuffix(got, ".mp4") {
		t.Errorf("extension was lost: %q", got)
	}
}

// --- end to end --------------------------------------------------------------

// A yt-dlp that prints through a code page which cannot spell the title: the
// file it writes is correct, the name it announces has had every non-ASCII
// character deleted. This is the exact failure users reported.
const fakeYtDlpLossyEncoding = `package main

import (
	"fmt"
	"os"
	"strings"
	"unicode/utf8"
)

func main() {
	dest := os.Getenv("FAKE_DEST")
	_ = os.WriteFile(dest, []byte("real video bytes"), 0o644)

	// errors="ignore": characters the code page cannot represent are dropped.
	var b strings.Builder
	for _, r := range dest {
		if r < utf8.RuneSelf {
			b.WriteRune(r)
		}
	}
	fmt.Println("[download] Destination: " + b.String())
	fmt.Println("[download] 100% of 16.00B")
}
`

func TestLossyEncodedNameStillCompletes(t *testing.T) {
	cfg := testConfig(t)
	dir := fakeYtDlp(t, fakeYtDlpLossyEncoding)
	t.Setenv("QD_YTDLP", filepath.Join(dir, exeName("yt-dlp")))
	t.Setenv("QD_FFMPEG", filepath.Join(dir, exeName("ffmpeg")))
	actual := filepath.Join(cfg.DownloadDir, "\u7c73\u6d25\u7384\u5e2b IRIS OUT \u7b2c76\u56de Kenshi Yonezu.mp4")
	t.Setenv("FAKE_DEST", actual)

	mgr := NewManager(cfg, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go mgr.Run(ctx)

	job, err := mgr.Enqueue(Request{URL: "https://www.youtube.com/watch?v=abc", Kind: "site"})
	if err != nil {
		t.Fatal(err)
	}

	snap := waitTerminal(t, job, 25*time.Second)
	if snap.State != StateCompleted {
		t.Fatalf("state = %s, error = %s (the file was written and is playable)", snap.State, snap.Error)
	}
	if snap.Path != actual {
		t.Errorf("path = %q, want %q", snap.Path, actual)
	}
}

// The failure exactly as it was reported, reproduced from the real filename on
// disk by encoding it the way yt-dlp did: cp874 with errors="ignore".
//
// Two things happened to that name at once. Every Japanese character was
// deleted, and the en dash came through as a byte that is not valid UTF-8,
// which reaches us as U+FFFD. Neither the length nor a Unicode-aware fold can
// recover from that; the surviving ASCII can.
func TestVerifyOutputResolvesTheReportedJapaneseTitle(t *testing.T) {
	const realName = "\u7c73\u6d25\u7384\u5e2b \u2013 IRIS OUT\u300c\u7b2c76\u56deNHK\u7d05\u767d\u6b4c\u5408\u6226\u300d\u3000Kenshi Yonezu \u2013 IRIS OUT (76th NHK Kouhaku Performance) - YouTube.mp4"

	for _, announced := range []string{
		" \u2013 IRIS OUT76NHKKenshi Yonezu \u2013 IRIS OUT (76th NHK Kouhaku Performance) - YouTube.mp4",
		" \uFFFD IRIS OUT76NHKKenshi Yonezu \uFFFD IRIS OUT (76th NHK Kouhaku Performance) - YouTube.mp4",
	} {
		dir := t.TempDir()
		mgr := NewManager(testConfig(t), nil)
		actual := write(t, dir, realName)

		job := verifyJob(dir, filepath.Join(dir, announced))
		if err := mgr.verifyYtDlpOutput(job, nil); err != nil {
			t.Fatalf("announced %q was called a phantom: %v", announced, err)
		}
		if got := job.Snapshot().Path; got != actual {
			t.Errorf("path = %q, want %q", got, actual)
		}
	}
}
