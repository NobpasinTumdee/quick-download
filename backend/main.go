// Command quick-download is both halves of the engine, selected by argv:
//
//	quick-download            -> Native Messaging host mode (Chrome starts it)
//	quick-download --daemon   -> the download engine + local API/GUI server
//
// Why two modes in one binary?
//
// chrome.runtime.sendNativeMessage() spawns a FRESH host process for every
// single message and kills it as soon as the reply arrives. That lifetime is
// useless for a downloader. So the host process is a thin, short-lived relay:
// it makes sure the long-lived daemon is running, forwards the job to it over
// the loopback API, answers Chrome, and exits. The daemon keeps downloading
// long after Chrome has reaped the host process, and the dashboard talks
// straight to it.
package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/quick-download/backend/internal/config"
	"github.com/quick-download/backend/internal/downloader"
	"github.com/quick-download/backend/internal/nativemsg"
	"github.com/quick-download/backend/internal/server"
)

//go:embed all:web
var webFS embed.FS

func main() {
	cfg := config.Load()

	daemon := false
	for _, a := range os.Args[1:] {
		switch a {
		case "--daemon", "-daemon":
			daemon = true
		case "--version", "-version", "-v":
			fmt.Println("quick-download " + config.Version)
			return
		case "--help", "-h":
			usage()
			return
		}
	}

	if daemon {
		runDaemon(cfg)
		return
	}
	runHost(cfg)
}

func usage() {
	fmt.Println(`quick-download ` + config.Version + `

  quick-download              run as a Chrome Native Messaging host (stdin/stdout)
  quick-download --daemon     run the download engine and the local dashboard
  quick-download --version    print the version

Environment overrides:
  QD_PORT    local server port (default 9090)
  QD_DIR     download directory (default ~/Downloads)
  QD_CHUNKS  max parallel connections per file (default 8, max 16)
  QD_JOBS    max simultaneous downloads (default 3)`)
}

// ---------------------------------------------------------------------------
// Daemon mode
// ---------------------------------------------------------------------------

func runDaemon(cfg *config.Config) {
	logFile, err := cfg.OpenLog("daemon")
	if err == nil {
		defer logFile.Close()
		log.SetOutput(logFile)
	}
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[daemon] ")

	// Binding the port IS the single-instance lock: if another daemon already
	// owns it, this process simply exits and the caller uses the running one.
	ln, err := net.Listen("tcp", cfg.Addr())
	if err != nil {
		log.Printf("port %d already in use, assuming another instance is live: %v", cfg.Port, err)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	hub := server.NewHub()
	mgr := downloader.NewManager(cfg, hub)
	go mgr.Run(ctx)

	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Printf("embedded GUI unavailable: %v", err)
		sub = nil
	}

	httpSrv := &http.Server{
		Handler: server.New(cfg, mgr, hub, sub).Handler(),
		// No WriteTimeout: it would kill hijacked WebSocket connections.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		<-ctx.Done()
		log.Println("shutting down")
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()

	log.Printf("engine listening on %s, saving to %s", cfg.BaseURL(), cfg.DownloadDir)
	if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
		log.Printf("server stopped: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Native Messaging host mode
// ---------------------------------------------------------------------------

// hostRequest is what the extension sends us.
type hostRequest struct {
	Type      string `json:"type"` // download | ping | status | open_dashboard
	URL       string `json:"url,omitempty"`
	Filename  string `json:"filename,omitempty"`
	Referrer  string `json:"referrer,omitempty"`
	Cookie    string `json:"cookie,omitempty"`
	UserAgent string `json:"userAgent,omitempty"`
	RequestID string `json:"requestId,omitempty"`
	// Streaming hints observed by the extension: it saw the live Content-Type,
	// which a bare URL does not reveal.
	Kind  string `json:"kind,omitempty"`
	Mime  string `json:"mime,omitempty"`
	Title string `json:"title,omitempty"`
}

// hostResponse is what we send back. Chrome delivers it to the extension's
// sendNativeMessage callback.
type hostResponse struct {
	OK        bool   `json:"ok"`
	Type      string `json:"type"`
	Error     string `json:"error,omitempty"`
	JobID     string `json:"jobId,omitempty"`
	RequestID string `json:"requestId,omitempty"`
	Dashboard string `json:"dashboard,omitempty"`
	Version   string `json:"version,omitempty"`
	// ToolsReady is false when yt-dlp/ffmpeg are missing, so the popup can warn
	// before the user tries a stream.
	ToolsReady bool `json:"toolsReady"`
}

func runHost(cfg *config.Config) {
	// CRITICAL: stdout is the message channel. Anything else printed there
	// corrupts the framing and Chrome kills us. Every log line goes to a file.
	logFile, err := cfg.OpenLog("host")
	if err == nil {
		defer logFile.Close()
		log.SetOutput(logFile)
	} else {
		log.SetOutput(io.Discard)
	}
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[host] ")
	log.Printf("started, argv=%v", os.Args)

	in := nativemsg.NewReader(os.Stdin)
	out := nativemsg.NewWriter(os.Stdout)

	client := &http.Client{Timeout: 10 * time.Second}

	for {
		raw, err := in.ReadRaw()
		if err != nil {
			// io.EOF is the normal "Chrome closed the port" signal.
			if err == io.EOF {
				log.Println("stdin closed, exiting cleanly")
				return
			}
			log.Printf("read error, exiting: %v", err)
			return
		}

		var req hostRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			// A malformed message must not kill the pipe: report and continue.
			log.Printf("bad message: %v", err)
			reply(out, hostResponse{Type: "error", Error: "malformed request: " + err.Error()})
			continue
		}

		reply(out, handle(cfg, client, req))
	}
}

// reply writes a framed response, tolerating a broken pipe.
func reply(out *nativemsg.Writer, resp hostResponse) {
	if err := out.Write(resp); err != nil {
		log.Printf("write error: %v", err)
	}
}

// handle turns one extension message into one response, never panicking.
func handle(cfg *config.Config, client *http.Client, req hostRequest) (resp hostResponse) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("panic while handling %q: %v", req.Type, r)
			resp = hostResponse{Type: req.Type, RequestID: req.RequestID,
				Error: fmt.Sprintf("internal error: %v", r)}
		}
	}()

	base := hostResponse{Type: req.Type, RequestID: req.RequestID, Dashboard: cfg.BaseURL()}

	switch req.Type {
	case "ping", "status", "":
		if err := ensureDaemon(cfg, client); err != nil {
			base.Error = err.Error()
			return base
		}
		base.OK = true
		base.Version = config.Version
		base.ToolsReady = cfg.Tools().Ready()
		return base

	case "open_dashboard":
		if err := ensureDaemon(cfg, client); err != nil {
			base.Error = err.Error()
			return base
		}
		if err := openBrowser(cfg.BaseURL()); err != nil {
			base.Error = err.Error()
			return base
		}
		base.OK = true
		return base

	case "download":
		if req.URL == "" {
			base.Error = "missing url"
			return base
		}
		if err := ensureDaemon(cfg, client); err != nil {
			base.Error = err.Error()
			return base
		}
		id, err := enqueue(cfg, client, req)
		if err != nil {
			base.Error = err.Error()
			return base
		}
		base.OK = true
		base.JobID = id
		return base

	default:
		base.Error = "unknown message type: " + req.Type
		return base
	}
}

// enqueue POSTs the job to the daemon's local API.
func enqueue(cfg *config.Config, client *http.Client, req hostRequest) (string, error) {
	body, err := json.Marshal(downloader.Request{
		URL:       req.URL,
		Filename:  req.Filename,
		Referrer:  req.Referrer,
		Cookie:    req.Cookie,
		UserAgent: req.UserAgent,
		Kind:      req.Kind,
		Mime:      req.Mime,
		Title:     req.Title,
	})
	if err != nil {
		return "", err
	}

	resp, err := client.Post(cfg.BaseURL()+"/api/enqueue", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("engine unreachable: %w", err)
	}
	defer resp.Body.Close()

	var out struct {
		OK    bool   `json:"ok"`
		ID    string `json:"id"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil {
		return "", fmt.Errorf("engine replied with garbage: %w", err)
	}
	if !out.OK {
		if out.Error == "" {
			out.Error = resp.Status
		}
		return "", fmt.Errorf("engine refused the job: %s", out.Error)
	}
	log.Printf("queued %s as job %s", req.URL, out.ID)
	return out.ID, nil
}

// ensureDaemon returns once a healthy daemon is reachable, starting a detached
// one if necessary.
func ensureDaemon(cfg *config.Config, client *http.Client) error {
	if daemonAlive(cfg, client, 400*time.Millisecond) {
		return nil
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot locate own executable: %w", err)
	}

	cmd := exec.Command(exe, "--daemon")
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	// The daemon must outlive this short-lived host process and must not
	// inherit our stdio (that would leak the Native Messaging pipe into it).
	detach(cmd)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("cannot start engine: %w", err)
	}
	// Never Wait(): we want the child orphaned, not reaped by us.
	log.Printf("spawned engine pid=%d", cmd.Process.Pid)
	_ = cmd.Process.Release()

	// Give it a moment to bind the port.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if daemonAlive(cfg, client, 300*time.Millisecond) {
			return nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	return fmt.Errorf("engine did not come up on %s", cfg.BaseURL())
}

func daemonAlive(cfg *config.Config, client *http.Client, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.BaseURL()+"/api/health", nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode == http.StatusOK
}

func openBrowser(url string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		return exec.Command("open", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}
