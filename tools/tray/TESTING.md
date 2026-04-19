# knomit-tray — manual test plan

Phase 1 (macOS) end-to-end verification. Run through this list after any change
that touches `tools/tray/` or `tools/remote/`.

## 1. Build everything

    make build tray

Expected: `dist/knomit`, `dist/knomit-remote`, `dist/knomit-tray` all present.

## 2. Start from a clean state

Remove any stale lockfile from a previous run:

    rm -f ~/Library/Application\ Support/knomit/server.json

## 3. Launch the tray

    KNOMIT_BIN=$PWD/dist/knomit ./dist/knomit-tray

Expected:
- A tray icon appears in the macOS menu bar.
- Clicking it shows: `Running on :<port>`, `Open`, `Restart server`,
  `Start at login [ ]`, `Quit knomit`.
- `~/Library/Application Support/knomit/server.json` exists and contains
  a live PID + port.
- `~/Library/Logs/knomit/serve.log` contains knomit's startup logs.

## 4. Open the window

Click **Open**. Expected: a native window (no browser chrome, no address bar)
opens displaying the knomit web UI.

## 5. Confirm remote discovery

In a new terminal, with the tray still running:

    echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}' | ./dist/knomit-remote

Expected: a valid JSON-RPC response (the server replies with its capabilities
and a session id). With `KNOMIT_MCP_DEBUG=1`, stderr should show
`discovered base-url from lockfile: http://127.0.0.1:<port>`.

## 6. Verify single-instance enforcement

Try to launch a second tray:

    KNOMIT_BIN=$PWD/dist/knomit ./dist/knomit-tray

Expected: exits immediately with `knomit-tray is already running.`

## 7. Toggle autostart

From the tray menu, click **Start at login**. Expected: the checkbox toggles
on; `~/Library/LaunchAgents/com.knomit.tray.plist` exists. Toggle it off;
the file is removed.

## 8. Restart server

Click **Restart server**. Expected: the status line briefly shows `Stopped`
then `Running on :<same-port-or-new>`. The log file `serve.log` shows a
second startup.

## 9. Quit

Click **Quit knomit**. Expected:

- Tray icon disappears.
- `knomit serve` process no longer running: `pgrep -f "knomit serve"`
  returns nothing.
- Lockfile `server.json` removed.
- If a webview window was still open, the user can still interact with it
  but the backend is gone (404s in the UI). This is expected — closing the
  window is the user's responsibility.

## Notes on flakes

- If step 3 fails with `knomit-tray is already running` when you expect a
  clean start, check `~/Library/Application\ Support/knomit/server.json`
  for a stale PID pointing at a still-running `knomit-tray` from a prior
  session (force-quit via Activity Monitor if needed).
- If the "Start at login" plist loads but the tray doesn't actually launch
  at next login, run `launchctl list | grep com.knomit.tray` to confirm
  it's registered, and check `~/Library/Logs/knomit/serve.log` for errors
  after logging back in.
