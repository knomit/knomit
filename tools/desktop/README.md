# knomit-desktop

A native cross-platform desktop app (macOS / Windows / Linux) built with
[Wails v3](https://v3.wails.io). It replaces the old `tools/tray` system-tray
launcher (clunky, CGO-heavy, no real systray on Linux).

## What it does

- **System tray** — icon/label with a menu: *Show Knomit*, *Start at login*,
  *Quit*.
- **Native webview window** — shows the knomit React UI, served by Wails from
  the embedded `web/dist` assets (no second frontend build).
- **A supervised server, not an embedded one** — the app contains no knomit
  server. It runs the bundled `knomit` binary (never one on `PATH`) as
  `knomit serve --supervised` and hosts the UI over that process's local socket
  (named pipe on Windows): `/api/` through a proxy in the asset handler, the
  event streams as Wails Streams. If a knomit already answers on this home's
  socket (a `knomit serve` you started), the app **attaches** to it instead and
  never stops or restarts it. See `supervisor.go`.
- **Discovery** — the server writes its own `server.json` (the default home's
  in the state directory, any other home's in that home), which kb and MCP
  clients read; kb dials the socket first.
- **Native actions** — exposed to the UI as typed Wails bindings
  (`NativeService`), reachable only from the embedded window (never over the
  API).
- **Bundled MCP bridge** — ships `kb` (the stdio↔HTTP MCP adapter)
  and, on launch, symlinks it to `<home>/bin/kb` so stdio MCP clients
  have a stable command path (see [MCP integration](#mcp-integration)).

## How it fits together

```
knomit-desktop (Wails v3, no server code)          knomit serve --supervised (bundled binary)
├─ desktop.lock (one desktop per data root)          local socket / pipe (mandatory)
├─ supervisor: probe -> attach | spawn ------------> stdin pipe (EOF = stop); Linux Pdeathsig,
│    restart with backoff; exit 2 = give up;           Windows Job Object as backstops
│    exit 3 = attach                                  TCP / TLS if configured; server.json
├─ Wails AssetServer: web/dist, /config.js, /boot/status,
│    /api/* proxy + Wails Streams ---- auth.DialLocal ---> the socket
└─ system tray + NativeService bindings (native OS actions)
```

The React app is the **same bundle** as the cloud build; `/config.js` reports
`__KNOMIT_BOOTING__` until the server answers on its socket, then the page's
own origin as `__KNOMIT_API_BASE__`, so every API call is same-origin.
Quitting the app stops a server it spawned (stdin closed, killed after 10 s)
and leaves an attached one running.

## Build & run

```sh
make desktop-deps # Linux only: install the GTK4 + WebKitGTK build deps (once, sudo)
make setup        # fetch native libs into dist/<platform>/lib (once)
make dist         # everything end to end: server, then desktop
make server       # server only: web UI + knomit, kb, knomit-okf in dist/<platform> (alias: make build)
make desktop      # server (as a prerequisite) + desktop; macOS: dist/<platform>/Knomit.app  ·  Linux: dist/<platform>/knomit-desktop  ·  Windows: knomit-desktop.exe
make desktop-run  # macOS: open the .app               ·  else: run the binary
make desktop-install # Linux only: install the .desktop launcher + app icon into ~/.local
```

On **Linux** the desktop app links GTK4 + WebKitGTK 6.0, so run `make
desktop-deps` once first (it `apt-get install`s the dev packages; the `-dev`
packages also pull the runtime libs). **macOS** needs nothing extra (system
Cocoa/WebKit).

**Windows** uses the system WebView2, but the build and the embedder need three
things that are not on a stock machine:

- an **MSYS2 mingw-w64 gcc** for CGo, and MSYS2's `make` — run it from an
  **MSYS2 MINGW64 shell with `MSYS2_PATH_TYPE=inherit`**, which is the only
  combination where the whole Makefile works. Two near-misses, both of which
  fail confusingly rather than clearly:
  - from **Git Bash**, `C:\msys64\usr\bin\make.exe` does not pass its
    environment to recipes (Git Bash and MSYS2 are different msys runtimes), so
    `%LocalAppData%` is empty, `go env GOCACHE` reports `off`, and every build
    dies with "build cache is required, but could not be located";
  - from **PowerShell/cmd**, the environment survives but recipes run with the
    Windows `PATH`, so npm's `#!/usr/bin/env bash` shebang fails with
    `/usr/bin/env: 'bash': No such file or directory`.

  A plain MSYS2 shell without `MSYS2_PATH_TYPE=inherit` has neither `go` nor
  `npm` on its PATH.
- **Rust/cargo** ([rustup.rs](https://rustup.rs)) plus the GNU target:

  ```sh
  rustup target add x86_64-pc-windows-gnu
  ```

  daulet/tokenizers publishes no Windows artifact, so `make setup` builds
  `libtokenizers.a` from source (~5 min, once). The target is not optional:
  rustup's default Windows host is **MSVC**, which produces a `.lib` that the
  MSYS2 mingw linker cannot use — cargo would succeed and the knomit link would
  then fail with a wall of undefined references. Set
  `KNOMIT_TOKENIZERS_SRC=<checkout>` to reuse an existing daulet/tokenizers
  working tree, and the whole step is skipped when
  `dist/windows-amd64/lib/libtokenizers.a` already exists;
- the **Visual C++ 2015-2022 x64 redistributable, version 14.40 or newer**
  ([installer](https://aka.ms/vs/17/release/vc_redist.x64.exe)). ONNX Runtime
  has required it since 1.21; below it `onnxruntime.dll` fails to load and the
  server dies at embedder init. knomit checks the installed version and says so
  by name when it is too old.

On **Windows** the binary is linked into the **GUI subsystem** (`-H windowsgui`,
added to the desktop binary's ldflags only — see `DESKTOP_LDFLAGS` in the
Makefile). Go's default is the CONSOLE subsystem, and a console-subsystem binary
is not a desktop app: Windows allocates a console for it, and the terminal that
launched it then **owns its lifetime**, so closing that window kills the tray
app. The CLIs (`knomit`, `kb`, `knomit-okf`) stay console-subsystem on purpose.

The trade is that a GUI-subsystem process gets no console, so `fmt.Println` has
nowhere to go. That only matters for one thing, and it is handled:

```powershell
.\knomit-desktop.exe --version   # prints to the console you ran it from
```

`tools/desktop/console_windows.go` re-attaches to the launching console
(`AttachConsole(ATTACH_PARENT_PROCESS)` + `CONOUT$`) on the `version` path only,
and *only* for a stream that has no usable handle — a stream the shell already
redirected is left alone, since clobbering it would send the output to a console
nobody is watching. Run from Explorer or the tray there is no parent console and
the call is a no-op. The `knomit-desktop: logging to ...` stderr line at startup
is simply discarded when there is no console; the same text goes to the log file.

**Capturing the version in PowerShell: use a pipe, not `>`.** PowerShell does not
wait on a GUI-subsystem child — it hands it a pipe instead of the file handle and
closes the read end without draining it, so the bytes are dropped. Measured, 3
runs each:

| command | PowerShell 5.1 | PowerShell 7 | cmd.exe |
| --- | --- | --- | --- |
| `knomit-desktop.exe --version > v.txt` | **0 bytes** | **0 bytes** | 15 bytes |
| `knomit-desktop.exe --version \| Set-Content v.txt` | 16 bytes | 16 bytes | — |

```powershell
.\knomit-desktop.exe --version | Set-Content v.txt   # works
cmd /c ".\knomit-desktop.exe --version > v.txt"      # also works
```

This is not something the app can fix: `CONOUT$` would not put the bytes in the
file either, and the same shells redirect a *console*-subsystem binary
(`knomit-okf.exe`) to a file correctly. It is PowerShell's handling of GUI
children, and it applies to any `-H windowsgui` program.

Artifacts are written under `dist/<goos>-<goarch>/` (Wails can't cross-compile,
so each platform is built natively). The desktop app/binary lives **only** under
that platform dir — there is no top-level symlink for it. Launch the macOS bundle
with `make desktop-run`, or `open dist/darwin-arm64/Knomit.app`.

On **macOS** `make desktop` packages a real **`dist/<platform>/Knomit.app`**
bundle — double-clickable in Finder or `open dist/darwin-arm64/Knomit.app`, no
terminal. The bundle
contains the binary under `Contents/MacOS/`, the `Info.plist`, and the ONNX
dylibs under `Contents/MacOS/lib/` (where the binary resolves them).
`libtokenizers.a` is linked statically, so it needs no runtime lib. The bundle
is unsigned (fine for local runs; sign + notarize for distribution).

`make desktop` builds with `-tags desktop` and CGO. The cloud `knomit` binary
never imports Wails (build-tag isolated), so `go build .` / the Docker image stay
Wails-free.

## Icons

All icon assets are generated from the logos by `make desktop-icons` and
committed (the binary `//go:embed`s them; regen only when a logo changes):

- **`appicon.png`** (256px, colored) — the application/window icon, passed as
  `application.Options{Icon}`. On **Linux** this is the *only* source for the
  window/taskbar/alt-tab icon (no `.app` bundle to fall back on), so without it
  the window shows GTK's generic placeholder. `make desktop-install` also
  installs it as the hicolor icon for the `.desktop` launcher.
- **`icon.png`** (64px, colored) — the tray icon on **Linux/Windows**, whose
  tray areas have no monochrome-template convention.
- **`icon-tray-light.png` / `icon-tray-dark.png`** (64px) — the **macOS**
  menu-bar icons: the same diamond+graph art as the app icon, with the green
  recolored. `*-light` is the dark glyph for a light menu bar; `*-dark` is the
  light glyph (white diamond) for a dark menu bar. `trayicon_darwin.go` swaps
  between them on the `ThemeChanged` event so the tray follows the system theme.
  (We don't use `SetTemplateIcon` — a template is single-tone and would collapse
  the two-tone mark — nor `SetDarkModeIcon`, which is a no-op on macOS in Wails
  v3.) Linux/Windows keep the colored `icon.png` via `trayicon_others.go`.
- **`macos/icon.icns`** — the `.app` bundle icon (Dock + Finder).
- **`rsrc_windows_amd64.syso` / `rsrc_windows_arm64.syso`** — the **Windows**
  application icon (Explorer, taskbar, Alt-Tab). Not embedded by Go code: the
  linker picks up any `*_windows_<arch>.syso` next to `package main`
  automatically. Generated from `appicon.png` by `make desktop-winres` (needs
  `go install github.com/tc-hib/go-winres@v0.3.3`, pinned) and committed like
  the rest. Icon only — **no manifest**, because the binary already has one from
  an unexpected source: the desktop build is CGO, so it links externally through
  MinGW, and mingw-w64 adds its `default-manifest.o` (asInvoker + supportedOS) to
  everything it links. (A pure-Go `-H windowsgui` binary has no manifest at all;
  this is the toolchain's doing, not Go's — it would vanish if the desktop app
  ever stopped needing cgo.) That manifest sets no `dpiAware`, which is exactly
  what lets Wails v3 set per-monitor-v2 DPI awareness at runtime — Windows
  permits that only once per process. No version resource either: it would bake
  in a version that `-ldflags` injects at build time.

## MCP integration

Stdio-only MCP clients (Claude Code/Desktop, VS Code) launch `kb` as
a subprocess; it discovers the running server via `server.json` and proxies MCP
over the looknomitck port. The app ships that binary so the integration works
without a separate `make server`:

- `make desktop` builds `kb` into the bundle —
  `Knomit.app/Contents/MacOS/kb` on macOS, next to the binary in
  `dist/<platform>/` elsewhere. It is pure Go (no CGO/dylibs).
- On launch the app symlinks it to **`<home>/bin/kb`** (`home` =
  `config.Home`, overridable via `KNOMIT_HOME`) — a stable path that survives
  the app being moved or updated. The symlink is refreshed idempotently each
  launch; failure is logged but never blocks startup ([bridge.go](bridge.go)).

### Where `<home>` is

`config.Home` is the knomit data root — `control.db`, `repos/`, `models/`, the
SSH keypair, `bin/` and `knomit.toml`. The default is per-OS and is the **same
for all three binaries**: `knomit serve`, `kb` and the desktop app resolve it
through `internal/foundation/config` (`config.DefaultHome`, `config.StateDir`), so it
cannot matter which one starts first. The OS half of that — which directory an
OS designates for per-user state, and the refusal to guess one when nothing
resolves — is `internal/platform/userdirs`, which knows no application name.

| OS | default `config.Home` |
| --- | --- |
| macOS, Linux | `~/.knomit` |
| Windows | `%LOCALAPPDATA%\knomit\home` |

On Windows the data root sits one level below `%LOCALAPPDATA%\knomit`, which is
the desktop app's *state* directory (`server.json`, logs, `update.json` — the
folder "Reveal log" opens). Nothing on Windows reads `%USERPROFILE%\.knomit`.

`KNOMIT_HOME` overrides the default everywhere. If it is unset and the OS
cannot say where your profile is, startup **fails with an error naming
`KNOMIT_HOME`** rather than choosing a fallback: a writable-but-wrong root
looks like a fresh install, so the models download again and a second identity
is generated somewhere nobody would think to look.

**Windows data from a pre-release build is NOT migrated.** Windows has never
shipped, so there is no automatic move and nothing reads
`%USERPROFILE%\.knomit` any more. If you have one from an earlier local build
and want to keep using it, set `KNOMIT_HOME` to it; otherwise delete it — it
holds an SSH identity, so it is not inert. A `C:\.knomit` (or any `.knomit` at
a drive root) is debris from a launch whose environment had no `%USERPROFILE%`
and can be deleted outright; that path is the bug this layout fixed, and
nothing creates it now.

Point an MCP client at that stable path, e.g. on macOS/Linux:

```json
{
  "mcpServers": {
    "knomit": {
      "command": "~/.knomit/bin/kb",
      "args": ["--repo", "<repo>", "--source", "<slug>", "--profile", "code"]
    }
  }
}
```

On Windows the same path is `%LOCALAPPDATA%\knomit\home\bin\kb.exe`.

Or scaffold a project's integration files with
`<home>/bin/kb claude init --source <slug>`. (`kb claude init` itself writes
`"command": "kb"` and relies on `PATH`, so the absolute path is only needed for
clients you configure by hand.)

Running `knomit serve` on the same home as the app is fine: the app finds it
answering on the socket and attaches to it (one server, one `server.json`).

## HTTP port (Settings)

The app and `kb` use the server's local socket, so the plaintext HTTP port is
optional. **Settings → HTTP port** chooses **Off**, **This computer only**
(loopback: over the default or any loopback host no `host` key is written; only
a move back from All networks writes `host = "127.0.0.1"`) or **All networks** (`host = "0.0.0.0"`), plus
the port. Save writes only the keys that change (`http`, `host`, `port`) to
`knomit.toml` and asks the running server to reload (the same thing as
`knomit reload`, never an HTTP route); the window shows what the server then
listens on, any key that needs a restart (`[auth].require`, for one), or why a
port could not be opened. **All networks** lets anyone who can reach the
computer READ every knowledge base and clone it, without signing in; they
cannot change anything. To work with other machines securely, enrol in a fleet
and use the Fleet port.

A fresh install (no `knomit.toml` and no `control.db` in the data root) starts
with `http = false`; an existing install keeps its port. A server you started
yourself with `knomit serve` shows a banner (its pid and both versions, with a
warning when major.minor differ): Save still reloads it, but **Restart
server** is unavailable. `KNOMIT_HTTP`/`KNOMIT_HOST` in the environment beat
`knomit.toml` and make the choice read-only.

## Fleet peer

The app can join a knomit fleet as an enrolled instance (knomit#256), without
the `knomit` CLI. In **Settings → Fleet identity**:

1. **Copy public key** and send the line to whoever holds the fleet root. They
   call the master's `POST /api/v1/fleet/enroll` with `agent_id` and `public_key` (the
   agent id is the `agent:` of this instance's member record) and send back a
   bundle.
2. **Paste from clipboard** or **Choose file…**, then **Install**. The same
   checks as `POST /api/v1/fleet/join` apply: the key matches, the chain
   verifies, the CRL is not older than the one held. A bundle from a
   *different* fleet root is refused until you confirm the move, and the
   confirmation names both roots' fingerprints.
3. Set **Fleet listener** (`[tls].addr`, e.g. `0.0.0.0:19279`) and Save: the
   running server reloads and opens it. (A first enrolment whose listener was
   off for lack of a certificate still offers **Restart server**.) The mTLS
   listener opens only when an address is set **and** a certificate is
   installed. The plaintext HTTP port is a separate choice, below.

Trust material is installed only from the Settings window, never over HTTP.
CRL updates are manual: a new bundle, or a fresh `crl.pem` dropped into
`[tls].dir`, which a running listener adopts within its recheck interval. A
desktop that never receives the new CRL keeps serving a revoked peer. If
`knomit serve` on the same home already holds the address, the app logs a
warning and serves without the listener.

## Logs

Two rotating files in the same directory: the server's `knomit.log` (or the
`[log] file` knomit.toml names; the app passes it as `--log-file`), which
Settings names and Reveal opens, and the app shell's own `knomit-desktop.log`
beside it, which also receives the server's stderr tail when it exits
unexpectedly:

- **macOS:** `~/Library/Logs/knomit/`
- **Linux:** `$XDG_STATE_HOME/knomit/` (or `~/.local/state/knomit/`)

```sh
tail -f ~/Library/Logs/knomit/knomit.log ~/Library/Logs/knomit/knomit-desktop.log
```

When launched as a `.app`, stderr is discarded by macOS, so the file is the only
place to see output. The file gets raw JSON (grep/parse-friendly); the terminal
gets human-readable output.

## Reused internals

`internal/{paths,singleinstance,autostart}` were salvaged from the old
`tools/tray` and moved here (the lockfile code now lives in
`internal/foundation/serveraddr`, the single-instance lock is an OS lock on
`desktop.lock`). The server runs as a supervised subprocess again, this time
the same `knomit serve` binary the CLI ships.

## Caveats

- **Wails v3 is alpha** (pinned to `v3.0.0-alpha.98-tui`); expect some churn.
- Wails calls `os.Exit` on quit, so cleanup (lockfile removal, app close) runs via
  the `OnShutdown` hook, not Go `defer`s.
