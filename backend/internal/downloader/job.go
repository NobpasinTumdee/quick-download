package downloader

import (
	"context"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// State is the lifecycle of a download.
type State string

const (
	StateQueued      State = "queued"
	StateProbing     State = "probing"
	StateDownloading State = "downloading"
	StateMerging     State = "merging"
	StateCompleted   State = "completed"
	StateFailed      State = "failed"
	StateCanceled    State = "canceled"
)

// Request is what the extension (or the GUI) asks us to download.
type Request struct {
	URL      string `json:"url"`
	Filename string `json:"filename,omitempty"`
	Referrer string `json:"referrer,omitempty"`
	Cookie   string `json:"cookie,omitempty"`
	// Kind is the extension's classification hint ("hls", "dash", "site",
	// "direct"). It wins over our own guess because the extension saw the
	// live Content-Type that the URL alone does not reveal.
	Kind string `json:"kind,omitempty"`
	// Mime is the observed Content-Type, used when Kind is absent.
	Mime string `json:"mime,omitempty"`
	// Title is the page/media title, used to name streaming downloads.
	Title string `json:"title,omitempty"`
	// UserAgent should mirror the browser's UA so that servers which gate on
	// it hand us the same bytes they handed the page.
	UserAgent string `json:"userAgent,omitempty"`
}

// Chunk is one byte range handled by exactly one goroutine.
type Chunk struct {
	Index int
	Start int64 // inclusive
	End   int64 // inclusive

	// downloaded is written by the chunk's own goroutine and read by the
	// progress ticker, hence atomic. It is the single source of truth for
	// progress: the job total is the sum over chunks, which makes retries
	// (where a chunk resumes mid-way) impossible to double count.
	downloaded atomic.Int64
	done       atomic.Bool
}

// Size is the total number of bytes this chunk must fetch.
func (c *Chunk) Size() int64 { return c.End - c.Start + 1 }

// Job is one file being downloaded.
type Job struct {
	ID  string
	URL string

	mu              sync.RWMutex
	filename        string
	finalPath       string
	size            int64
	resumable       bool
	state           State
	errMsg          string
	finalPathLocked bool
	mime            string
	speed           float64 // bytes/sec, exponentially smoothed
	createdAt       time.Time
	startedAt       time.Time
	finishedAt      time.Time
	chunks          []*Chunk
	req             Request
	cancel          context.CancelFunc

	// Engine dispatch and streaming metadata.
	engine string // EngineHTTP or EngineYtDlp
	kind   Kind
	title  string
	phase  string

	// External progress, owned by the yt-dlp engine. When engine != http these
	// values are authoritative and the chunk counters stay empty, because a
	// muxed stream has no byte ranges to speak of.
	ext          externalProgress
	extSeen      bool // did --progress-template ever produce a line?
	extComplete  bool
	externalProc *exec.Cmd

	// yt-dlp downloads each track separately (video, then audio) and its
	// percent restarts at 0 for every one. streamIndex/streamTotal fold those
	// passes into a single monotonic bar.
	streamIndex int // 1-based; 0 until the first Destination line
	streamTotal int // 1 unless the format line says otherwise

	// lastBytes/lastTick belong to the progress ticker only.
	lastBytes int64
	lastTick  time.Time
}

// externalProgress is one progress update from an external downloader.
type externalProgress struct {
	downloaded int64
	total      int64
	percent    float64 // -1 when unknown
	speed      float64 // bytes/sec, -1 when unknown
	eta        float64 // seconds, -1 when unknown
	phase      string
}

// setStreamPlan records how many separate tracks yt-dlp will fetch.
func (j *Job) setStreamPlan(total int) {
	if total < 1 {
		return
	}
	j.mu.Lock()
	j.streamTotal = total
	j.mu.Unlock()
}

// beginStream advances to the next track (called on each Destination line).
func (j *Job) beginStream() {
	j.mu.Lock()
	j.streamIndex++
	if j.streamTotal < j.streamIndex {
		j.streamTotal = j.streamIndex
	}
	j.mu.Unlock()
}

// overallPercentLocked maps a per-track percent onto the whole job.
// Track 2 of 2 at 50% is 75% overall, so the bar only ever moves forward.
func (j *Job) overallPercentLocked(trackPercent float64) float64 {
	total := j.streamTotal
	if total < 1 {
		total = 1
	}
	index := j.streamIndex
	if index < 1 {
		index = 1
	}
	if index > total {
		index = total
	}
	overall := (float64(index-1) + trackPercent/100) / float64(total) * 100
	if overall > 100 {
		return 100
	}
	if overall < 0 {
		return 0
	}
	return overall
}

// applyExternalProgress records an update from yt-dlp.
func (j *Job) applyExternalProgress(p externalProgress) {
	j.mu.Lock()
	defer j.mu.Unlock()

	// The first machine-readable line is authoritative: it takes over from the
	// scraped percent, which may have been anywhere. Only from then on does the
	// bar become monotonic.
	firstTemplateLine := !j.extSeen
	j.extSeen = true
	if p.downloaded >= 0 {
		j.ext.downloaded = p.downloaded
	}
	if p.total > 0 && j.streamTotal <= 1 {
		// With several tracks the per-track total is not the file size, so we
		// only publish it when yt-dlp is fetching a single stream.
		j.ext.total = p.total
		j.size = p.total
	}
	if p.percent >= 0 {
		next := j.overallPercentLocked(p.percent)
		// Never let a new track's restart drag the bar backwards.
		if firstTemplateLine || next > j.ext.percent {
			j.ext.percent = next
		}
	}
	// yt-dlp reports its own rate; trust it over our sampling.
	if p.speed >= 0 {
		j.ext.speed = p.speed
		j.speed = p.speed
	}
	j.ext.eta = p.eta
	if p.phase != "" {
		j.phase = p.phase
	}
}

// setExternalPercent is the fallback path for builds without --progress-template.
func (j *Job) setExternalPercent(pct float64) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if next := j.overallPercentLocked(pct); next > j.ext.percent {
		j.ext.percent = next
	}
}

// hasTemplateProgress reports whether machine-readable progress arrived, so the
// legacy scraper knows to stay out of the way.
func (j *Job) hasTemplateProgress() bool {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.extSeen
}

// markExternalComplete pins the job at 100% once yt-dlp exits successfully.
func (j *Job) markExternalComplete() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.extComplete = true
	j.ext.percent = 100
	j.ext.eta = 0
	j.speed = 0
}

func (j *Job) setPhase(phase string) {
	j.mu.Lock()
	j.phase = phase
	j.mu.Unlock()
}

// setFinalPath records where the file landed. authoritative marks the muxed
// output, which must not be overwritten by a later per-stream Destination line.
func (j *Job) setFinalPath(path string, authoritative bool) {
	if path == "" {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.finalPathLocked && !authoritative {
		return
	}
	j.finalPath = path
	j.filename = filepath.Base(path)
	if authoritative {
		j.finalPathLocked = true
	}
}

// setProcess remembers the running child so Cancel can kill its whole tree.
func (j *Job) setProcess(cmd *exec.Cmd) {
	j.mu.Lock()
	j.externalProc = cmd
	j.mu.Unlock()
}

// Downloaded is the sum of every chunk counter.
func (j *Job) Downloaded() int64 {
	j.mu.RLock()
	chunks := j.chunks
	external := j.engine != "" && j.engine != EngineHTTP
	extBytes := j.ext.downloaded
	j.mu.RUnlock()

	if external {
		return extBytes
	}
	var total int64
	for _, c := range chunks {
		total += c.downloaded.Load()
	}
	return total
}

func (j *Job) setState(s State) {
	j.mu.Lock()
	j.state = s
	if s == StateDownloading && j.startedAt.IsZero() {
		j.startedAt = time.Now()
	}
	if s == StateCompleted || s == StateFailed || s == StateCanceled {
		j.finishedAt = time.Now()
	}
	j.mu.Unlock()
}

func (j *Job) State() State {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.state
}

func (j *Job) fail(err error) {
	j.mu.Lock()
	j.state = StateFailed
	j.errMsg = err.Error()
	j.finishedAt = time.Now()
	j.mu.Unlock()
}

func (j *Job) setCancel(fn context.CancelFunc) {
	j.mu.Lock()
	j.cancel = fn
	j.mu.Unlock()
}

// Cancel stops the job; in-flight HTTP reads abort through the context and any
// external downloader is killed along with its children.
func (j *Job) Cancel() {
	j.mu.Lock()
	fn := j.cancel
	proc := j.externalProc
	switch j.state {
	case StateCompleted, StateFailed, StateCanceled:
		// already terminal - nothing to do
	default:
		j.state = StateCanceled
		j.finishedAt = time.Now()
	}
	j.mu.Unlock()
	if fn != nil {
		fn()
	}
	// CommandContext kills the child on ctx cancel, but only the child: yt-dlp
	// spawns ffmpeg, so we take out the whole tree explicitly.
	if proc != nil {
		_ = killProcessTree(proc)
	}
}

// ChunkSnapshot is the per-connection view sent to the GUI.
type ChunkSnapshot struct {
	Index      int     `json:"index"`
	Start      int64   `json:"start"`
	End        int64   `json:"end"`
	Total      int64   `json:"total"`
	Downloaded int64   `json:"downloaded"`
	Progress   float64 `json:"progress"`
	Done       bool    `json:"done"`
}

// JobSnapshot is the JSON contract with the GUI. Keep it flat and cheap to
// serialise: we broadcast it several times per second.
type JobSnapshot struct {
	ID          string          `json:"id"`
	URL         string          `json:"url"`
	Filename    string          `json:"filename"`
	Path        string          `json:"path"`
	State       State           `json:"state"`
	Error       string          `json:"error,omitempty"`
	Mime        string          `json:"mime,omitempty"`
	Engine      string          `json:"engine"`
	Kind        string          `json:"kind"`
	Title       string          `json:"title,omitempty"`
	Phase       string          `json:"phase,omitempty"`
	Size        int64           `json:"size"`
	Downloaded  int64           `json:"downloaded"`
	Progress    float64         `json:"progress"` // 0..100
	Speed       float64         `json:"speed"`    // bytes/sec
	ETA         float64         `json:"eta"`      // seconds, -1 when unknown
	Connections int             `json:"connections"`
	Resumable   bool            `json:"resumable"`
	CreatedAt   int64           `json:"createdAt"`
	FinishedAt  int64           `json:"finishedAt,omitempty"`
	Chunks      []ChunkSnapshot `json:"chunks"`
}

// Snapshot renders the job for the API/WebSocket.
func (j *Job) Snapshot() JobSnapshot {
	j.mu.RLock()
	defer j.mu.RUnlock()

	s := JobSnapshot{
		ID:          j.ID,
		URL:         j.URL,
		Engine:      j.engine,
		Kind:        string(j.kind),
		Title:       j.title,
		Phase:       j.phase,
		Filename:    j.filename,
		Path:        j.finalPath,
		State:       j.state,
		Error:       j.errMsg,
		Mime:        j.mime,
		Size:        j.size,
		Speed:       j.speed,
		ETA:         -1,
		Connections: len(j.chunks),
		Resumable:   j.resumable,
		CreatedAt:   j.createdAt.UnixMilli(),
	}
	if !j.finishedAt.IsZero() {
		s.FinishedAt = j.finishedAt.UnixMilli()
	}

	var done int64
	s.Chunks = make([]ChunkSnapshot, 0, len(j.chunks))
	for _, c := range j.chunks {
		d := c.downloaded.Load()
		done += d
		cs := ChunkSnapshot{
			Index:      c.Index,
			Start:      c.Start,
			End:        c.End,
			Total:      c.Size(),
			Downloaded: d,
			Done:       c.done.Load(),
		}
		if cs.Total > 0 {
			cs.Progress = float64(d) / float64(cs.Total) * 100
		}
		s.Chunks = append(s.Chunks, cs)
	}
	s.Downloaded = done

	// Streaming jobs have no chunks: yt-dlp reports its own numbers.
	if j.engine == EngineYtDlp {
		s.Downloaded = j.ext.downloaded
		s.Connections = 0
		switch {
		case j.state == StateCompleted || j.extComplete:
			s.Progress = 100
		case j.ext.percent >= 0:
			s.Progress = j.ext.percent
		}
		if j.ext.eta > 0 && j.state == StateDownloading {
			s.ETA = j.ext.eta
		}
		return s
	}

	switch {
	case j.state == StateCompleted:
		s.Progress = 100
	case j.size > 0:
		s.Progress = float64(done) / float64(j.size) * 100
		if s.Progress > 100 {
			s.Progress = 100
		}
		if j.speed > 1 && j.state == StateDownloading {
			s.ETA = float64(j.size-done) / j.speed
		}
	}
	return s
}
