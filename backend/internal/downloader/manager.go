package downloader

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/quick-download/backend/internal/config"
)

// EventSink receives snapshots to push to connected GUIs.
type EventSink interface {
	Publish(v any)
}

// Manager owns the job table, the worker pool and the progress ticker.
type Manager struct {
	cfg    *config.Config
	sink   EventSink
	client *http.Client

	mu    sync.RWMutex
	jobs  map[string]*Job
	order []string // insertion order, newest last

	queue chan *Job
}

// NewManager builds a manager with a connection-pooled HTTP client.
func NewManager(cfg *config.Config, sink EventSink) *Manager {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		// One idle connection per parallel chunk, otherwise Go tears down and
		// re-dials sockets between retries and throughput suffers.
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   cfg.MaxChunks * 2,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 2 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &Manager{
		cfg:  cfg,
		sink: sink,
		// No client-level Timeout: a large file legitimately takes minutes.
		// Cancellation is handled per request through the job context.
		client: &http.Client{Transport: transport},
		jobs:   make(map[string]*Job),
		queue:  make(chan *Job, 256),
	}
}

// Run starts the worker pool and the progress broadcaster. It blocks until ctx
// is cancelled.
func (m *Manager) Run(ctx context.Context) {
	for i := 0; i < m.cfg.MaxConcurrentJobs; i++ {
		go m.worker(ctx, i)
	}

	ticker := time.NewTicker(400 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if m.refreshSpeeds() {
				m.Broadcast()
			}
		}
	}
}

func (m *Manager) worker(ctx context.Context, id int) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-m.queue:
			if job.State() == StateCanceled {
				continue
			}
			log.Printf("worker %d: starting %s", id, job.URL)
			m.execute(ctx, job)
			m.Broadcast()
		}
	}
}

// Enqueue validates the request, registers the job and schedules it.
func (m *Manager) Enqueue(req Request) (*Job, error) {
	u, err := url.Parse(req.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("unsupported or malformed URL: %q", req.URL)
	}

	// A bad save path must fail loudly here rather than silently writing the
	// file somewhere the user is not expecting.
	dir, err := resolveDownloadDir(m.cfg.DownloadDir, req.SavePath)
	if err != nil {
		return nil, err
	}

	// Classify up front so the dashboard can show the right engine badge while
	// the job is still queued.
	kind := Classify(u.String(), req.Mime, req.Kind)

	job := &Job{
		ID:        newID(),
		URL:       u.String(),
		state:     StateQueued,
		createdAt: time.Now(),
		req:       req,
		filename:  sanitizeFilename(req.Filename),
		engine:    kind.Engine(),
		kind:      kind,
		dir:       dir,
		title:     strings.TrimSpace(req.Title),
	}
	job.ext.percent = -1
	job.ext.eta = -1

	// A streaming job has no name until yt-dlp resolves one; show something
	// meaningful in the meantime.
	if job.filename == "" && kind.Streaming() {
		job.filename = placeholderName(u, job.title)
	}

	m.mu.Lock()
	m.jobs[job.ID] = job
	m.order = append(m.order, job.ID)
	m.mu.Unlock()

	select {
	case m.queue <- job:
	default:
		job.fail(errors.New("queue is full"))
	}
	m.Broadcast()
	return job, nil
}

// Get returns a job by id.
func (m *Manager) Get(id string) (*Job, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	j, ok := m.jobs[id]
	return j, ok
}

// Cancel stops a running or queued job.
func (m *Manager) Cancel(id string) bool {
	j, ok := m.Get(id)
	if !ok {
		return false
	}
	j.Cancel()
	m.Broadcast()
	return true
}

// Retry re-queues a failed/cancelled job as a brand new one.
func (m *Manager) Retry(id string) (*Job, error) {
	j, ok := m.Get(id)
	if !ok {
		return nil, errors.New("no such job")
	}
	j.mu.RLock()
	req := j.req
	j.mu.RUnlock()
	return m.Enqueue(req)
}

// ClearFinished drops terminal jobs from the table.
func (m *Manager) ClearFinished() int {
	m.mu.Lock()
	kept := m.order[:0]
	removed := 0
	for _, id := range m.order {
		switch m.jobs[id].State() {
		case StateCompleted, StateFailed, StateCanceled:
			delete(m.jobs, id)
			removed++
		default:
			kept = append(kept, id)
		}
	}
	m.order = append([]string(nil), kept...)
	m.mu.Unlock()
	m.Broadcast()
	return removed
}

// Snapshot renders every job, newest first.
func (m *Manager) Snapshot() []JobSnapshot {
	m.mu.RLock()
	jobs := make([]*Job, 0, len(m.order))
	for _, id := range m.order {
		if j, ok := m.jobs[id]; ok {
			jobs = append(jobs, j)
		}
	}
	m.mu.RUnlock()

	out := make([]JobSnapshot, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, j.Snapshot())
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].CreatedAt > out[b].CreatedAt })
	return out
}

// Broadcast pushes the current state to every GUI client.
func (m *Manager) Broadcast() {
	if m.sink == nil {
		return
	}
	m.sink.Publish(map[string]any{
		"type": "snapshot",
		"jobs": m.Snapshot(),
	})
}

// refreshSpeeds recomputes the smoothed transfer rate of active jobs and
// reports whether anything is worth broadcasting.
func (m *Manager) refreshSpeeds() bool {
	m.mu.RLock()
	jobs := make([]*Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		jobs = append(jobs, j)
	}
	m.mu.RUnlock()

	active := false
	now := time.Now()
	for _, j := range jobs {
		switch j.State() {
		case StateDownloading, StateProbing, StateMerging, StateQueued:
			active = true
		default:
			continue
		}
		bytes := j.Downloaded()
		j.mu.Lock()
		if j.engine == EngineYtDlp {
			// yt-dlp publishes an authoritative rate; sampling it again would
			// fight with the values arriving from --progress-template.
			j.lastBytes, j.lastTick = bytes, now
			j.mu.Unlock()
			continue
		}
		if !j.lastTick.IsZero() {
			elapsed := now.Sub(j.lastTick).Seconds()
			if elapsed > 0 {
				instant := float64(bytes-j.lastBytes) / elapsed
				if j.speed == 0 {
					j.speed = instant
				} else {
					// Exponential moving average: smooth enough for a UI,
					// responsive enough to show a stall.
					j.speed = 0.7*j.speed + 0.3*instant
				}
				if j.speed < 0 {
					j.speed = 0
				}
			}
		}
		j.lastBytes = bytes
		j.lastTick = now
		j.mu.Unlock()
	}
	return active
}

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// placeholderName gives a streaming job a readable label before yt-dlp reports
// the real output filename.
func placeholderName(u *url.URL, title string) string {
	if title != "" {
		if s := sanitizeFilename(title); s != "" {
			return s
		}
	}
	host := u.Hostname()
	if host == "" {
		host = "stream"
	}
	return host + " (resolving…)"
}
