package downloader

import (
	"context"
	"crypto/sha256"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/quick-download/backend/internal/config"
)

// testConfig points the engine at a temp folder with small chunks so the
// splitting logic actually runs on modest test payloads.
func testConfig(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	return &config.Config{
		Host:              "127.0.0.1",
		Port:              0,
		DownloadDir:       dir,
		TempDir:           filepath.Join(dir, ".parts"),
		LogDir:            dir,
		MaxChunks:         6,
		MinChunkSize:      4096,
		MaxConcurrentJobs: 2,
		MaxRetries:        2,
		UserAgent:         "test",
	}
}

// mediaServer serves a fixed body with full Range support, like a real CDN.
func mediaServer(t *testing.T, body []byte, opts serverOpts) *httptest.Server {
	t.Helper()
	var flaked bool
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if opts.noHead && r.Method == http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if !opts.noRanges {
			w.Header().Set("Accept-Ranges", "bytes")
		}
		w.Header().Set("Content-Type", "video/mp4")
		if opts.disposition != "" {
			w.Header().Set("Content-Disposition", opts.disposition)
		}

		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			w.WriteHeader(http.StatusOK)
			return
		}

		rangeHdr := r.Header.Get("Range")
		if rangeHdr == "" || opts.noRanges {
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
			return
		}

		var start, end int64
		if _, err := fmt.Sscanf(rangeHdr, "bytes=%d-%d", &start, &end); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if end >= int64(len(body)) {
			end = int64(len(body)) - 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
		w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
		w.WriteHeader(http.StatusPartialContent)

		chunk := body[start : end+1]
		// Simulate one mid-transfer connection drop to exercise chunk retry.
		if opts.flaky && !flaked && len(chunk) > 16 {
			flaked = true
			_, _ = w.Write(chunk[:len(chunk)/2])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			return
		}
		_, _ = w.Write(chunk)
	}))
}

type serverOpts struct {
	noHead      bool
	noRanges    bool
	flaky       bool
	disposition string
}

func randomBody(n int) []byte {
	b := make([]byte, n)
	rnd := rand.New(rand.NewSource(42))
	rnd.Read(b)
	return b
}

// runJob enqueues one URL and waits for a terminal state.
func runJob(t *testing.T, cfg *config.Config, req Request) JobSnapshot {
	t.Helper()
	mgr := NewManager(cfg, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go mgr.Run(ctx)

	job, err := mgr.Enqueue(req)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	deadline := time.Now().Add(25 * time.Second)
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

func TestChunkedDownloadMatchesSource(t *testing.T) {
	body := randomBody(300 * 1024) // 300 KiB over 4 KiB min chunks -> 6 chunks
	srv := mediaServer(t, body, serverOpts{})
	defer srv.Close()

	cfg := testConfig(t)
	snap := runJob(t, cfg, Request{URL: srv.URL + "/clip.mp4"})

	if snap.State != StateCompleted {
		t.Fatalf("state = %s, error = %s", snap.State, snap.Error)
	}
	if snap.Connections != cfg.MaxChunks {
		t.Errorf("used %d connections, expected %d", snap.Connections, cfg.MaxChunks)
	}
	if snap.Progress != 100 {
		t.Errorf("progress = %v, want 100", snap.Progress)
	}
	if snap.Filename != "clip.mp4" {
		t.Errorf("filename = %q, want clip.mp4", snap.Filename)
	}

	got, err := os.ReadFile(snap.Path)
	if err != nil {
		t.Fatalf("reading result: %v", err)
	}
	if sha256.Sum256(got) != sha256.Sum256(body) {
		t.Fatalf("downloaded content differs from source (%d vs %d bytes)", len(got), len(body))
	}

	// Every .part file must be gone once the merge succeeded.
	entries, _ := os.ReadDir(cfg.TempDir)
	if len(entries) != 0 {
		t.Errorf("temp dir still holds %d part files", len(entries))
	}
}

func TestRetriesRecoverFromTruncatedChunk(t *testing.T) {
	body := randomBody(120 * 1024)
	srv := mediaServer(t, body, serverOpts{flaky: true})
	defer srv.Close()

	snap := runJob(t, testConfig(t), Request{URL: srv.URL + "/flaky.mp4"})
	if snap.State != StateCompleted {
		t.Fatalf("state = %s, error = %s", snap.State, snap.Error)
	}
	got, err := os.ReadFile(snap.Path)
	if err != nil {
		t.Fatalf("reading result: %v", err)
	}
	if sha256.Sum256(got) != sha256.Sum256(body) {
		t.Fatal("content mismatch after retry")
	}
}

// A server that refuses HEAD must still work through the ranged-GET probe.
func TestProbeFallsBackWhenHeadIsRejected(t *testing.T) {
	body := randomBody(64 * 1024)
	srv := mediaServer(t, body, serverOpts{noHead: true})
	defer srv.Close()

	snap := runJob(t, testConfig(t), Request{URL: srv.URL + "/nohead.mp4"})
	if snap.State != StateCompleted {
		t.Fatalf("state = %s, error = %s", snap.State, snap.Error)
	}
	if snap.Size != int64(len(body)) {
		t.Errorf("size = %d, want %d", snap.Size, len(body))
	}
}

// No Range support at all: we must fall back to a single stream, not fail.
func TestSingleStreamFallback(t *testing.T) {
	body := randomBody(50 * 1024)
	srv := mediaServer(t, body, serverOpts{noRanges: true})
	defer srv.Close()

	snap := runJob(t, testConfig(t), Request{URL: srv.URL + "/plain.mp4"})
	if snap.State != StateCompleted {
		t.Fatalf("state = %s, error = %s", snap.State, snap.Error)
	}
	if snap.Connections != 1 {
		t.Errorf("connections = %d, want 1", snap.Connections)
	}
	got, _ := os.ReadFile(snap.Path)
	if len(got) != len(body) {
		t.Fatalf("size = %d, want %d", len(got), len(body))
	}
}

func TestContentDispositionWins(t *testing.T) {
	body := randomBody(20 * 1024)
	srv := mediaServer(t, body, serverOpts{disposition: `attachment; filename="Holiday Clip.mp4"`})
	defer srv.Close()

	snap := runJob(t, testConfig(t), Request{URL: srv.URL + "/x?id=9"})
	if snap.Filename != "Holiday Clip.mp4" {
		t.Fatalf("filename = %q, want %q", snap.Filename, "Holiday Clip.mp4")
	}
}

func TestPlanChunksCoversWholeFileWithoutGaps(t *testing.T) {
	cfg := testConfig(t)
	mgr := NewManager(cfg, nil)

	for _, size := range []int64{4096, 10000, 1 << 20, 1<<20 + 7, 123456789} {
		chunks := mgr.planChunks(size)
		if len(chunks) == 0 || len(chunks) > cfg.MaxChunks {
			t.Fatalf("size %d: got %d chunks", size, len(chunks))
		}
		if chunks[0].Start != 0 {
			t.Fatalf("size %d: first chunk starts at %d", size, chunks[0].Start)
		}
		if last := chunks[len(chunks)-1]; last.End != size-1 {
			t.Fatalf("size %d: last chunk ends at %d, want %d", size, last.End, size-1)
		}
		var total int64
		for i, c := range chunks {
			if c.Size() <= 0 {
				t.Fatalf("size %d: chunk %d is empty", size, i)
			}
			if i > 0 && c.Start != chunks[i-1].End+1 {
				t.Fatalf("size %d: gap or overlap between chunk %d and %d", size, i-1, i)
			}
			total += c.Size()
		}
		if total != size {
			t.Fatalf("size %d: chunks cover %d bytes", size, total)
		}
	}
}

func TestSanitizeFilename(t *testing.T) {
	cases := map[string]string{
		// Path separators become underscores and leading dots are trimmed, so
		// a traversal attempt can never escape the download directory.
		`../../etc/passwd`: "_.._etc_passwd",
		// A query string is cut off: URL basenames routinely carry one.
		`clip.mp4?token=abc`: "clip.mp4",
		// Everything from the first "?" on is dropped, then the remaining
		// Windows-illegal characters are replaced.
		`bad<>:"|?*name.mp4`: "bad_____",
		`CON.mp4`:            "_CON.mp4",
		`  spaced.mp4  `:     "spaced.mp4",
	}
	for in, want := range cases {
		got := sanitizeFilename(in)
		if got != want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", in, got, want)
		}
		if strings.ContainsAny(got, `/\`) {
			t.Errorf("sanitizeFilename(%q) leaked a path separator: %q", in, got)
		}
	}
}

func TestUniquePathAvoidsOverwrite(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.mp4"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := uniquePath(dir, "a.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != "a (1).mp4" {
		t.Fatalf("got %q, want %q", filepath.Base(got), "a (1).mp4")
	}
}

func TestCancelStopsJob(t *testing.T) {
	// A server that dribbles bytes forever gives us time to cancel.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", "104857600")
		w.WriteHeader(http.StatusOK)
		for i := 0; i < 1000; i++ {
			// Stop as soon as the engine cancels, otherwise httptest.Close
			// blocks waiting for this handler to return.
			select {
			case <-r.Context().Done():
				return
			default:
			}
			if _, err := w.Write(make([]byte, 1024)); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(10 * time.Millisecond)
		}
	}))
	defer srv.Close()

	cfg := testConfig(t)
	mgr := NewManager(cfg, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Run(ctx)

	job, err := mgr.Enqueue(Request{URL: srv.URL + "/slow.mp4"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	mgr.Cancel(job.ID)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if job.Snapshot().State == StateCanceled {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job never reached canceled state (got %s)", job.Snapshot().State)
}
