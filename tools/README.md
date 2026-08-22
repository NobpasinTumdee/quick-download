# tools/

Drop `yt-dlp` and `ffmpeg` here (or next to `bin/quick-download.exe`) to enable
streaming downloads — HLS, DASH and site pages.

The engine looks for them, in this order:

1. the paths in `QD_YTDLP` / `QD_FFMPEG`, if set
2. **the folder holding `quick-download.exe`** — i.e. `bin/`
3. `bin/tools/`
4. the working directory, `./bin`, `./tools`
5. anything on `PATH`

Resolution happens per request, so you can drop the binaries in while the engine
is running — no restart needed. Check what it found at
<http://127.0.0.1:9090/api/tools>.

## Where to get them

| Tool | Windows | Notes |
|---|---|---|
| yt-dlp | <https://github.com/yt-dlp/yt-dlp/releases/latest> → `yt-dlp.exe` | single file, no install |
| ffmpeg | <https://www.gyan.dev/ffmpeg/builds/> → "release essentials" | take `ffmpeg.exe` out of `bin/` in the zip |

On Linux/macOS: `pipx install yt-dlp` (or your package manager) plus
`apt install ffmpeg` / `brew install ffmpeg`, then they are on `PATH` and this
folder stays empty.

## Recommended layout

```
quick-download/
└── bin/
    ├── quick-download.exe
    ├── com.downloader.app.json
    ├── yt-dlp.exe            <-- here
    └── ffmpeg.exe            <-- and here
```

Both binaries stay out of git (see `.gitignore`); they are third-party
executables under their own licences (yt-dlp: Unlicense, ffmpeg: LGPL/GPL
depending on the build).
