# knomit-desktop

A native cross-platform desktop app (macOS / Windows / Linux) built with
[Wails v3](https://v3.wails.io). It replaces the old `tools/tray` system-tray
launcher (clunky, CGO-heavy, no real systray on Linux).

## What it does

- **System tray** — icon/label with a menu: *Show Knomit*, *Start at login*,
  *Quit*.
- **Native webview window** — shows the knomit React UI, served **in-process**
  by Wails from the embedded `web/dist` assets (no second frontend build).
- **In-process server** — boots the knomit server in the same process in
  **API-only** mode (no UI routes) on a looknomitck TCP port, preferring `19278`
  and falling back to an ephemeral port if it is taken.
- **Discovery** — writes the port to `<StateDir>/server.json` (the same lockfile
  knomit-remote / MCP clients already read), so Claude Code et al. can reach the
  MCP endpoint over HTTP. Opening the port in a browser returns the API
  (problem+json), **not** a duplicate UI.
- **Native actions** — exposed to the UI as typed Wails bindings
  (`NativeService`), reachable only from the embedded window (never over the
  looknomitck port).
- **Bundled MCP bridge** — ships `kb` (the stdio↔HTTP MCP adapter)
  and, on launch, symlinks it to `<home>/bin/kb` so stdio MCP clients
  have a stable command path (see [MCP integration](#mcp-integration)).

## How it fits together

```
knomit-desktop (CGO, Wails v3)
├─ Wails AssetServer ── serves web/dist + a dynamic /config.js  → webview window
│                         (sets window.__KNOMIT_API_BASE__ = http://127.0.0.1:PORT)
├─ in-process http.Server :PORT ── API + MCP + git only (APIOnly=true)
│                         ← Claude Code / MCP clients, and the webview's fetch/SSE
└─ system tray + NativeService bindings (native OS actions)
```

The React app is the **same bundle** as the cloud build; it reads
`window.__KNOMIT_API_BASE__` at runtime (empty/same-origin in the cloud,
the looknomitck URL here) so its API/SSE calls reach the in-process server
cross-origin. CORS allows the Wails origin (`wails://localhost` on
macOS/Linux, `http://wails.localhost` on Windows).

## Build & run

```sh
make desktop-deps # Linux only: install the GTK4 + WebKitGTK build deps (once, sudo)
make setup        # fetch native libs into dist/<platform>/lib (once)
make desktop      # macOS: dist/<platform>/Knomit.app  ·  Linux: dist/<platform>/knomit-desktop  ·  Windows: knomit-desktop.exe
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
without a separate `make build`:

- `make desktop` builds `kb` into the bundle —
  `Knomit.app/Contents/MacOS/kb` on macOS, next to the binary in
  `dist/<platform>/` elsewhere. It is pure Go (no CGO/dylibs).
- On launch the app symlinks it to **`<home>/bin/kb`** (`home` =
  `config.Home`, default `~/.knomit`, overridable via `KNOMIT_HOME`) — a stable
  path that survives the app being moved or updated. The symlink is refreshed
  idempotently each launch; failure is logged but never blocks startup
  ([bridge.go](bridge.go)).

Point an MCP client at that stable path, e.g.:

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

Or scaffold a project's integration files with
`~/.knomit/bin/kb claude init --source <slug>`.

Run **either** the app **or** `knomit serve`, not both — the app falls back to
an ephemeral port when `:19278` is taken, leaving two servers and an ambiguous
`server.json`. The bridge resolves its target once at startup, so if the app
quits and relaunches on a new port, restart the MCP client to pick up the new
`server.json`.

## Logs

The app logs to a rotating file (also to stderr when run from a terminal):

- **macOS:** `~/Library/Logs/knomit/knomit-desktop.log`
- **Linux:** `$XDG_STATE_HOME/knomit/knomit-desktop.log` (or `~/.local/state/knomit/…`)

```sh
tail -f ~/Library/Logs/knomit/knomit-desktop.log
```

When launched as a `.app`, stderr is discarded by macOS, so the file is the only
place to see output. The file gets raw JSON (grep/parse-friendly); the terminal
gets human-readable output.

## Reused internals

`internal/{lockfile,netutil,paths,singleinstance,autostart}` were salvaged from
the old `tools/tray` and moved here. The supervisor + `webview_go` / `fyne.io/systray`
glue is gone — the server runs in-process, not as a supervised subprocess.

## Caveats

- **Wails v3 is alpha** (pinned to `v3.0.0-alpha.98-tui`); expect some churn.
- Wails calls `os.Exit` on quit, so cleanup (lockfile removal, app close) runs via
  the `OnShutdown` hook, not Go `defer`s.
