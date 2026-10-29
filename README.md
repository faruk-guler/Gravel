# Gravel

<p align="center">
  <img src="icon/gravel-logo.png" alt="Gravel logo" width="300" height="300" />
</p>

A PuTTY + WinSCP hybrid desktop client for Linux servers, built with Go and [Wails](https://wails.io).

**Version:** 4.8.0

## Features

- **SSH terminal** — PuTTY-style in-session login (`login as:` / password), multiple tabbed sessions, and the PuTTY mouse model (select to copy, right-click to paste). Servers that demand keyboard-interactive / OTP (2FA) logins are supported: the extra challenges are prompted in the terminal just like the password. An SSH-level keepalive probe plus OS TCP keepalive detects dead or half-open links instead of leaving a frozen terminal on screen, and a dropped link can reconnect itself (Settings, on by default). Servers that only speak pre-OpenSSH-7.4 algorithms (CBC ciphers, SHA-1 MACs, group14/group1 kex) are reached through a quiet second handshake with the legacy algorithm set. New connections can be routed through an outbound SOCKS5 or HTTP proxy (Settings; the proxy password is kept in memory only, never on disk). Key logins read OpenSSH private keys *and* PuTTY's own `.ppk` files - both on-disk versions, including Argon2-protected encrypted ones - so a `.ppk` somebody already has from PuTTY works without converting it in PuTTYgen first, and a mistyped passphrase is re-prompted in place just like a password.
- **SFTP browser** — docked beside the terminal; upload a file or a whole folder (mirrored under the remote directory, tree created as needed), download files and folder trees, rename, delete, and optionally follow `cd` commands. Files open in an in-app editor (monospace, modified indicator, Ctrl+S to save) with a size cap and binary detection so huge or non-text files are refused or opened read-only instead of garbled. A permission editor sets mode (chmod, via rwx checkboxes or octal) and owner/group (chown, resolved against the server's account files)
- **Transfer queue** — live progress, speed and ETA per file pushed to the UI as it moves (throttled, no polling gaps while bytes are flowing), with cancel and pause/resume: a paused transfer stops at the next chunk and continues from the bytes already moved instead of restarting. A brief link failure (reset, timeout, EOF) is retried from the exact byte where the copy stopped, up to three attempts with backoff - the partial file is kept between tries and only a final failure removes it. When a destination file already exists, a configurable conflict policy overwrites, renames (`file (1).ext`) or skips it
- **Tunnels** — local (`-L`), remote/reverse (`-R`, the SSH server opens a port and hands each connection back to a local service) and dynamic SOCKS5 (`-D`) port forwards with inline edit, start/stop per session
- **Host key trust** — verified on first use and recorded per key type (like OpenSSH's one line per algorithm), so a server switching between its RSA and ED25519 keys is not a false alarm. A genuinely changed key raises an explicit warning and is only replaced after the user confirms the exact new fingerprint
- **Saved sessions** — server profiles with drag-and-drop ordering and bulk import from pasted text or a hosts file; credentials are never stored
- **Settings** — terminal font size, transfer conflict policy, outbound connection proxy and a dropped-link reconnect toggle in one place (theme and file-browser toggles live where they apply); the whole configuration (servers, trusted host keys, proxy and preferences) can be exported to a file and imported back, with secrets never included
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

A `sessions.json` or `known_hosts.json` that cannot be parsed is renamed to
`<name>.bad-<timestamp>` and reported in the Logs tab instead of being treated as
empty, so the next save can never overwrite data it could not read. Only one
Gravel window runs at a time: a second launch raises the open window rather than
starting another writer over the same files.

Third-party components and their licenses are listed in
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
