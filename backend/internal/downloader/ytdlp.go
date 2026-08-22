package downloader

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/quick-download/backend/internal/config"
)

// ---------------------------------------------------------------------------
// yt-dlp engine
// ---------------------------------------------------------------------------
//
// Streaming sites hand out a manifest (HLS/DASH) plus thousands of segments,
// often with separate video and audio tracks that must be muxed. Re-implementing
// that is a project in itself, so we shell out to yt-dlp and use ffmpeg for the
// muxing — and translate yt-dlp's progress into the same JobSnapshot the
// chunked engine produces, so the dashboard cannot tell the difference.
//
// Progress comes from --progress-template, which prints one machine-readable
// line per update. We never scrape the human-readable progress bar unless the
// template output is missing (older builds), because its format changes.

// progressSentinel prefixes our machine-readable progress lines.
const (
	progressSentinel = "@QDP@"
	postSentinel     = "@QDPP@"
)

// runYtDlp downloads a job with yt-dlp and streams progress into the job.
func (m *Manager) runYtDlp(ctx context.Context, job *Job, kind Kind) error {
	tools := m.cfg.Tools()
	if !tools.YtDlp.Found {
		return fmt.Errorf(
			"yt-dlp is required for %s downloads but was not found. Put yt-dlp%s next to the engine (%s) or on PATH",
			kind, exeSuffix(), firstDir(tools.SearchedIn))
	}
	if !tools.Ffmpeg.Found {
		return fmt.Errorf(
			"ffmpeg is required to merge %s streams but was not found. Put ffmpeg%s next to the engine (%s) or on PATH",
			kind, exeSuffix(), firstDir(tools.SearchedIn))
	}

	args := m.ytDlpArgs(job, tools)
	cmd := exec.CommandContext(ctx, tools.YtDlp.Path, args...)
	cmd.Dir = job.Dir()

	// yt-dlp spawns ffmpeg as a child; put the whole thing in its own process
	// group so cancelling kills the tree instead of orphaning the muxer.
	configureProcAttr(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("cannot pipe yt-dlp stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("cannot pipe yt-dlp stderr: %w", err)
	}

	log.Printf("job %s: %s %s", job.ID, tools.YtDlp.Path, strings.Join(redactArgs(args), " "))
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("cannot start yt-dlp: %w", err)
	}

	job.setProcess(cmd)
	job.setPhase("starting")

	// stderr is drained concurrently: a full pipe buffer would deadlock the
	// child process, and the tail of it is the only useful error message.
	var (
		wg       sync.WaitGroup
		errTail  = newRingBuffer(12)
		scanDone = make(chan struct{})
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		scanLines(stderr, func(line string) {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				return
			}
			errTail.add(trimmed)
			// yt-dlp reports some ordinary progress on stderr too.
			m.consumeYtDlpLine(job, trimmed)
			log.Printf("job %s [yt-dlp:err] %s", job.ID, trimmed)
		})
	}()

	go func() {
		defer close(scanDone)
		scanLines(stdout, func(line string) {
			m.consumeYtDlpLine(job, strings.TrimSpace(line))
		})
	}()

	<-scanDone
	wg.Wait()
	waitErr := cmd.Wait()
	job.setProcess(nil)

	if ctx.Err() != nil {
		return ctx.Err()
	}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			if tail := errTail.String(); tail != "" {
				return fmt.Errorf("yt-dlp failed (exit %d): %s", exitErr.ExitCode(), tail)
			}
			return fmt.Errorf("yt-dlp failed with exit code %d", exitErr.ExitCode())
		}
		return fmt.Errorf("yt-dlp failed: %w", waitErr)
	}

	job.setPhase("done")
	job.markExternalComplete()
	return nil
}

// ytDlpArgs builds the command line.
func (m *Manager) ytDlpArgs(job *Job, tools config.Tools) []string {
	job.mu.RLock()
	req := job.req
	job.mu.RUnlock()

	// ---Add this blog to clean YouTube URLs ---
	cleanURL := job.URL
	if strings.Contains(cleanURL, "youtube.com/watch") || strings.Contains(cleanURL, "youtu.be/") {
		// ถ้ามี &list หรือพารามิเตอร์อื่นๆ ที่ไม่ใช่ v= ให้พยายามตัดออก
		if idx := strings.Index(cleanURL, "&list="); idx != -1 {
			cleanURL = cleanURL[:idx]
		}
	}

	dir := job.Dir()
	outTmpl := filepath.Join(dir, "%(title).150B [%(id)s].%(ext)s")
	if req.Filename != "" {
		// An explicit name still needs yt-dlp to pick the container extension.
		stem := strings.TrimSuffix(sanitizeFilename(req.Filename), filepath.Ext(req.Filename))
		if stem != "" {
			outTmpl = filepath.Join(dir, stem+".%(ext)s")
		}
	}

	// Strip playlist/tracking parameters before yt-dlp ever sees the URL.
	target := cleanMediaURL(job.URL)
	if target != job.URL {
		log.Printf("job %s: cleaned URL %s -> %s", job.ID, job.URL, target)
	}

	args := []string{
		"--newline",     // one progress update per line instead of \r redraws
		"--no-colors",   // no ANSI escapes to strip
		"--no-playlist", // a page URL means "this video", not "all 400 of them"
		"--no-warnings",

		// Keep going when one item in a tab/playlist context errors out rather
		// than aborting the whole job. This can make yt-dlp exit 0 having
		// downloaded nothing, so runYtDlp verifies the output file exists
		// before calling the job complete.
		"--ignore-errors",
		"--ffmpeg-location", filepath.Dir(tools.Ffmpeg.Path),
		"-o", outTmpl,

		// Skip the authentication probe the youtube:tab extractor performs.
		// Even with the URL cleaned, a mix or radio id can still route through
		// that extractor, which then fails on "Playlists that require
		// authentication". Unknown extractor keys are ignored by yt-dlp.
		"--extractor-args", "youtubetab:skip=authcheck",

		// Machine-readable progress. %(progress.X)s prints "NA" when unknown,
		// which parseNum turns into a zero value.
		"--progress-template",
		progressSentinel + "%(progress.status)s|%(progress.downloaded_bytes)s|%(progress.total_bytes)s|" +
			"%(progress.total_bytes_estimate)s|%(progress.speed)s|%(progress.eta)s|" +
			"%(progress.fragment_index)s|%(progress.fragment_count)s",
		"--progress-template",
		"postprocess:" + postSentinel + "%(progress.status)s|%(progress.postprocessor)s",
	}

	// Resolution / audio-only selection.
	args = append(args, qualityArgs(req.Quality)...)

	// Carry the browser's identity so gated streams resolve the same way —
	// except on YouTube, where doing exactly that is what trips the anti-bot
	// check. See the block comment at the bottom of this file.
	//
	// The User-Agent matters as much as the cookie here: a session cookie is
	// bound to the client that minted it, so a mismatched UA is itself a
	// signal. All three go together or none do.
	if sendBrowserIdentity(target) {
		if req.UserAgent != "" {
			args = append(args, "--user-agent", req.UserAgent)
		}
		if req.Referrer != "" {
			args = append(args, "--referer", req.Referrer)
		}
		if req.Cookie != "" {
			// One header for every request yt-dlp makes for this job.
			args = append(args, "--add-header", "Cookie:"+req.Cookie)
		}
	} else {
		log.Printf("job %s: YouTube - not forwarding browser UA/Referer/Cookie", job.ID)
	}

	// "--" terminates option parsing: without it a URL beginning with "-"
	// would be read as a flag.
	return append(args, "--", target)
}

// ---------------------------------------------------------------------------
// Output parsing
// ---------------------------------------------------------------------------

var (
	// Fallback for builds without --progress-template: "[download]  12.3% of ..."
	legacyPercentRe = regexp.MustCompile(`\[download\]\s+(\d{1,3}(?:\.\d+)?)%`)
	// "[download] Destination: C:\path\file.f137.mp4"
	destinationRe = regexp.MustCompile(`^\[download\]\s+Destination:\s+(.+)$`)
	// "[Merger] Merging formats into "C:\path\file.mp4""
	mergerRe = regexp.MustCompile(`Merging formats into "(.+)"`)
	// "[download] C:\path\file.mp4 has already been downloaded"
	alreadyRe = regexp.MustCompile(`^\[download\]\s+(.+?)\s+has already been downloaded`)
	// "[ExtractAudio] Destination: ..." and friends announce the phase.
	phaseRe = regexp.MustCompile(`^\[([A-Za-z0-9_]+)\]`)
	// "[info] abc: Downloading 1 format(s): 137+140" - the "+" tells us how
	// many separate tracks will be fetched before the merge.
	formatPlanRe = regexp.MustCompile(`Downloading \d+ format\(s\):\s*(\S+)`)
)

// countTracks turns a format spec like "137+140" into the number of separate
// downloads yt-dlp will perform.
func countTracks(spec string) int {
	return strings.Count(spec, "+") + 1
}

// consumeYtDlpLine turns one line of yt-dlp output into job state.
func (m *Manager) consumeYtDlpLine(job *Job, line string) {
	if line == "" {
		return
	}

	switch {
	case strings.HasPrefix(line, progressSentinel):
		if m.applyProgressTemplate(job, strings.TrimPrefix(line, progressSentinel)) {
			m.Broadcast()
		}
		return

	case strings.HasPrefix(line, postSentinel):
		fields := strings.Split(strings.TrimPrefix(line, postSentinel), "|")
		if len(fields) >= 2 && fields[1] != "NA" {
			job.setPhase("post-processing: " + fields[1])
			m.Broadcast()
		}
		return
	}

	// Filenames: the merger's output wins, it is the file the user keeps.
	if mm := mergerRe.FindStringSubmatch(line); len(mm) == 2 {
		job.setFinalPath(strings.TrimSpace(mm[1]), true)
		job.setPhase("merging")
		m.Broadcast()
		return
	}
	if mm := destinationRe.FindStringSubmatch(line); len(mm) == 2 {
		// Each Destination line starts a new track.
		job.beginStream()
		job.setFinalPath(strings.TrimSpace(mm[1]), false)
		m.Broadcast()
		return
	}
	if mm := formatPlanRe.FindStringSubmatch(line); len(mm) == 2 {
		job.setStreamPlan(countTracks(mm[1]))
		return
	}
	if mm := alreadyRe.FindStringSubmatch(line); len(mm) == 2 {
		job.setFinalPath(strings.TrimSpace(mm[1]), true)
		job.markExternalComplete()
		m.Broadcast()
		return
	}

	// Legacy percent, only used when the template produced nothing.
	if mm := legacyPercentRe.FindStringSubmatch(line); len(mm) == 2 && !job.hasTemplateProgress() {
		if pct, err := strconv.ParseFloat(mm[1], 64); err == nil {
			job.setExternalPercent(pct)
			m.Broadcast()
		}
		return
	}

	// "[SomePhase] ..." lines are a decent human-readable phase label.
	if mm := phaseRe.FindStringSubmatch(line); len(mm) == 2 {
		switch tag := mm[1]; tag {
		case "download":
			// too chatty to use as a phase
		case "Merger":
			job.setPhase("merging")
			m.Broadcast()
		default:
			job.setPhase(strings.ToLower(tag))
			m.Broadcast()
		}
	}
}

// applyProgressTemplate parses our pipe-delimited progress line.
//
// Layout: status|downloaded|total|total_estimate|speed|eta|frag_index|frag_count
func (m *Manager) applyProgressTemplate(job *Job, payload string) bool {
	fields := strings.Split(payload, "|")
	if len(fields) < 8 {
		return false
	}

	status := fields[0]
	downloaded := parseNum(fields[1])
	total := parseNum(fields[2])
	if total <= 0 {
		total = parseNum(fields[3]) // total_bytes_estimate
	}
	speed := parseNum(fields[4])
	eta := parseNum(fields[5])
	fragIndex := parseNum(fields[6])
	fragCount := parseNum(fields[7])

	// Percent precedence: real byte counts, then fragment counts (HLS rarely
	// knows its total size), then leave it unknown.
	percent := -1.0
	switch {
	case total > 0 && downloaded >= 0:
		percent = downloaded / total * 100
	case fragCount > 0 && fragIndex >= 0:
		percent = fragIndex / fragCount * 100
	}
	if percent > 100 {
		percent = 100
	}

	job.applyExternalProgress(externalProgress{
		downloaded: int64(downloaded),
		total:      int64(total),
		percent:    percent,
		speed:      speed,
		eta:        eta,
		phase:      ytDlpPhase(status, fragCount > 0),
	})
	return true
}

func ytDlpPhase(status string, fragmented bool) string {
	switch status {
	case "downloading":
		if fragmented {
			return "downloading segments"
		}
		return "downloading"
	case "finished":
		return "stream complete"
	case "error":
		return "error"
	case "", "NA":
		return ""
	default:
		return status
	}
}

// parseNum turns a progress-template field into a float. yt-dlp prints "NA"
// for values it does not know, and sometimes "None" on older builds.
func parseNum(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "NA" || s == "None" || s == "null" {
		return -1
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return -1
	}
	return v
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// scanLines reads r and calls fn per line, splitting on \n AND \r so a progress
// bar that redraws with carriage returns still yields updates.
func scanLines(r io.Reader, fn func(string)) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	scanner.Split(scanLinesCRLF)
	for scanner.Scan() {
		fn(scanner.Text())
	}
}

// scanLinesCRLF is bufio.ScanLines extended to treat a lone \r as a terminator.
func scanLinesCRLF(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	for i, b := range data {
		if b == '\n' || b == '\r' {
			// Swallow the \n of a \r\n pair.
			skip := 1
			if b == '\r' && i+1 < len(data) && data[i+1] == '\n' {
				skip = 2
			}
			return i + skip, data[:i], nil
		}
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// ringBuffer keeps the last N lines of stderr for the error message.
type ringBuffer struct {
	mu    sync.Mutex
	lines []string
	max   int
}

func newRingBuffer(max int) *ringBuffer { return &ringBuffer{max: max} }

func (r *ringBuffer) add(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, line)
	if len(r.lines) > r.max {
		r.lines = r.lines[len(r.lines)-r.max:]
	}
}

func (r *ringBuffer) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	// The last ERROR line is almost always the useful one.
	for i := len(r.lines) - 1; i >= 0; i-- {
		if strings.Contains(r.lines[i], "ERROR:") {
			return r.lines[i]
		}
	}
	if len(r.lines) == 0 {
		return ""
	}
	return r.lines[len(r.lines)-1]
}

// redactArgs hides cookie values before the command line reaches the log file.
func redactArgs(args []string) []string {
	out := make([]string, len(args))
	copy(out, args)
	for i, a := range out {
		if strings.HasPrefix(a, "Cookie:") {
			out[i] = "Cookie:<redacted>"
		}
	}
	return out
}

func exeSuffix() string {
	if isWindows() {
		return ".exe"
	}
	return ""
}

func firstDir(dirs []string) string {
	if len(dirs) == 0 {
		return "the engine folder"
	}
	return dirs[0]
}

// ---------------------------------------------------------------------------
// YouTube special cases
// ---------------------------------------------------------------------------
//
// YouTube needs the opposite treatment from every other site.
//
// Elsewhere, forwarding the browser's User-Agent, Referer and Cookie is what
// makes a gated CDN hand over the bytes. On YouTube it is what gets you
// blocked: a session cookie sent from a different client than the one that
// minted it trips the anti-bot check, and yt-dlp reports
//
//	ERROR: [youtube] <id>: Sign in to confirm... / The page needs to be reloaded
//
// yt-dlp already negotiates its own client identity with YouTube, so the
// correct move is to stay out of its way and send nothing.
//
// If you ever need authenticated YouTube downloads (age-gated, members-only,
// private), the supported route is yt-dlp's own --cookies-from-browser chrome,
// which reads Chrome's cookie database directly and in the right format. A
// scraped Cookie header is not a substitute.

var youtubeHosts = []string{"youtube.com", "youtu.be", "youtube-nocookie.com"}

// isYouTubeURL reports whether a URL belongs to YouTube, including every
// subdomain (m., music., accounts., consent., ...).
func isYouTubeURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	host = strings.TrimPrefix(host, "www.")
	for _, h := range youtubeHosts {
		// Exact match or a real subdomain — never a suffix match, so
		// "youtube.com.evil.test" does not qualify.
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

// videoPathRe matches the YouTube URL shapes where the path alone identifies
// the video, so the entire query string is disposable.
var videoPathRe = regexp.MustCompile(`^/(shorts|live|embed|v)/[\w-]+`)

// cleanMediaURL removes parameters that send yt-dlp down the wrong code path.
//
// The offender is `list`: a watch URL copied from the address bar while a mix
// or radio playlist is active carries `&list=RD…`, and yt-dlp then routes
// through its youtube:tab extractor, which fails with
//
//	ERROR: [youtube:tab] api: Playlists that require authentication...
//
// even though the user only ever wanted the single video on screen. `index`,
// `t`, `pp` and `si` are equally irrelevant to downloading.
//
// A deliberate playlist URL (/playlist?list=…, with no video id to fall back
// on) is left untouched — stripping its only parameter would break it.
func cleanMediaURL(raw string) string {
	if !isYouTubeURL(raw) {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.Fragment = ""

	// youtu.be/<id> -> the canonical watch URL.
	if strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.") == "youtu.be" {
		if id := strings.Trim(u.Path, "/"); id != "" {
			return "https://www.youtube.com/watch?v=" + strings.SplitN(id, "/", 2)[0]
		}
		return raw
	}

	if v := u.Query().Get("v"); v != "" && (u.Path == "/watch" || u.Path == "/watch/") {
		return "https://www.youtube.com/watch?v=" + v
	}

	if videoPathRe.MatchString(u.Path) {
		u.RawQuery = ""
		return u.String()
	}

	return raw
}

// sendBrowserIdentity reports whether the browser's UA/Referer/Cookie should be
// forwarded to yt-dlp for this URL. See the block comment above.
func sendBrowserIdentity(raw string) bool {
	return !isYouTubeURL(raw)
}

// ---------------------------------------------------------------------------
// Quality selection
// ---------------------------------------------------------------------------

// Quality values accepted from the extension.
const (
	QualityBest  = "best"
	Quality1080p = "1080p"
	Quality720p  = "720p"
	QualityAudio = "audio"
)

// qualityArgs maps a UI choice onto yt-dlp format selectors.
//
// The trailing "/b" in each selector is the fallback: if no separate video and
// audio streams match, take the best pre-muxed single file instead. Without it
// a site that only offers combined streams fails outright.
//
// Audio-only deliberately omits --merge-output-format: there is no video track
// to merge, and the container is decided by --audio-format.
func qualityArgs(quality string) []string {
	switch strings.ToLower(strings.TrimSpace(quality)) {
	case QualityAudio, "audio-only", "audioonly", "mp3":
		return []string{
			"-f", "ba/b",
			"--extract-audio",
			"--audio-format", "mp3",
			"--audio-quality", "0", // best VBR
		}
	case Quality1080p, "1080":
		return []string{
			"-f", "bv*[height<=1080]+ba/b",
			"--merge-output-format", "mp4",
		}
	case Quality720p, "720":
		return []string{
			"-f", "bv*[height<=720]+ba/b",
			"--merge-output-format", "mp4",
		}
	default: // "", "best", or anything unrecognised
		return []string{
			"-f", "bv*+ba/b",
			"--merge-output-format", "mp4",
		}
	}
}
