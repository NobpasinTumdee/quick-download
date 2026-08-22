# Quick Download

A lightweight, IDM-style universal downloader in three parts: a Chrome MV3 extension
that sniffs media on pages (including HLS/DASH streams hidden behind blob URLs), a Go
engine that fetches direct files over 4–8 parallel HTTP range connections and hands
streams to `yt-dlp` + `ffmpeg`, and a local glassmorphism dashboard that watches both.

No third-party Go modules. No bundler. No cloud anything — the engine binds to
`127.0.0.1` and nothing else. `yt-dlp` and `ffmpeg` are optional external binaries,
needed only for streaming.

**Two engines, one queue:**

| What you clicked | Engine | How |
|---|---|---|
| `.mp4`, `.jpg`, `.zip`, any direct file | built-in | 4–8 parallel HTTP `Range` connections, merged |
| `.m3u8` (HLS), `.mpd` (DASH) | `yt-dlp` | segments fetched and muxed by `ffmpeg` |
| A YouTube/Vimeo/X/… page URL | `yt-dlp` | its extractor finds the real streams |

Both report into the same WebSocket progress feed, so the dashboard shows them
side by side.

```
Chrome page ──webRequest (headers)──▶ Service Worker ──sendNativeMessage──▶ host process
     ▲                                   │      ▲                                │ HTTP
     └── content.ts (thumbnails) ────────┘      │                                ▼
                                          popup.html            ┌──── Go engine (daemon) ─────┐
                                                                │  classify(url, mime, hint)  │
                                                                │    ├─ direct → chunked HTTP │
                                                                │    └─ stream → yt-dlp+ffmpeg│
                                                                │  :9090 API + WS + GUI       │
                                                                └──────────┬──────────────────┘
                                                                           │ ws://127.0.0.1:9090/ws
                                                                           ▼
                                                                     Dashboard (index.html)
```

## Directory structure

```
quick-download/
├── backend/                        # Go engine — no external dependencies
│   ├── main.go                     # mode switch: native host ⇄ daemon
│   ├── detach_windows.go           # spawn the daemon detached (Windows)
│   ├── detach_unix.go              # spawn the daemon detached (Linux/macOS)
│   ├── go.mod
│   ├── internal/
│   │   ├── config/config.go        # ports, folders, chunk tuning, tool discovery
│   │   ├── nativemsg/              # 4-byte little-endian framing + tests
│   │   ├── downloader/
│   │   │   ├── classify.go         # direct vs HLS vs DASH vs site
│   │   │   ├── fetch.go            # chunked HTTP engine
│   │   │   ├── ytdlp.go            # yt-dlp process + progress parsing
│   │   │   ├── proc_windows.go     # kill the yt-dlp/ffmpeg process TREE
│   │   │   ├── proc_unix.go
│   │   │   └── job.go, manager.go  # job model, worker pool (+ tests)
│   │   └── server/                 # JSON API, hand-rolled WebSocket + tests
│   └── web/                        # GUI, embedded into the binary via go:embed
│       ├── index.html
│       ├── style.css
│       └── app.js
├── extension/                      # Chrome MV3 extension (TypeScript)
│   ├── manifest.json
│   ├── rules.json                  # declarativeNetRequest ruleset (off by default)
│   ├── popup.html / popup.css
│   ├── src/                        # TypeScript sources
│   │   ├── background.ts           # service worker: sniffing + native messaging
│   │   ├── content.ts              # injected page scanner (thumbnails)
│   │   ├── popup.ts
│   │   └── types.ts
│   ├── dist/                       # compiled JS (tsc output, what Chrome loads)
│   ├── package.json
│   └── tsconfig.json
├── host/
│   ├── com.downloader.app.json     # host manifest template
│   ├── install-windows.ps1         # build + write manifest + registry keys
│   ├── uninstall-windows.ps1
│   └── install-unix.sh
├── tools/
│   ├── README.md                   # where to put yt-dlp / ffmpeg
│   └── get-tools.ps1               # optional downloader for both (asks first)
├── bin/                            # build output + the external binaries
│   ├── quick-download.exe
│   ├── com.downloader.app.json
│   ├── yt-dlp.exe                  # you supply these two
│   └── ffmpeg.exe
└── build.ps1
```

---

## Install (Windows)

### 1. Build the engine

```powershell
cd T:\GitHub\quick-download
.\build.ps1
```

That produces `bin\quick-download.exe` and compiles the extension's TypeScript
into `extension\dist\`. Needs Go 1.22+ and Node 18+ on `PATH`.

### 2. Load the extension and copy its id

1. Open `chrome://extensions`
2. Turn on **Developer mode** (top right)
3. **Load unpacked** → select the `extension\` folder
4. Copy the **ID** shown on the card — 32 letters, `a`–`p`

### 3. Register the native messaging host

```powershell
cd host
.\install-windows.ps1 -ExtensionId <paste-the-32-char-id>
```

This writes `bin\com.downloader.app.json` with the absolute path to the exe and your
extension id, then creates the registry value Chrome looks for:

```
HKCU\Software\Google\Chrome\NativeMessagingHosts\com.downloader.app
    (Default) = T:\GitHub\quick-download\bin\com.downloader.app.json
```

Edge, Chromium and Brave get the same treatment. `HKCU` means no admin rights.

The equivalent one-liners, if you prefer doing it by hand:

```powershell
# create the key and point it at the manifest
New-Item -Path 'HKCU:\Software\Google\Chrome\NativeMessagingHosts\com.downloader.app' -Force
Set-ItemProperty -Path 'HKCU:\Software\Google\Chrome\NativeMessagingHosts\com.downloader.app' `
                 -Name '(Default)' -Value 'T:\GitHub\quick-download\bin\com.downloader.app.json'
```

```cmd
:: or with reg.exe
reg add "HKCU\Software\Google\Chrome\NativeMessagingHosts\com.downloader.app" /ve /t REG_SZ ^
        /d "T:\GitHub\quick-download\bin\com.downloader.app.json" /f
```

### 4. Add yt-dlp and ffmpeg (needed for streams)

Direct file downloads work without them. HLS, DASH and site pages do not.

```
bin/
├── quick-download.exe
├── com.downloader.app.json
├── yt-dlp.exe     <-- drop here
└── ffmpeg.exe     <-- and here
```

Either download both by hand (see [`tools/README.md`](tools/README.md)) or run:

```powershell
.	ools\get-tools.ps1        # asks before downloading anything
```

Anything on `PATH` works too — `pipx install yt-dlp` plus a package-manager
ffmpeg is fine. The engine re-checks on every request, so you can add them while
it is running. Confirm with <http://127.0.0.1:9090/api/tools>; the dashboard and
the popup both show a warning banner until both are found.

### 5. Restart Chrome completely

Close **every** Chrome window. Chrome caches native host registrations at startup;
a reloaded extension is not enough.

### 6. Use it

- Click the extension icon → every video, stream and image found on the page,
  each with a thumbnail → **Download**
- Right-click a video/image/link → **Download with Quick Download**
- Right-click anywhere on a page → **Download video on this page (yt-dlp)**
- Paste any URL — file, `.m3u8`, or a YouTube link — into the popup or dashboard
- Dashboard: <http://127.0.0.1:9090> (the **Dashboard** button opens it)

Files land in `%USERPROFILE%\Downloads`.

## Install (Linux / macOS)

```bash
cd quick-download
(cd backend && go build -trimpath -o ../bin/quick-download .)
(cd extension && npm install && npm run build)
./host/install-unix.sh <your-32-char-extension-id>
```

No registry — the manifest is copied into each browser's
`NativeMessagingHosts/` directory.

---

## How it works

### Native messaging framing

Chrome speaks a 4-byte length header followed by a UTF-8 JSON payload:

```
+---------------------------+--------------------------------+
| uint32 length, LITTLE     |  JSON payload, exactly N bytes  |
| ENDIAN, on every platform |                                 |
+---------------------------+--------------------------------+
```

`internal/nativemsg` implements both directions. The three traps it handles:

- **Little endian always**, regardless of host byte order.
- **stdout is sacred.** A single stray `fmt.Println` desynchronises the stream and
  Chrome kills the host. Every log line goes to a file under `%APPDATA%\quick-download\`.
- **Clean EOF on stdin means "Chrome closed the port"** — exit 0, don't treat it
  as an error.

### Why there are two processes

`chrome.runtime.sendNativeMessage()` starts a **fresh host process per message** and
kills it the moment the reply arrives. That lifetime is useless for a downloader.

So the host process is a thin relay:

1. Is the daemon healthy? (`GET /api/health`, 400 ms timeout)
2. If not, spawn `quick-download --daemon` **detached** (`DETACHED_PROCESS |
   CREATE_NEW_PROCESS_GROUP` on Windows, `setsid` elsewhere) and wait for the port.
3. `POST /api/enqueue`, reply to Chrome, exit.

The daemon outlives the host and keeps downloading. Binding port 9090 doubles as the
single-instance lock: a second daemon just exits.

### Chunked downloading

1. `HEAD` the URL. If that fails (plenty of CDNs answer HEAD with 403/405), fall back
   to `GET` with `Range: bytes=0-0` and read the total out of `Content-Range`.
2. If the server advertises `Accept-Ranges: bytes` and the file is ≥ 2 MiB, split it
   into `size / 1 MiB` chunks, capped at 8.
3. One goroutine per chunk, each with its own `Range: bytes=start-end` request writing
   to its own `.partNN` file — no shared mutable state, no locks on the hot path.
4. Failures retry up to 4 times with quadratic backoff, **resuming** from the bytes
   already on disk; the first unrecoverable failure cancels the shared context, which
   aborts every other in-flight read instantly.
5. Parts are concatenated in index order, the total is checked against the expected
   size, then the parts are deleted.

Progress is the sum of per-chunk `atomic.Int64` counters, so a resumed retry can never
double-count. Anything without range support falls back to a single stream.

### Progress feed

`internal/server/ws.go` is a ~200-line RFC 6455 implementation (handshake, frame
write, masked-frame read, ping/pong/close) so the engine needs no dependencies. The
dashboard connects to `ws://127.0.0.1:9090/ws`, receives a full snapshot on connect
and then roughly every 400 ms, and falls back to polling `/api/downloads` if the
socket ever drops.

### HTTP API

| Method | Path              | Body / query        | Purpose                          |
|--------|-------------------|---------------------|----------------------------------|
| GET    | `/api/health`     | –                   | liveness + config + tools ready  |
| GET    | `/api/tools`      | –                   | yt-dlp / ffmpeg paths, re-probed |
| GET    | `/api/downloads`  | –                   | full snapshot (polling fallback) |
| POST   | `/api/enqueue`    | `{url, filename?, referrer?, cookie?, userAgent?, kind?, mime?, title?}` | queue a download |
| POST   | `/api/cancel`     | `{id}`              | cancel                           |
| POST   | `/api/retry`      | `{id}`              | requeue                          |
| POST   | `/api/clear`      | –                   | drop finished jobs               |
| POST   | `/api/reveal`     | `{id}`              | show the file in the OS explorer |
| GET    | `/ws`             | –                   | WebSocket progress feed          |

### Detection in MV3

Blocking `webRequest` is gone in MV3, but **observing** responses is not:
`chrome.webRequest.onHeadersReceived` still fires and gives us `Content-Type` and
`Content-Length`, which URL patterns alone cannot. That's the primary detector.

`declarativeNetRequest` is declared and `rules.json` ships with the extension, but the
ruleset is **`"enabled": false`** on purpose. DNR cannot notify an extension of a match
outside `onRuleMatchedDebug` (unpacked extensions only), and its `allow` rules can
override *other* extensions' blocking rules — enabling media-wide `allow` rules would
punch holes in your ad blocker. Flip `enabled` to `true` in `manifest.json` if you want
the debug matches; detection does not depend on it.

### Sniffing streams

Modern sites never serve a video as one file. The page fetches a **manifest**
(`.m3u8` or `.mpd`), then thousands of 2–10 second segments, and feeds the result to
`<video>` as a `blob:` URL. A blob URL is meaningless outside that page, so it is not
the handle you want — **the manifest is**, and that is what the sniffer keeps.

The rules, in `background.ts`:

| Seen | Action |
|---|---|
| `Content-Type: application/vnd.apple.mpegurl` (and 5 aliases) | capture as HLS |
| `Content-Type: application/dash+xml` | capture as DASH |
| URL ends `.m3u8` / `.m3u` / `.mpd` | capture as HLS/DASH |
| URL ends `.ts` / `.m4s` / `.cmfv` / `.cmfa` / `.vtt` / `.key` | **discard** — a segment |
| Host is youtube/vimeo/x/tiktok/… | the page URL itself is offered |
| Anything else media-ish | direct file |

Two details that matter in practice:

- **Segments are dropped before anything else.** A one-hour HLS video is ~900 of them;
  listing them would be useless and would blow the `chrome.storage.session` quota.
- **A live playlist is re-fetched every few seconds.** Entries are keyed on
  tab + URL, so the same manifest refreshes its entry instead of stacking up.

The manifest URL alone is usually not enough to download — most CDNs 403 a request
without the page's `Referer` and cookies. Every job therefore carries the page URL,
`chrome.cookies.getAll()` for that URL, and the browser's own `User-Agent`, and the
engine passes all three to yt-dlp (`--referer`, `--add-header Cookie:…`,
`--user-agent`). Cookie values are redacted before the command line is logged.

### Thumbnails

The network sniffer sees URLs, not pictures. `content.ts` is injected into the active
tab when the popup opens (`chrome.scripting.executeScript`) and scans the DOM:

- **`<video>`** — draws the current frame into a 320px canvas and calls
  `toDataURL('image/jpeg', 0.7)`, roughly a 2 KB thumbnail. If that throws it falls
  back to the `poster` attribute.
- **`<img>`** — no canvas at all; the image URL *is* the thumbnail. Images whose
  intrinsic size is under 200px on both edges are skipped as sprites and avatars.
- **`<audio>`** — source and duration only.

Cross-origin video without CORS headers **taints the canvas** and `toDataURL` throws
`SecurityError`. That is the browser protecting pixel data, not a bug to route around,
so the code catches it and uses the poster instead.

Streams are the interesting case: a sniffed `.m3u8` and the `<video>` playing it share
no URL, so they cannot be matched directly. The join is done per tab — if the page has
a video, its frame is the preview for the stream sniffed there.

Injection is on demand rather than a declared content script: a frame can only be
captured once the video is actually playing, and a scanner running on every page all
the time is pure overhead. The script guards against double injection, and quietly
does nothing on `chrome://` pages, the Web Store and PDFs, where injection is refused.

Data-URL thumbnails are capped at the newest 40 entries, and a quota error retries once
with all of them stripped, so the popup can never wedge itself on storage.

### The yt-dlp engine

`Classify(url, mime, hint)` picks the engine. The extension's hint wins when present —
it saw the live `Content-Type`, which the URL alone does not reveal.

Progress comes from `--progress-template`, which prints one machine-readable line per
update, rather than scraping the human progress bar (whose format changes between
releases):

```
@QDP@%(progress.status)s|%(progress.downloaded_bytes)s|%(progress.total_bytes)s|…
```

Percent is derived in this order: real byte counts → `total_bytes_estimate` →
**fragment index / fragment count** (live HLS usually knows neither total nor
estimate, only how many segments it has fetched). If the template produces nothing at
all — a very old yt-dlp — the classic `[download] 12.3%` line is scraped as a fallback,
and is ignored the moment a real template line arrives.

Three things this had to get right:

- **yt-dlp downloads video and audio as separate passes**, and its percent restarts at
  0 for the second one. `[info] … Downloading 1 format(s): 137+140` tells us how many
  tracks are coming, and each `Destination:` line advances the counter, so the two
  passes are folded into one bar that only moves forward. Track 2 of 2 at 50% reads as
  75% overall.
- **The output filename is only known at the end.** `Destination:` names each
  intermediate track; `Merging formats into "…"` names the file the user keeps, and it
  is marked authoritative so a late `Destination:` line cannot overwrite it.
- **yt-dlp spawns ffmpeg.** `cmd.Process.Kill()` would kill only yt-dlp and leave the
  muxer holding the output file, so cancel goes through `taskkill /T` on Windows and a
  process-group signal on Unix.

`stderr` is drained on its own goroutine — a full pipe buffer would deadlock the child
— and the last `ERROR:` line becomes the message shown on the card, so a failure reads
`yt-dlp failed (exit 1): ERROR: Unsupported URL` instead of `exit status 1`.

## Configuration

Environment variables, read by both modes:

| Variable    | Default        | Meaning                                  |
|-------------|----------------|------------------------------------------|
| `QD_PORT`   | `9090`         | local API/WebSocket port                 |
| `QD_DIR`    | `~/Downloads`  | where finished files land                |
| `QD_CHUNKS` | `8`            | max parallel connections per file (1–16) |
| `QD_JOBS`   | `3`            | max simultaneous downloads (1–10)        |
| `QD_YTDLP`  | auto-discover  | explicit path to the yt-dlp binary       |
| `QD_FFMPEG` | auto-discover  | explicit path to the ffmpeg binary       |

Run the engine standalone (no Chrome involved):

```powershell
$env:QD_PORT=9000; .\bin\quick-download.exe --daemon
```

## Security notes

- The server listens on `127.0.0.1` only.
- Non-loopback `Host` headers are rejected (cheap DNS-rebinding defence).
- The WebSocket endpoint checks `Origin`: only the dashboard itself and
  `chrome-extension://` / `moz-extension://` pages are accepted. Without this, any
  website you visit could open a socket to your loopback port.
- Filenames are sanitised: path separators, control characters, Windows-reserved
  names (`CON`, `LPT1`, …) and query strings are stripped, so a hostile
  `Content-Disposition` cannot escape the download directory.
- Existing files are never overwritten — `clip.mp4` becomes `clip (1).mp4`.
- yt-dlp is executed directly (no shell), and the URL is passed after a `--`
  separator so a URL starting with `-` cannot be parsed as a flag.
- Cookie values are redacted before the yt-dlp command line reaches the log file.
- yt-dlp/ffmpeg are resolved from the engine's own folder first and `PATH` last,
  so a stray `yt-dlp.exe` in the working directory cannot shadow the real one.

## Tests

```bash
cd backend && go test ./...
```

Covers, for the HTTP engine: little-endian framing round-trip and truncation handling,
the RFC 6455 handshake against the specification's own test vector, frame length
encodings, masked-frame decoding, origin filtering, chunk planning (no gaps or overlaps
at any size), filename sanitisation, and full end-to-end downloads against a
range-capable test server — including a mid-transfer connection drop, a server that
rejects `HEAD`, a server with no range support, and cancellation.

For the streaming engine: URL classification across manifests, MIME types, extractor
sites and hint overrides; segment rejection; `--progress-template` parsing in all three
modes (bytes, estimate, fragments); monotonic multi-track progress; filename discovery
via `Destination:` / `Merging formats into`; the legacy-percent fallback and its
suppression; ``-split line scanning; cookie redaction; the actionable error when the
tools are missing; and two end-to-end runs against a **compiled fake yt-dlp** that
prints a real yt-dlp transcript — one success, one failure whose stderr must surface.

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| Popup says **engine offline** | Host not registered, or Chrome not restarted. Check the registry value points at an existing `com.downloader.app.json`. |
| `Specified native messaging host not found` | The `name` in the manifest, the registry key name and `NATIVE_HOST` in `src/types.ts` must all be `com.downloader.app`. |
| `Access to the specified native messaging host is forbidden` | `allowed_origins` doesn't list your extension id. Re-run the installer with the current id — it changes if you move the extension folder. |
| Nothing is detected on a page | Media may be HLS/DASH (skipped by design), or images below the size floor. Lower it in the popup's Settings. |
| Downloads are single-connection | The server didn't advertise `Accept-Ranges: bytes`, or the file is under 2 MiB. Both are expected. |
| Port 9090 is taken | `setx QD_PORT 9000`, restart Chrome. |
| Nothing in the log | `%APPDATA%\quick-download\quick-download-host.log` and `-daemon.log`. |

## Known limits

- No pause/resume across restarts — jobs live in memory. The chunk-resume machinery is
  already there, so persisting the job table is a small step.
- Streaming downloads are single-threaded per file: yt-dlp manages its own concurrency,
  so the 4–8 connection chunking does not apply to them.
- DRM-protected streams (Widevine/PlayReady) are not downloadable, by design.
- No quality picker yet — streams take `bv*+ba/b`, i.e. best video + best audio. Change
  the `-f` argument in `ytdlp.go` for something else.
- No global bandwidth cap or scheduler.

