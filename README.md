# Gravel

<p align="center">
  <img src="icon/gravel-logo.png" alt="Gravel logo" width="300" height="300" />
</p>

A PuTTY + WinSCP hybrid desktop client for Linux servers, built with Go and [Wails](https://wails.io).

## Features

- **SSH terminal** — PuTTY-style in-session login (`login as:` / password), multiple tabbed sessions, and the PuTTY mouse model (select to copy, right-click to paste)
- **SFTP browser** — docked beside the terminal; upload, download, rename, delete, and optionally follow `cd` commands
- **Transfer queue** — live progress, speed and ETA per file, with cancel and pause/resume: a paused transfer stops at the next chunk and continues from the bytes already moved instead of restarting
- **Tunnels** — local (`-L`) and dynamic SOCKS5 (`-D`) port forwards with inline edit, start/stop per session
- **Saved sessions** — server profiles with drag-and-drop ordering and bulk import from pasted text, a hosts file, or your existing PuTTY session registry; credentials are never stored
- **Settings** — terminal font size in one place (theme and file-browser toggles live where they apply)
- **Logs** — every action in one place, live in the Logs tab, plus one log file per app launch

## Requirements

- Go 1.26+
- Node.js
- [Wails CLI v2](https://wails.io/getting-started/install/)
- Windows (WebView2)

## Build

```sh
wails build          # release build -> build/bin/Gravel.exe
wails dev            # live-reload development mode
```

`wails build` installs the frontend packages and generates `frontend/dist` on its
own, so a freshly cloned copy needs nothing but the prerequisites above. Run it
before plain `go build ./...` or `go test ./...`: `main.go` embeds
`frontend/dist`, so those fail with "pattern all:frontend/dist: no matching
files found" until the first build exists. `go test ./internal/...` works
without it.

## Configuration

Settings live in `%APPDATA%\Gravel`. A folder left over from the earlier names
(Genpilot, then Bedrock) is moved there on first run, so saved servers and
trusted host keys come along — as does the browser profile holding the stored
preferences, which older builds kept in `%APPDATA%\Gravel.exe`:

| File / folder | Purpose |
|------|---------|
| `sessions.json` | saved server profiles |
| `known_hosts.json` | trusted SSH host keys |
| `gravel-<timestamp>.log` | one log file per app launch |
| `EBWebView/` | WebView2 profile: theme, font size, pane width |
