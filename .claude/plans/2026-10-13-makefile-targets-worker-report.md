# Makefile targets: server / desktop / dist (worker report)

Branch `build/dist-server-desktop-targets` from `origin/dev` (0a75075b). PR against `dev`.

## Result
- `server`: the former `build` recipe (web, tokenizers-lib, download-ort, then knomit, kb, knomit-okf into `$(DIST)`). `build: server` is a compatibility alias.
- `desktop: server desktop-ui` (was `build desktop-ui`); behaviour unchanged, the server is still compiled once and copied (2ae77359).
- `dist: server desktop` on every platform, with a per-platform final echo (darwin names the .app, others name knomit-desktop). Before, `dist` was `download-ort tokenizers-lib build`.
- The Makefile has no `help` target; nothing to update there.

## Caller audit
| Caller | Was | Action |
|---|---|---|
| `e2e/helpers/binary.ts` (`requireBuild`, `E2E_BUILD=1`, called by `e2e/global-setup.ts`) | `execSync('make dist')` | Changed to `make server` (comment, log line, error text too). Read, not run. Without this, `E2E_BUILD=1` would have started building the desktop app. |
| `e2e/README.md` | `make dist`, `make build` | Reworded to `make server`. |
| Makefile `e2e` comment | named `make dist` | Now `make server`. |
| Makefile `run`, `test` | do not use build/dist (`download-ort tokenizers-lib`) | No change. |
| Makefile `docker`, `docker-amd64` | `docker build`; Dockerfile has no make calls | No change. |
| Makefile `release-server` | `release-server: build` | Now `server` (alias would also work). |
| Makefile `release-desktop`, `desktop-run`, `release` | depend on `desktop` / the two release targets | No change; `desktop` still reaches `server` once. |
| `desktop-app-macos` guard message | "run 'make build'" | Now "run 'make server'". |
| `.github/workflows/*` | only `make setup`, `make release`, `make desktop-deps`, `make print-*`; no `dist`/`build` | No change. |
| `scripts/` | directory does not exist | n/a |
| `Dockerfile` | no make | No change. |
| Docs/comments: `README.md`, `tools/desktop/README.md`, `tools/bridge/README.md`, `tools/okf/README.md`, `tools/desktop/serverbin.go`, `tools/fetchlibs/main.go`, `tools/fetchlibs/spec_test.go` | said `make build` | Now `make server` (README and tools/desktop README also show `dist`/`desktop`). No doc files added; `docs/` untouched. |

## make -n excerpts (darwin-arm64)
- `make -n server` / `make -n build` (identical): `cd web && npm ci && npm run build`; `go run ./tools/fetchlibs -only tokenizers ...`; `... -only ort ...`; `CGO_ENABLED=1 go build ... -o dist/darwin-arm64/knomit .`; `go build ... kb`; `go build ... knomit-okf`; three `ln -sfn`.
- `make -n desktop`: the server set above, then `cd tools/desktop/ui && npm ci && npm run build`, `make desktop-app-macos` (`-tags desktop` build of knomit-desktop, kb, knomit-okf into the bundle, `cp dist/darwin-arm64/knomit .../Contents/MacOS/knomit`, codesign, `codesign --verify --deep --strict`), `Built ... Knomit.app`.
- `make -n dist`: the `desktop` set (server commands appear exactly once, one `go build ... -o dist/darwin-arm64/knomit .`), then `Distribution ready in dist/darwin-arm64/: knomit, kb, knomit-okf and dist/darwin-arm64/Knomit.app`.

## Gates
- `make dist` for real (ad hoc signing), in a worktree with no node_modules: both `npm ci` ran into REAL directories, never a symlink. rc=0. `dist/darwin-arm64/{knomit,kb,knomit-okf}` and `Knomit.app` exist; `codesign --verify --deep --strict dist/darwin-arm64/Knomit.app` passes.
- `make server` for real afterwards: rc=0, no desktop output.
- `go build ./...`: OK (only the pre-existing `-ldl` linker warning).
- `cd e2e && npx playwright test` with that dist: 123 passed, 0 failed, 6 skipped (harness found the binary).
- `E2E_BUILD=1` path: read-only check of `requireBuild()`, it now calls `make server`.
- Zip guard (c49b1162): `release-desktop` recipe untouched; not run.
- Not run: web vitest/lint (no web source changed).

## Recalled facts (5 knomit_query, 4 knomit_explain)
- kb/conventions/build/desktop/2ae77359: desktop depends on the server build and copies `$(DIST)/knomit`. Still true with `server` as the prerequisite; title/body say `build`, now stale in wording.
- kb/gotchas/testing/e2e/7d273c30: `E2E_BUILD=1` ran `make dist`. Now `make server`; fact stale.
- kb/gotchas/build/git/worktrees/node-modules-symlink/e07d5204: `make dist`, `build`, `web` all hit `npm ci`; still true, plus `server`/`desktop`. Followed: real node_modules, no symlink.
- kb/invariants/build/release/macos-app-zip/c49b1162: unaffected (release-desktop not touched).
- Query 1 also returned kb/architecture/build/artifacts/dca50bdb and kb/decisions/desktop/windows/gui-subsystem/74ccadab (snippets only), no conflict.

## Facts to write after merge
1. UPDATE kb/conventions/build/desktop/2ae77359: replace `build` with `server` (`desktop: server desktop-ui`; the guard says "run 'make server'"); add that `build` is an alias of `server`, and `dist` = `server` + `desktop` on every platform (the server still compiles once). Re-pin the Makefile ref to the merge commit.
2. UPDATE kb/gotchas/testing/e2e/7d273c30: `E2E_BUILD=1` runs `make server` (not `make dist`) because `dist` now also builds the desktop app; the by-hand hint says `make server`. Still runs `npm ci` in web/. Re-pin refs.
3. UPDATE kb/gotchas/build/git/worktrees/node-modules-symlink/e07d5204: the dangerous set is `make server`, `build`, `dist`, `desktop`, `web` (all reach `npm ci` in web/); `dist`/`desktop` also `npm ci` in tools/desktop/ui; E2E_BUILD=1 reaches it via `make server`.
4. Any other fact naming `make dist`/`make build` for the server-only build (check kb/architecture/build/artifacts/dca50bdb): re-word to `make server`.
5. NEW convention: "`make dist` is end to end (server then desktop) and needs the desktop toolchain (GTK4/WebKitGTK on Linux); server-only callers (e2e harness, release-server) use `make server`. Misreading: `dist` is not the server tarball." Refs: Makefile, e2e/helpers/binary.ts.
