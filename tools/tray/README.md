# knomit-tray

macOS system-tray app that keeps `knomit serve` running and opens a native
webview window onto the web UI on demand.

## Build prerequisites (macOS)

- Xcode command-line tools (provides WebKit headers).
- CGO enabled (set by default on darwin).

## Build

    go build -o dist/knomit-tray ./tools/tray

## Run (dev)

    ./dist/knomit-tray              # start tray (default command)
    ./dist/knomit-tray window --url http://127.0.0.1:19278

The tray looks for the `knomit` binary in (1) `$KNOMIT_BIN`, (2) the same
directory as `knomit-tray`, (3) `$PATH`.
