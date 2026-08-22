// Package server exposes the local control plane: a small JSON API, the
// WebSocket progress feed and the embedded GUI.
//
// Everything binds to 127.0.0.1 only. On top of that, the WebSocket endpoint
// checks the Origin header, because any web page you visit can open a
// WebSocket to a loopback port; the same-origin policy does not stop it.
package server

import (
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/quick-download/backend/internal/config"
	"github.com/quick-download/backend/internal/downloader"
)

// Server wires the manager, the hub and the static GUI together.
type Server struct {
	cfg *config.Config
	mgr *downloader.Manager
	hub *Hub
	web fs.FS
}

// New builds the server. web is the embedded GUI file system (may be nil).
func New(cfg *config.Config, mgr *downloader.Manager, hub *Hub, web fs.FS) *Server {
	return &Server{cfg: cfg, mgr: mgr, hub: hub, web: web}
}

// Handler returns the fully routed http.Handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("/api/tools", s.handleTools)
	mux.HandleFunc("/api/downloads", s.handleDownloads)
	mux.HandleFunc("/api/enqueue", s.handleEnqueue)
	mux.HandleFunc("/api/cancel", s.handleCancel)
	mux.HandleFunc("/api/retry", s.handleRetry)
	mux.HandleFunc("/api/clear", s.handleClear)
	mux.HandleFunc("/api/reveal", s.handleReveal)
	mux.HandleFunc("/ws", s.handleWS)

	if s.web != nil {
		mux.Handle("/", http.FileServer(http.FS(s.web)))
	}
	return s.withCommonHeaders(mux)
}

// withCommonHeaders adds permissive CORS for the extension (which lives on a
// chrome-extension:// origin) and blocks non-loopback Host headers, which is a
// cheap defence against DNS rebinding.
func (s *Server) withCommonHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if i := strings.LastIndex(host, ":"); i > 0 {
			host = host[:i]
		}
		host = strings.Trim(host, "[]")
		switch host {
		case "127.0.0.1", "localhost", "::1", "":
		default:
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}

		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// JSON API
// ---------------------------------------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":      "ok",
		"version":     config.Version,
		"downloadDir": s.cfg.DownloadDir,
		"maxChunks":   s.cfg.MaxChunks,
		"toolsReady":  s.cfg.Tools().Ready(),
	})
}

// handleTools reports whether yt-dlp and ffmpeg are available. Resolution runs
// per request, so dropping the binaries into bin/ takes effect immediately.
func (s *Server) handleTools(w http.ResponseWriter, r *http.Request) {
	tools := s.cfg.Tools()
	writeJSON(w, http.StatusOK, map[string]any{
		"ytdlp":      tools.YtDlp,
		"ffmpeg":     tools.Ffmpeg,
		"ready":      tools.Ready(),
		"searchedIn": tools.SearchedIn,
	})
}

func (s *Server) handleDownloads(w http.ResponseWriter, r *http.Request) {
	// Polling fallback for clients that cannot use WebSockets.
	writeJSON(w, http.StatusOK, map[string]any{
		"type": "snapshot",
		"jobs": s.mgr.Snapshot(),
	})
}

func (s *Server) handleEnqueue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var req downloader.Request
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	job, err := s.mgr.Enqueue(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "id": job.ID})
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	id := jobID(w, r)
	if !s.mgr.Cancel(id) {
		writeErr(w, http.StatusNotFound, "no such job")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleRetry(w http.ResponseWriter, r *http.Request) {
	job, err := s.mgr.Retry(jobID(w, r))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "id": job.ID})
}

func (s *Server) handleClear(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": s.mgr.ClearFinished()})
}

// handleReveal opens the OS file manager on the finished file.
func (s *Server) handleReveal(w http.ResponseWriter, r *http.Request) {
	job, ok := s.mgr.Get(jobID(w, r))
	if !ok {
		writeErr(w, http.StatusNotFound, "no such job")
		return
	}
	path := job.Snapshot().Path
	if path == "" {
		writeErr(w, http.StatusBadRequest, "file not written yet")
		return
	}
	if err := reveal(path); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func reveal(path string) error {
	switch runtime.GOOS {
	case "windows":
		// explorer.exe exits with code 1 even on success, so ignore the error.
		_ = exec.Command("explorer.exe", "/select,"+filepath.Clean(path)).Start()
		return nil
	case "darwin":
		return exec.Command("open", "-R", path).Start()
	default:
		return exec.Command("xdg-open", filepath.Dir(path)).Start()
	}
}

func jobID(w http.ResponseWriter, r *http.Request) string {
	if id := r.URL.Query().Get("id"); id != "" {
		return id
	}
	var body struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body)
	return body.ID
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"ok": false, "error": msg})
}

// ---------------------------------------------------------------------------
// WebSocket endpoint + hub
// ---------------------------------------------------------------------------

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	if !originAllowed(r.Header.Get("Origin"), s.cfg.Port) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	conn, err := upgrade(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	client := s.hub.add(conn)

	// Send the current state immediately so a fresh tab is never blank.
	if payload, err := json.Marshal(map[string]any{
		"type": "snapshot",
		"jobs": s.mgr.Snapshot(),
	}); err == nil {
		client.enqueue(payload)
	}

	go client.writeLoop()
	go client.readLoop(s.hub)
}

// originAllowed permits only the dashboard itself and the extension pages.
// An empty Origin (a native client, curl) is allowed; a random website is not.
func originAllowed(origin string, port int) bool {
	if origin == "" {
		return true
	}
	if strings.HasPrefix(origin, "chrome-extension://") ||
		strings.HasPrefix(origin, "moz-extension://") {
		return true
	}
	for _, host := range []string{"127.0.0.1", "localhost", "[::1]"} {
		if origin == "http://"+host+portSuffix(port) {
			return true
		}
	}
	return false
}

func portSuffix(port int) string {
	if port == 80 {
		return ""
	}
	return ":" + itoa(port)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// Hub fans one message out to every connected GUI.
type Hub struct {
	mu      sync.RWMutex
	clients map[*client]struct{}
}

// NewHub creates an empty hub.
func NewHub() *Hub { return &Hub{clients: make(map[*client]struct{})} }

// Publish marshals v once and pushes it to all clients.
// It satisfies downloader.EventSink.
func (h *Hub) Publish(v any) {
	payload, err := json.Marshal(v)
	if err != nil {
		log.Printf("hub: marshal: %v", err)
		return
	}
	h.mu.RLock()
	clients := make([]*client, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.RUnlock()

	for _, c := range clients {
		c.enqueue(payload)
	}
}

// Count reports how many GUIs are connected.
func (h *Hub) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

func (h *Hub) add(conn *wsConn) *client {
	c := &client{conn: conn, send: make(chan []byte, 32), done: make(chan struct{})}
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	return c
}

func (h *Hub) remove(c *client) {
	h.mu.Lock()
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
		close(c.done)
	}
	h.mu.Unlock()
	_ = c.conn.close()
}

// client is one connected dashboard.
type client struct {
	conn *wsConn
	send chan []byte
	done chan struct{}
	once sync.Once
}

// enqueue never blocks: a dashboard that cannot keep up simply drops frames.
// Progress messages are absolute snapshots, so dropping one is harmless.
func (c *client) enqueue(payload []byte) {
	select {
	case c.send <- payload:
	default:
	}
}

func (c *client) writeLoop() {
	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-c.done:
			return
		case payload := <-c.send:
			if err := c.conn.writeFrame(opText, payload); err != nil {
				c.shutdown()
				return
			}
		case <-ping.C:
			if err := c.conn.writeFrame(opPing, nil); err != nil {
				c.shutdown()
				return
			}
		}
	}
}

// readLoop drains incoming frames. The dashboard sends nothing meaningful, but
// we must answer pings and honour close frames to shut down cleanly.
func (c *client) readLoop(h *Hub) {
	defer h.remove(c)
	for {
		opcode, payload, err := c.conn.readFrame()
		if err != nil {
			return
		}
		switch opcode {
		case opClose:
			_ = c.conn.writeFrame(opClose, payload)
			return
		case opPing:
			_ = c.conn.writeFrame(opPong, payload)
		case opPong, opText, opBinary, opContinuation:
			// nothing to do
		}
	}
}

func (c *client) shutdown() {
	c.once.Do(func() { _ = c.conn.close() })
}
