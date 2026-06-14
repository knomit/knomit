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
make setup        # fetch native libs into dist/lib (once)
make desktop      # macOS: builds dist/Knomit.app  ·  Linux/Windows: dist/knomit-desktop
make desktop-run  # macOS: open dist/Knomit.app    ·  else: run the binary
```

On **macOS** `make desktop` packages a real **`dist/Knomit.app`** bundle —
double-clickable in Finder or `open dist/Knomit.app`, no terminal. The bundle
contains the binary under `Contents/MacOS/`, the `Info.plist`, and the ONNX +
graphqlite dylibs under `Contents/MacOS/lib/` (where the binary resolves them).
`libtokenizers.a` is linked statically, so it needs no runtime lib. The bundle
is unsigned (fine for local runs; sign + notarize for distribution).

`make desktop` builds with `-tags desktop` and CGO. The cloud `knomit` binary
never imports Wails (build-tag isolated), so `go build .` / the Docker image stay
Wails-free.

## Reused internals

`internal/{lockfile,netutil,paths,singleinstance,autostart}` were salvaged from
the old `tools/tray` and moved here. The supervisor + `webview_go` / `fyne.io/systray`
glue is gone — the server runs in-process, not as a supervised subprocess.

## Caveats

- **Wails v3 is alpha** (pinned to `v3.0.0-alpha.98-tui`); expect some churn.
- Wails calls `os.Exit` on quit, so cleanup (lockfile removal, app close) runs via
  the `OnShutdown` hook, not Go `defer`s.
