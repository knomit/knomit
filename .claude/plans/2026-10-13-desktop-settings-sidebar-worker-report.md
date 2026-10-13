# Worker report: desktop Settings sidebar (variant A)

Branch `feat/desktop-settings-sidebar`, PR against `dev`.

## What changed
- `tools/desktop/windows.go`: Settings window 520x470 -> 680x500, DisableResize kept, window Name kept (it gates Save, Restart, PublicKeyLine, InstallBundle).
- `tools/desktop/ui/src/SettingsForm.tsx`: sidebar (General, Network, Fleet, Logs) with the server status (pip, address, "· attached", "configured port was busy") at its foot; pages are kept mounted and hidden with `hidden`, so edits and an open fleet-root confirmation survive navigation. One Cancel/Save footer and one restart bar for every page. HTTP scope is native radios drawn as a segmented control; KNOMIT_HTTP/KNOMIT_HOST put the amber `env` chip on that row and disable it. A refused field marks its page's sidebar item with a red dot ("has an error" in the accessible name) and a failed Save switches to the first such page unless the page in view holds one. The old "one grouped list, not icon-headed sections" comment is replaced by the reversal.
- `Row.tsx` (new): label + sublabel left, control right, notes under the row.
- `icons.tsx` (new): Gear, Globe, Broadcast, Folder, Copy copied verbatim from web/src/icons.tsx; `icons.test.tsx` reads both files as text and pins each copy byte-for-byte. Nothing imports web/.
- `FleetIdentity.tsx`: same rows/cards; behaviour unchanged (install, root confirmation, refusal classes).
- `App.css`: rewritten for the sidebar layout; sublabels use --k-label (#9a9a9a). The footer outcome now wraps up to three lines instead of clipping to one ("Fleet por…" fix).

## Verification
- `npx tsc -b` in tools/desktop/ui: clean.
- `npx vitest run` in tools/desktop/ui: 5 files, 123 tests passed.
- `go test -tags desktop ./tools/desktop/...`: all ok.
- Sabotage (against commit c8860997, one at a time, restored with git checkout): dropping the page switch fails 8 tests; dropping the sidebar mark fails 2; dropping the scope env chip fails 1.
- Browser: Vite dev server with a stubbed @wailsio/runtime, Chromium at 680x472 (the content area under a 28px title bar); screenshots of every page and the env, scope-lock, all-networks, other-host, attach+restart, busy-port, enrolled, root-confirm, error-switch, error-mark, long-save-message and no-log-file states. Not seen in a real Wails WebKit/WebView2 window.

## Deviations from the mockup
- Scope "other" (knomit.toml binds a specific address): the mockup replaced the segmented control with the text "specific address". Kept the control with nothing selected plus the existing note, because the current code lets the user move from "other" to a scope and the brief keeps behaviour.
- Restart bar keys render as plain text (`auth.require`), not mono `<code>` as in the mockup; unchanged copy path (restartReason).
- Logs group header is "Output" as in the mockup HTML; the older A-logs PNG said "Server log".
- The env-owned select keeps its chevron and #bbb value (index.css's disabled shorthand would have dropped it).
