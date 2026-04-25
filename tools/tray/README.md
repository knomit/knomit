# knomit-tray

Background supervisor for `knomit serve`, plus a native-window UI command.
Runs as a menu-bar tray on macOS and as a headless daemon on Linux (the
desktop-environment app launcher invokes the window command).

## macOS

### Prereqs

- Xcode command-line tools (provides WebKit headers).

### Build and run

```bash
make build tray
./dist/knomit-tray              # starts tray + server
./dist/knomit-tray window --url http://127.0.0.1:19278   # optional; the tray's
                                                         # menu also opens it
```

The tray looks for the `knomit` binary in `$KNOMIT_BIN`, then the same
directory as `knomit-tray`, then `$PATH`.

## Linux

### Prereqs

One-time per distro:

| Distro | Install |
|--------|---------|
| Debian 12 / Ubuntu < 24.04 | `sudo apt install libwebkit2gtk-4.0-dev` |
| Debian 13+ / Ubuntu 24.04+ | `sudo apt install libwebkit2gtk-4.1-dev` |
| Fedora | `sudo dnf install webkit2gtk3-devel` |
| Arch / Manjaro | `sudo pacman -S webkit2gtk` |

> **Note:** The upstream `webview_go` library hardcodes `webkit2gtk-4.0` in its CGO directives. The Makefile automatically creates a pkg-config shim when only 4.1 is installed, so either version works.

### Build

```bash
make build tray
```

This produces four files under `dist/`:

- `knomit-tray` — the binary (runs the background supervisor)
- `knomit.desktop` — app launcher entry (points at `knomit-tray window`)
- `knomit-tray.service` — systemd user unit (optional)
- `knomit.png` — 128×128 launcher icon

### Install the desktop launcher

```bash
cp dist/knomit.desktop ~/.local/share/applications/
```

Then press Super, type "Knomit", and hit Enter. A native window opens against
the running supervisor. (GNOME users: no extension needed — we use the
launcher, not a tray icon.)

### Running the supervisor

Option A — from a terminal (foreground):

```bash
./dist/knomit-tray
```

Ctrl-C to stop.

Option B — systemd user unit (auto-start on login, auto-restart on crash):

```bash
cp dist/knomit-tray.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now knomit-tray
```

Stop: `systemctl --user stop knomit-tray`.
Logs: `journalctl --user -u knomit-tray` or the log file at
`$XDG_STATE_HOME/knomit/serve.log` (default `~/.local/state/knomit/serve.log`).

### Lockfile location

- macOS: `~/Library/Application Support/knomit/server.json`
- Linux: `$XDG_STATE_HOME/knomit/server.json` (default `~/.local/state/knomit/server.json`)

`knomit-remote` and `knomit-tray window` read this file to auto-discover
the running server's port.
