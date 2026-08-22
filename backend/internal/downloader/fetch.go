package downloader

import (
	"context"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// execute drives one job from probe to finished file.
func (m *Manager) execute(parent context.Context, job *Job) {
	// Every network read of this job hangs off this context, so a single
	// cancel() aborts all chunk goroutines at once.
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	job.setCancel(cancel)

	defer func() {
		// A panic inside a worker must never take the daemon (and therefore
		// the stdin/stdout pipe of the host) down with it.
		if r := recover(); r != nil {
			log.Printf("job %s panicked: %v", job.ID, r)
			job.fail(fmt.Errorf("internal error: %v", r))
		}
	}()

	// Streaming manifests and extractor-only sites go to yt-dlp; everything
	// else uses the chunked HTTP engine below.
	job.mu.RLock()
	kind, engine := job.kind, job.engine
	job.mu.RUnlock()

	if engine == EngineYtDlp {
		job.setState(StateDownloading)
		m.Broadcast()

		if err := m.runYtDlp(ctx, job, kind); err != nil {
			if ctx.Err() != nil || job.State() == StateCanceled {
				job.setState(StateCanceled)
				return
			}
			job.fail(err)
			return
		}
		job.setState(StateCompleted)
		job.mu.RLock()
		out := job.finalPath
		job.mu.RUnlock()
		log.Printf("job %s completed via yt-dlp -> %s", job.ID, out)
		return
	}

	job.setState(StateProbing)
	m.Broadcast()

	info, err := m.probe(ctx, job)
	if err != nil {
		job.fail(err)
		return
	}

	job.mu.Lock()
	job.size = info.size
	job.resumable = info.resumable
	job.mime = info.mime
	if job.filename == "" {
		job.filename = info.filename
	}
	job.mu.Unlock()

	finalPath, err := uniquePath(m.cfg.DownloadDir, job.filenameOrDefault())
	if err != nil {
		job.fail(err)
		return
	}
	job.mu.Lock()
	job.finalPath = finalPath
	job.filename = filepath.Base(finalPath)
	job.mu.Unlock()

	// Decide the strategy: parallel ranges only pay off when the server
	// advertises byte ranges AND the file is big enough to split.
	parallel := info.resumable && info.size >= m.cfg.MinChunkSize*2

	job.setState(StateDownloading)
	m.Broadcast()

	if parallel {
		err = m.downloadChunked(ctx, job, info.size)
	} else {
		err = m.downloadSingle(ctx, job, info.size)
	}

	if err != nil {
		if ctx.Err() != nil || job.State() == StateCanceled {
			job.setState(StateCanceled)
			m.cleanupParts(job)
			return
		}
		job.fail(err)
		m.cleanupParts(job)
		return
	}

	job.setState(StateCompleted)
	log.Printf("job %s completed -> %s", job.ID, finalPath)
}

// ---------------------------------------------------------------------------
// Probing
// ---------------------------------------------------------------------------

type probeInfo struct {
	size      int64
	resumable bool
	filename  string
	mime      string
}

// probe discovers the size, the range support and the target file name.
// It tries HEAD first and falls back to a one-byte ranged GET, because plenty
// of CDNs answer HEAD with 403/405 while happily serving ranged GETs.
func (m *Manager) probe(ctx context.Context, job *Job) (probeInfo, error) {
	headCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	info := probeInfo{size: -1}

	req, err := m.newRequest(headCtx, http.MethodHead, job)
	if err != nil {
		return info, err
	}
	resp, err := m.client.Do(req)
	if err == nil {
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			info = readProbeHeaders(resp, job.URL)
		}
		drainAndClose(resp.Body)
		if info.size > 0 && info.resumable {
			return info, nil
		}
	}

	// Fallback: ask for the very first byte. A compliant server answers 206
	// with "Content-Range: bytes 0-0/<total>", which gives us both facts at once.
	getCtx, cancel2 := context.WithTimeout(ctx, 20*time.Second)
	defer cancel2()
	req2, err := m.newRequest(getCtx, http.MethodGet, job)
	if err != nil {
		return info, err
	}
	req2.Header.Set("Range", "bytes=0-0")
	resp2, err := m.client.Do(req2)
	if err != nil {
		return info, fmt.Errorf("cannot reach server: %w", err)
	}
	defer drainAndClose(resp2.Body)

	if resp2.StatusCode >= 400 {
		return info, fmt.Errorf("server replied %s", resp2.Status)
	}

	fallback := readProbeHeaders(resp2, job.URL)
	if resp2.StatusCode == http.StatusPartialContent {
		if total := parseContentRangeTotal(resp2.Header.Get("Content-Range")); total > 0 {
			fallback.size = total
			fallback.resumable = true
		}
	} else {
		// 200 in reply to a ranged request means "I ignore Range" -> single stream.
		fallback.resumable = false
	}
	if fallback.filename == "" {
		fallback.filename = info.filename
	}
	return fallback, nil
}

func readProbeHeaders(resp *http.Response, rawURL string) probeInfo {
	info := probeInfo{size: -1}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil {
			info.size = n
		}
	}
	info.resumable = strings.Contains(strings.ToLower(resp.Header.Get("Accept-Ranges")), "bytes")
	info.mime = strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0])
	info.filename = filenameFrom(resp, rawURL, info.mime)
	return info
}

var contentRangeRe = regexp.MustCompile(`/\s*(\d+)\s*$`)

func parseContentRangeTotal(v string) int64 {
	mm := contentRangeRe.FindStringSubmatch(v)
	if len(mm) != 2 {
		return -1
	}
	n, err := strconv.ParseInt(mm[1], 10, 64)
	if err != nil {
		return -1
	}
	return n
}

// ---------------------------------------------------------------------------
// Parallel (chunked) download
// ---------------------------------------------------------------------------

// planChunks splits [0,size) into as many ranges as make sense: never more
// than MaxChunks, never smaller than MinChunkSize.
func (m *Manager) planChunks(size int64) []*Chunk {
	n := int(size / m.cfg.MinChunkSize)
	if n > m.cfg.MaxChunks {
		n = m.cfg.MaxChunks
	}
	if n < 1 {
		n = 1
	}

	chunks := make([]*Chunk, 0, n)
	per := size / int64(n)
	var start int64
	for i := 0; i < n; i++ {
		end := start + per - 1
		if i == n-1 {
			end = size - 1 // last chunk absorbs the rounding remainder
		}
		chunks = append(chunks, &Chunk{Index: i, Start: start, End: end})
		start = end + 1
	}
	return chunks
}

// downloadChunked fans out one goroutine per byte range, waits for all of them
// and then concatenates the .part files in order.
func (m *Manager) downloadChunked(ctx context.Context, job *Job, size int64) error {
	chunks := m.planChunks(size)
	job.mu.Lock()
	job.chunks = chunks
	job.mu.Unlock()

	if err := os.MkdirAll(m.cfg.TempDir, 0o755); err != nil {
		return fmt.Errorf("cannot create temp dir: %w", err)
	}

	var (
		wg   sync.WaitGroup
		errs = make([]error, len(chunks))
	)

	// Goroutine synchronisation, deliberately simple:
	//   - each goroutine owns exactly one Chunk and one .part file, so there
	//     is no shared mutable state and no locking on the hot path;
	//   - progress is published through the chunk's atomic counter;
	//   - errors go into a pre-sized slice indexed by chunk, so there is no
	//     channel and no risk of a blocked send leaking a goroutine;
	//   - the first failure cancels the shared context, which unblocks every
	//     other in-flight Read immediately.
	failCtx, failCancel := context.WithCancel(ctx)
	defer failCancel()

	for _, c := range chunks {
		wg.Add(1)
		go func(c *Chunk) {
			defer wg.Done()
			if err := m.fetchChunk(failCtx, job, c); err != nil {
				errs[c.Index] = err
				failCancel()
				return
			}
			c.done.Store(true)
		}(c)
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	job.setState(StateMerging)
	m.Broadcast()
	return m.mergeParts(job, chunks, size)
}

// fetchChunk downloads one range with bounded retries and resume support.
func (m *Manager) fetchChunk(ctx context.Context, job *Job, c *Chunk) error {
	partPath := m.partPath(job, c.Index)
	var lastErr error

	for attempt := 0; attempt <= m.cfg.MaxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if attempt > 0 {
			// Quadratic backoff, capped, and interruptible by cancellation.
			delay := time.Duration(attempt*attempt) * 500 * time.Millisecond
			if delay > 8*time.Second {
				delay = 8 * time.Second
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
			log.Printf("job %s chunk %d: retry %d after %v", job.ID, c.Index, attempt, lastErr)
		}

		// Resume from whatever this chunk already wrote to disk.
		f, err := os.OpenFile(partPath, os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return fmt.Errorf("chunk %d: cannot open part file: %w", c.Index, err)
		}
		st, err := f.Stat()
		if err != nil {
			f.Close()
			return fmt.Errorf("chunk %d: stat part file: %w", c.Index, err)
		}
		written := st.Size()
		if written > c.Size() {
			// Stale or oversized leftover: restart this chunk from scratch.
			written = 0
			if err := f.Truncate(0); err != nil {
				f.Close()
				return fmt.Errorf("chunk %d: truncate: %w", c.Index, err)
			}
		}
		c.downloaded.Store(written)
		if written == c.Size() {
			f.Close()
			return nil
		}
		if _, err := f.Seek(written, io.SeekStart); err != nil {
			f.Close()
			return fmt.Errorf("chunk %d: seek: %w", c.Index, err)
		}

		err = m.streamRange(ctx, job, c, f, c.Start+written, c.End)
		f.Close()

		if err == nil {
			if c.downloaded.Load() == c.Size() {
				return nil
			}
			err = fmt.Errorf("short read: got %d of %d bytes", c.downloaded.Load(), c.Size())
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		lastErr = err
	}
	return fmt.Errorf("chunk %d failed after %d retries: %w", c.Index, m.cfg.MaxRetries, lastErr)
}

// streamRange performs the ranged GET and copies the body into w while
// updating the chunk's atomic progress counter.
func (m *Manager) streamRange(ctx context.Context, job *Job, c *Chunk, w io.Writer, from, to int64) error {
	req, err := m.newRequest(ctx, http.MethodGet, job)
	if err != nil {
		return err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", from, to))

	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer drainAndClose(resp.Body)

	if resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("expected 206 Partial Content, got %s", resp.Status)
	}

	_, err = io.Copy(w, &progressReader{r: resp.Body, counter: c})
	return err
}

// progressReader increments the chunk counter as bytes flow through it.
type progressReader struct {
	r       io.Reader
	counter *Chunk
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.counter.downloaded.Add(int64(n))
	}
	return n, err
}

// mergeParts concatenates every .part file, in index order, into the target.
func (m *Manager) mergeParts(job *Job, chunks []*Chunk, expected int64) error {
	job.mu.RLock()
	finalPath := job.finalPath
	job.mu.RUnlock()

	out, err := os.OpenFile(finalPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("cannot create output file: %w", err)
	}
	defer out.Close()

	buf := make([]byte, 1<<20)
	var total int64
	for _, c := range chunks {
		p := m.partPath(job, c.Index)
		in, err := os.Open(p)
		if err != nil {
			return fmt.Errorf("missing part %d: %w", c.Index, err)
		}
		n, err := io.CopyBuffer(out, in, buf)
		in.Close()
		if err != nil {
			return fmt.Errorf("merging part %d: %w", c.Index, err)
		}
		total += n
	}
	if err := out.Sync(); err != nil {
		return fmt.Errorf("flushing output: %w", err)
	}
	if expected > 0 && total != expected {
		return fmt.Errorf("size mismatch: wrote %d bytes, expected %d", total, expected)
	}

	m.cleanupParts(job)
	return nil
}

// ---------------------------------------------------------------------------
// Single-stream fallback (no range support / unknown size)
// ---------------------------------------------------------------------------

func (m *Manager) downloadSingle(ctx context.Context, job *Job, size int64) error {
	end := size - 1
	if size <= 0 {
		end = 0 // unknown length: the UI shows an indeterminate bar
	}
	c := &Chunk{Index: 0, Start: 0, End: end}
	job.mu.Lock()
	job.chunks = []*Chunk{c}
	finalPath := job.finalPath
	job.mu.Unlock()

	req, err := m.newRequest(ctx, http.MethodGet, job)
	if err != nil {
		return err
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("server replied %s", resp.Status)
	}

	out, err := os.OpenFile(finalPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("cannot create output file: %w", err)
	}
	defer out.Close()

	n, err := io.Copy(out, &progressReader{r: resp.Body, counter: c})
	if err != nil {
		return err
	}
	if size <= 0 {
		// We only learn the real size at the end; patch it so the UI hits 100%.
		job.mu.Lock()
		job.size = n
		job.mu.Unlock()
		c.End = n - 1
	}
	c.done.Store(true)
	return out.Sync()
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (m *Manager) partPath(job *Job, index int) string {
	return filepath.Join(m.cfg.TempDir, fmt.Sprintf("%s.part%02d", job.ID, index))
}

func (m *Manager) cleanupParts(job *Job) {
	job.mu.RLock()
	chunks := job.chunks
	job.mu.RUnlock()
	for _, c := range chunks {
		_ = os.Remove(m.partPath(job, c.Index))
	}
}

// newRequest builds a request carrying the browser context (UA, Referer,
// Cookie) so that protected media resolves exactly as it did in the page.
func (m *Manager) newRequest(ctx context.Context, method string, job *Job) (*http.Request, error) {
	job.mu.RLock()
	r := job.req
	job.mu.RUnlock()

	req, err := http.NewRequestWithContext(ctx, method, job.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("bad request: %w", err)
	}
	ua := r.UserAgent
	if ua == "" {
		ua = m.cfg.UserAgent
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "*/*")
	if r.Referrer != "" {
		req.Header.Set("Referer", r.Referrer)
	}
	if r.Cookie != "" {
		req.Header.Set("Cookie", r.Cookie)
	}
	return req, nil
}

// drainAndClose lets the transport reuse the connection instead of dropping it.
func drainAndClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 8<<10))
	_ = body.Close()
}

func (j *Job) filenameOrDefault() string {
	j.mu.RLock()
	defer j.mu.RUnlock()
	if j.filename != "" {
		return j.filename
	}
	return "download.bin"
}

// filenameFrom resolves the target name: Content-Disposition wins, then the
// URL path, then a generic name plus an extension guessed from the MIME type.
func filenameFrom(resp *http.Response, rawURL, mimeType string) string {
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		if _, params, err := mime.ParseMediaType(cd); err == nil {
			if name := params["filename"]; name != "" {
				if s := sanitizeFilename(name); s != "" {
					return s
				}
			}
		}
	}
	if u, err := url.Parse(rawURL); err == nil {
		base := path.Base(u.Path)
		if unescaped, err := url.PathUnescape(base); err == nil {
			base = unescaped
		}
		if s := sanitizeFilename(base); s != "" && s != "/" && s != "." {
			if filepath.Ext(s) == "" {
				s += extensionFor(mimeType)
			}
			return s
		}
	}
	return "download" + extensionFor(mimeType)
}

func extensionFor(mimeType string) string {
	if mimeType == "" {
		return ".bin"
	}
	if exts, err := mime.ExtensionsByType(mimeType); err == nil && len(exts) > 0 {
		return exts[0]
	}
	return ".bin"
}

var (
	illegalChars = regexp.MustCompile("[<>:\"/\\\\|?*\x00-\x1f]")

	windowsReserved = map[string]bool{
		"CON": true, "PRN": true, "AUX": true, "NUL": true,
		"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
		"COM6": true, "COM7": true, "COM8": true, "COM9": true,
		"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
		"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
	}
)

// sanitizeFilename strips path separators, control characters and Windows
// specific hazards. It also drops any query string that leaked in from a URL.
func sanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if i := strings.IndexAny(name, "?#"); i >= 0 {
		name = name[:i]
	}
	name = illegalChars.ReplaceAllString(name, "_")
	name = strings.Trim(name, " .")
	if name == "" {
		return ""
	}
	stem := strings.ToUpper(strings.TrimSuffix(name, filepath.Ext(name)))
	if windowsReserved[stem] {
		name = "_" + name
	}
	if len(name) > 180 {
		ext := filepath.Ext(name)
		name = name[:180-len(ext)] + ext
	}
	return name
}

// uniquePath avoids clobbering an existing file: video.mp4 -> video (1).mp4.
func uniquePath(dir, name string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("cannot create download dir: %w", err)
	}
	candidate := filepath.Join(dir, name)
	if _, err := os.Stat(candidate); os.IsNotExist(err) {
		return candidate, nil
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 1; i < 1000; i++ {
		candidate = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, i, ext))
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("cannot find a free filename for %q", name)
}
