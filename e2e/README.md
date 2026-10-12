# knomit e2e

Playwright suite (not run in CI). It starts its own `knomit serve` on a free port with a temp `KNOMIT_HOME`.

## Prerequisites (the harness does not build)

The suite needs two build outputs and fails fast, naming these commands, when either is missing:

```
cd web && npx tsc -b && npx vite build        # web/dist/index.html (embedded in the binary)
CGO_ENABLED=1 go build -o dist/knomit .       # or: make build
cd e2e && npm ci && npx playwright install chromium   # once (make e2e-setup)
```

The harness used to run `make dist`, whose `npm ci` in `web/` empties a symlinked `web/node_modules` in a worktree.
`E2E_BUILD=1 make e2e` restores that behaviour. `E2E_BINARY=<path>` launches a binary other than `dist/knomit`
(the ONNX runtime is looked up in `lib/` next to the resolved binary).

`@playwright/test` is pinned to the release whose chromium build is the one in the local browser cache; bump the
pin and reinstall browsers together.

## Run

```
make e2e            # cd e2e && npx playwright test
```

## Cleanup

The state file `e2e/.e2e-state.json` (server pid, temp home) is written as soon as the server is spawned, and teardown
(or a failed setup) kills the server and deletes the home. If a run is killed hard anyway:

```
make e2e-clean      # kills knomit serve processes started from THIS checkout's binary whose KNOMIT_HOME is a
                    # knomit-e2e-* dir, then removes knomit-e2e-* / knomit-remote-* temp homes
```

It never touches a `knomit serve` whose binary is elsewhere (for example a long-running local instance).
