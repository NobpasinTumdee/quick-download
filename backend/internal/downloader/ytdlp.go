package downloader

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

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

	// Belt and braces for the encoding problem --encoding utf-8 solves below.
	// These are what a pip-installed yt-dlp running on the system Python obeys;
	// the frozen yt-dlp.exe ignores them, which is why the flag does the real
	// work. (dedupEnv keeps the last value, so these override an inherited one.)
	cmd.Env = append(os.Environ(), "PYTHONIOENCODING=utf-8", "PYTHONUTF8=1")

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

	// yt-dlp can exit 0 having downloaded nothing - most often when
	// --ignore-errors swallows a failed extraction. Never call a job complete
	// without a file on disk to show for it.
	if err := m.verifyYtDlpOutput(job, errTail); err != nil {
		return err
	}

	job.setPhase("done")
	job.markExternalComplete()
	return nil
}

// ---------------------------------------------------------------------------
// Output verification
// ---------------------------------------------------------------------------
//
// A clean exit is not proof of a download: --ignore-errors makes yt-dlp exit 0
// after an extraction that produced nothing. But the check that catches those
// must never fail a download that did work, and the name yt-dlp announces is
// only a hint at the name it finally writes:
//
//   - the container changes (a failed mp4 merge falls back to .mkv);
//   - a post-processor replaces the file entirely (--extract-audio turns
//     "Clip.webm" into "Clip.mp3" and deletes the original);
//   - "%(title).150B" truncates a long title mid-word;
//   - filename sanitisation rewrites the characters Windows forbids; and
//   - when yt-dlp's stdout is a pipe, Python encodes it with the console code
//     page, and every character that page cannot represent is printed as "?".
//     The path we parsed can therefore be literally unspellable. (runYtDlp now
//     forces UTF-8 on the child to stop this at the source, but the fallbacks
//     still have to cope with output from a build that ignores it.)
//
// So resolution is a ladder from exact to fuzzy, and every rung below the first
// is matched against a real directory listing rather than a glob pattern: a
// pattern built from a name containing "[", "*" or "?" does not mean what it
// says, and on Windows it cannot be escaped either.

// Suffixes yt-dlp leaves behind for work in progress. A half-written file is
// not a completed download, so these never count as a match.
var incompleteSuffixes = []string{".part", ".ytdl", ".temp", ".tmp"}

// perTrackSuffixRe matches the format-id yt-dlp appends to each track it
// downloads separately, e.g. "Clip.f137.mp4" for the video half of a merge.
var perTrackSuffixRe = regexp.MustCompile(`\.f\d+$`)

// idInBracketsRe pulls the id out of a name built by our output template,
// "%(title).150B [%(id)s].%(ext)s". The id is the one part of the name that
// truncation, sanitisation and encoding loss all leave alone, which makes it
// the strongest handle we have on the file.
var idInBracketsRe = regexp.MustCompile(`\[([A-Za-z0-9_.-]{3,})\]$`)

// urlIDRe recovers a video id straight from the page URL, for the case where
// yt-dlp announced no usable name at all.
var urlIDRe = regexp.MustCompile(`(?:[?&]v=|youtu\.be/|/shorts/|/embed/|/watch/)([A-Za-z0-9_-]{6,})`)

// mediaExts gates the last-resort scan. Anything outside this list (a
// thumbnail, a .json info dump, a subtitle) is never adopted as the download.
var mediaExts = map[string]bool{
	".mp4": true, ".mkv": true, ".webm": true, ".mov": true, ".avi": true,
	".flv": true, ".ts": true, ".m4v": true, ".mpg": true, ".mpeg": true,
	".3gp": true, ".ogv": true, ".mp3": true, ".m4a": true, ".opus": true,
	".ogg": true, ".oga": true, ".aac": true, ".flac": true, ".wav": true,
	".wma": true,
}

// candidateFile is one finished file found in a directory we searched.
type candidateFile struct {
	path  string
	stem  string // base name without its extension
	ext   string // lower cased, with the dot
	size  int64
	mod   time.Time
	track bool // the name carries a per-track ".f137" suffix
}

// verifyYtDlpOutput confirms that a successful-looking run actually produced a
// file, and adopts the real path when yt-dlp chose a different one.
func (m *Manager) verifyYtDlpOutput(job *Job, tail *ringBuffer) error {
	job.mu.RLock()
	announced := append([]string(nil), job.outputs...)
	predicted := job.finalPath
	dir := job.dir
	started := job.startedAt
	pageURL := job.URL
	job.mu.RUnlock()

	// 1. The exact path yt-dlp last told us about. This is the overwhelmingly
	//    common case, and it costs one stat instead of a directory listing.
	if predicted != "" && isRealFile(predicted) {
		return nil
	}

	names := normalizePaths(dir, append(announced, predicted))
	found, how := resolveOutput(names, dir, pageURL, started, func(p string) bool {
		return m.pathClaimedElsewhere(job.ID, p)
	})
	if found != "" {
		log.Printf("job %s: output resolved by %s -> %s (yt-dlp announced %q)",
			job.ID, how, found, predicted)
		job.setFinalPath(found, true)
		return nil
	}

	if len(names) == 0 {
		return phantomError(tail, "yt-dlp never reported an output file")
	}
	return phantomError(tail, fmt.Sprintf("expected %s", filepath.Base(names[len(names)-1])))
}

// resolveOutput walks the ladder. names are the paths yt-dlp announced, in the
// order it announced them, so the last is the most likely; dir is the job's own
// output directory. claimed reports whether a path already belongs to a
// different job.
//
// It returns the resolved path and the name of the rung that found it, or "".
func resolveOutput(names []string, dir, pageURL string, started time.Time, claimed func(string) bool) (string, string) {
	files := scanFinishedFiles(searchDirs(dir, names))
	if len(files) == 0 {
		return "", ""
	}

	// Stems to match on, most recently announced first, each also stripped of
	// its per-track suffix: only "Clip.f137.mp4" may have been announced while
	// the merged "Clip.mp4" is what survived.
	stems := make([]string, 0, len(names)*2)
	for i := len(names) - 1; i >= 0; i-- {
		base := filepath.Base(names[i])
		stem := strings.TrimSuffix(base, filepath.Ext(base))
		if stem == "" {
			continue
		}
		stems = appendUnique(stems, stem)
		if trimmed := perTrackSuffixRe.ReplaceAllString(stem, ""); trimmed != stem && trimmed != "" {
			stems = appendUnique(stems, trimmed)
		}
	}

	// 2. The same name under a different extension. Covers the changed
	//    container and the post-processor that rewrote the file.
	for _, stem := range stems {
		if hit := best(filterFiles(files, func(f candidateFile) bool {
			return strings.EqualFold(f.stem, stem)
		})); hit != "" {
			return hit, "stem"
		}
	}

	// 3. The same name with "?" standing in for the characters the console
	//    encoding could not print. One "?" is one lost character, so this is a
	//    single-character wildcard match rather than a fuzzy one.
	for _, stem := range stems {
		if !strings.ContainsRune(stem, '?') {
			continue
		}
		if hit := best(filterFiles(files, func(f candidateFile) bool {
			return wildcardEqual(stem, f.stem)
		})); hit != "" {
			return hit, "encoding-lossy name"
		}
	}

	// 4. The id in brackets, which our output template always appends and no
	//    amount of sanitising touches. The strongest heuristic we have.
	for _, id := range outputIDs(stems, pageURL) {
		marker := "[" + id + "]"
		if hit := best(filterFiles(files, func(f candidateFile) bool {
			return strings.Contains(f.stem, marker)
		})); hit != "" {
			return hit, "video id"
		}
	}

	// 5. The ASCII skeleton. When yt-dlp printed through a code page that
	//    could not represent the title, every character outside that page was
	//    dropped (errors="ignore") or replaced - but the ASCII survived intact
	//    and in order. Comparing only the ASCII letters and digits therefore
	//    matches the file exactly, whatever script the title was written in.
	for _, stem := range stems {
		skeleton := asciiFold(stem)
		if len(skeleton) < 6 {
			continue // a title that is almost entirely non-ASCII leaves nothing to match
		}
		if hit := best(filterFiles(files, func(f candidateFile) bool {
			return asciiFold(f.stem) == skeleton
		})); hit != "" {
			return hit, "ASCII skeleton"
		}
	}

	// 6. Fuzzy stem: compare with punctuation, spaces and case removed, and
	//    accept a prefix either way round so a truncated title still matches.
	for _, stem := range stems {
		folded := foldName(stem)
		if len(folded) < 8 {
			continue // too little signal to be sure it is the same file
		}
		if hit := best(filterFiles(files, func(f candidateFile) bool {
			other := foldName(f.stem)
			if len(other) < 8 {
				return false
			}
			return strings.HasPrefix(folded, other) || strings.HasPrefix(other, folded)
		})); hit != "" {
			return hit, "fuzzy name"
		}
	}

	// 7. Last resort: the name told us nothing, so go by time. yt-dlp exited 0,
	//    and exactly one media file appeared in the output directory while this
	//    job was running - that is the download, whatever it ended up called.
	//
	//    Deliberately narrow, because the download directory is usually shared
	//    with the browser's own downloads: it must be the only candidate, and
	//    unclaimed by any other job. Two candidates is not a tie to break, it is
	//    a question we cannot answer, and adopting the wrong file is worse than
	//    reporting the failure.
	if started.IsZero() {
		return "", ""
	}
	fresh := filterFiles(files, func(f candidateFile) bool {
		return mediaExts[f.ext] && writtenDuring(f.mod, started) && !claimed(f.path)
	})
	// The separate video and audio tracks of a merge are not the download, so
	// they never make the choice ambiguous as long as something else survived.
	if merged := filterFiles(fresh, func(f candidateFile) bool { return !f.track }); len(merged) > 0 {
		fresh = merged
	}
	switch len(fresh) {
	case 1:
		return fresh[0].path, "the only file written during the run"
	case 0:
		return "", ""
	default:
		log.Printf("%d media files were written while the job ran; refusing to guess which is the download", len(fresh))
		return "", ""
	}
}

// writtenDuring reports whether a file was created or last written while the
// job was running. The slack absorbs both clock granularity (FAT timestamps
// have a two-second resolution) and the gap between marking the job started and
// the child process actually opening the file.
func writtenDuring(mod, started time.Time) bool {
	const slack = 2 * time.Second
	return !mod.Before(started.Add(-slack)) && !mod.After(time.Now().Add(slack))
}

// searchDirs is the job directory plus every directory an announced name lives
// in, deduplicated. The extra directories matter when a post-processor moves
// the file somewhere else.
func searchDirs(dir string, names []string) []string {
	var dirs []string
	if dir != "" {
		dirs = append(dirs, filepath.Clean(dir))
	}
	for _, name := range names {
		if d := filepath.Dir(name); d != "" && d != "." {
			dirs = appendUniqueFold(dirs, d)
		}
	}
	return dirs
}

// scanFinishedFiles lists the finished, non-empty files in the given
// directories. A listing beats a glob here: the names we match against
// routinely contain "[", and on Windows filepath.Glob cannot escape it.
func scanFinishedFiles(dirs []string) []candidateFile {
	var out []candidateFile
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			log.Printf("cannot list %s while resolving an output file: %v", dir, err)
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			info, err := entry.Info()
			if err != nil || info.Size() == 0 || isIncompleteName(entry.Name()) {
				continue
			}
			stem := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
			out = append(out, candidateFile{
				path:  filepath.Join(dir, entry.Name()),
				stem:  stem,
				ext:   strings.ToLower(filepath.Ext(entry.Name())),
				size:  info.Size(),
				mod:   info.ModTime(),
				track: perTrackSuffixRe.MatchString(stem),
			})
		}
	}
	return out
}

func filterFiles(files []candidateFile, keep func(candidateFile) bool) []candidateFile {
	var out []candidateFile
	for _, f := range files {
		if keep(f) {
			out = append(out, f)
		}
	}
	return out
}

// best picks the most plausible of several matches: the merged file over a
// per-track one (both survive when yt-dlp is told to keep the parts), then the
// largest, which is the video rather than its audio track.
func best(matches []candidateFile) string {
	var winner *candidateFile
	for i := range matches {
		c := &matches[i]
		switch {
		case winner == nil:
			winner = c
		case c.track != winner.track:
			if !c.track {
				winner = c
			}
		case c.size > winner.size:
			winner = c
		}
	}
	if winner == nil {
		return ""
	}
	return winner.path
}

// outputIDs collects the "[id]" suffixes of the announced names, falling back
// to an id parsed out of the page URL.
func outputIDs(stems []string, pageURL string) []string {
	var ids []string
	for _, stem := range stems {
		clean := perTrackSuffixRe.ReplaceAllString(stem, "")
		if mm := idInBracketsRe.FindStringSubmatch(clean); len(mm) == 2 {
			ids = appendUnique(ids, mm[1])
		}
	}
	if mm := urlIDRe.FindStringSubmatch(pageURL); len(mm) == 2 {
		ids = appendUnique(ids, mm[1])
	}
	return ids
}

// wildcardEqual compares two names treating "?" and U+FFFD in the pattern as
// any single character - the two things a lossy encoder leaves behind when it
// substitutes rather than deletes. Both sides are folded to lower case, since
// Windows filenames are.
func wildcardEqual(pattern, name string) bool {
	p, n := []rune(strings.ToLower(pattern)), []rune(strings.ToLower(name))
	if len(p) != len(n) {
		return false
	}
	for i := range p {
		if p[i] != '?' && p[i] != '\uFFFD' && p[i] != n[i] {
			return false
		}
	}
	return true
}

// asciiFold reduces a name to its ASCII letters and digits, lower cased. It is
// deliberately blind to everything else: the characters it drops are exactly
// the ones a lossy code page loses, so two names that differ only by that loss
// fold to the same string.
func asciiFold(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		if r < utf8.RuneSelf && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// foldName reduces a name to its letters and digits, lower cased, so that two
// spellings of the same title compare equal.
func foldName(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// normalizePaths cleans the raw strings scraped from yt-dlp's output into
// absolute, native-separator paths, dropping blanks and duplicates while
// keeping the announcement order.
//
// Note that we standardise on the OS separator rather than forward slashes:
// os.Stat accepts either on Windows, but any name comparison has to pick one,
// and it must be the one os.ReadDir hands back.
func normalizePaths(dir string, raw []string) []string {
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		if p := normalizePath(dir, r); p != "" {
			out = appendUniqueFold(out, p)
		}
	}
	return out
}

// normalizePath makes one announced path absolute and canonical. yt-dlp prints
// the path as the output template spelled it, which is absolute in our case,
// but a post-processor that prints a bare filename means it relative to the
// working directory - which is the job's own directory.
func normalizePath(dir, raw string) string {
	p := strings.TrimSpace(raw)
	p = strings.Trim(p, `"`)
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	p = filepath.FromSlash(p)
	if !filepath.IsAbs(p) && dir != "" {
		p = filepath.Join(dir, p)
	}
	return filepath.Clean(p)
}

// isRealFile reports whether a path is a finished, non-empty file.
func isRealFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Size() == 0 {
		return false
	}
	return !isIncompleteName(path)
}

func isIncompleteName(path string) bool {
	lower := strings.ToLower(path)
	for _, suffix := range incompleteSuffixes {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

// pathClaimedElsewhere reports whether another job already owns a file, so the
// last-resort scan cannot adopt a concurrent download's output.
func (m *Manager) pathClaimedElsewhere(selfID, path string) bool {
	for _, snap := range m.Snapshot() {
		if snap.ID != selfID && snap.Path != "" && strings.EqualFold(snap.Path, path) {
			return true
		}
	}
	return false
}

func appendUnique(list []string, v string) []string {
	for _, existing := range list {
		if existing == v {
			return list
		}
	}
	return append(list, v)
}

func appendUniqueFold(list []string, v string) []string {
	for _, existing := range list {
		if strings.EqualFold(existing, v) {
			return list
		}
	}
	return append(list, v)
}

// phantomError builds the message shown on the card, carrying yt-dlp's own last
// error when it left one - that is usually the real explanation.
func phantomError(tail *ringBuffer, detail string) error {
	base := "yt-dlp exited successfully but no output file was found (phantom completion)"
	if detail != "" {
		base = fmt.Sprintf("%s: %s", base, detail)
	}
	if tail != nil {
		if last := tail.String(); last != "" && strings.Contains(last, "ERROR:") {
			return fmt.Errorf("%s - %s", base, last)
		}
	}
	return errors.New(base)
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

		// Print in UTF-8, whatever the machine's code page is.
		//
		// This is not cosmetic. yt-dlp encodes everything it prints with
		// locale.getpreferredencoding() - the ANSI code page, cp874 on a Thai
		// Windows - and it does so with errors="ignore". A Japanese title is
		// therefore not mangled but SILENTLY DELETED on its way through the
		// pipe: "米津玄師 IRIS OUT 第76回.mp4" arrives as " IRIS OUT 76.mp4",
		// naming a file that does not exist, and the output check then calls a
		// perfectly good download a phantom completion.
		//
		// PYTHONIOENCODING does not fix this (the frozen yt-dlp.exe ignores it,
		// and preferredencoding() consults the locale rather than the stream),
		// and neither does chcp: the console code page is irrelevant to a pipe.
		// Verified against yt-dlp.exe on a cp874 machine - only this flag works.
		"--encoding", "utf-8",

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

	// Carry the browser's identity so gated streams resolve the same way.
	//
	// The extension is the policy layer for credentials: its cookie allowlist
	// decides whether any cookie was collected in the first place. A cookie that
	// reaches us was therefore deliberate, and we honour it even on YouTube -
	// otherwise adding youtube.com to the allowlist would silently do nothing.
	//
	// With no cookie, YouTube still gets nothing at all: forwarding a browser
	// User-Agent and Referer on their own is what trips its anti-bot check.
	//
	// The three headers travel together or not at all. A session cookie is bound
	// to the client that minted it, so pairing one with a mismatched User-Agent
	// is itself a bot signal.
	if req.Cookie != "" || sendBrowserIdentity(target) {
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
		log.Printf("job %s: YouTube and no allowlisted cookie - sending no browser identity", job.ID)
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
	// A post-processor announces the file it is creating, which supersedes the
	// per-track download destination: --extract-audio writes "Clip.mp3" and
	// deletes the "Clip.webm" the download step named. Any tag other than
	// [download] is a post-processor, so match them all rather than keeping a
	// list that goes stale with every yt-dlp release.
	postDestinationRe = regexp.MustCompile(`^\[(?:[A-Za-z0-9_]+)\]\s+Destination:\s+(.+)$`)
	// "[MoveFiles] Moving file "C:\tmp\x.mp4" to "C:\dl\x.mp4"" - the last
	// word on where a file ended up, and the one line that can move it out of
	// the directory we would otherwise search.
	moveFilesRe = regexp.MustCompile(`^\[MoveFiles\]\s+Moving file\s+"(.+)"\s+to\s+"(.+)"$`)
	// "[FixupM3u8] Fixing MPEG-TS in MP4 container of "C:\dl\x.mp4""
	fixupRe = regexp.MustCompile(`^\[Fixup\w*\]\s+.*\bof\s+"(.+)"$`)
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

// normalizeJobPath cleans a path scraped from yt-dlp's output against the
// job's own directory, so that everything downstream - the snapshot the popup
// renders, and the verification below - deals in one spelling.
func (m *Manager) normalizeJobPath(job *Job, raw string) string {
	return normalizePath(job.Dir(), raw)
}

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
	// A build that ignores --encoding can emit code-page bytes, which are not
	// valid UTF-8. Left alone they would travel into the WebSocket JSON and the
	// log as broken text; replaced, they at least compare consistently.
	line = strings.ToValidUTF8(line, "\uFFFD")

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
		job.setFinalPath(m.normalizeJobPath(job, mm[1]), true)
		job.setPhase("merging")
		m.Broadcast()
		return
	}
	if mm := moveFilesRe.FindStringSubmatch(line); len(mm) == 3 {
		job.setFinalPath(m.normalizeJobPath(job, mm[2]), true)
		m.Broadcast()
		return
	}
	if mm := fixupRe.FindStringSubmatch(line); len(mm) == 2 {
		job.setFinalPath(m.normalizeJobPath(job, mm[1]), true)
		m.Broadcast()
		return
	}
	// [download] is the per-track downloader; every other tag is a
	// post-processor, whose destination replaces what the download step named.
	if mm := destinationRe.FindStringSubmatch(line); len(mm) == 2 {
		// Each Destination line starts a new track.
		job.beginStream()
		job.setFinalPath(m.normalizeJobPath(job, mm[1]), false)
		m.Broadcast()
		return
	}
	if mm := postDestinationRe.FindStringSubmatch(line); len(mm) == 2 {
		job.setFinalPath(m.normalizeJobPath(job, mm[1]), true)
		m.Broadcast()
		return
	}
	if mm := formatPlanRe.FindStringSubmatch(line); len(mm) == 2 {
		job.setStreamPlan(countTracks(mm[1]))
		return
	}
	if mm := alreadyRe.FindStringSubmatch(line); len(mm) == 2 {
		job.setFinalPath(m.normalizeJobPath(job, mm[1]), true)
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

// sendBrowserIdentity reports whether the browser's UA and Referer should be
// forwarded for this URL in the ABSENCE of a cookie. An explicitly allowlisted
// cookie overrides this decision - see ytDlpArgs.
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
