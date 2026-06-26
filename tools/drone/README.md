# drone

Execute an implementation plan with Claude Code, **unattended** and **sandboxed**,
then let Claude open a PR against the repo.

`drone` does the deterministic, risky setup itself — clean-tree check, branch
creation, `gh` token plumbing, and the OS sandbox policy — and delegates the
open-ended work (implement → test → commit → push → open PR) to a single
headless `claude` run.

## Usage

```bash
go run ./tools/drone --plan .claude/plans/your-plan.md   # or build: go build -o drone ./tools/drone
```

## Configuration

Values are resolved in layers, **lowest to highest precedence**:

```text
built-in defaults  <  TOML config file  <  DRONE_* env vars  <  command-line flags
```

- **TOML file** — pass `--config path.toml`, or drop a `drone.toml` in the
  working directory or `~/.config/drone/`. See [drone.example.toml](drone.example.toml)
  for every key.
- **Env vars** — upper-case the key, prefix `DRONE_`, replace `.` with `_`:
  `sandbox.enabled` → `DRONE_SANDBOX_ENABLED`, `model` → `DRONE_MODEL`. For the
  list-valued keys (`sandbox.allow_domains`, `sandbox.allow_write`) separate
  entries with commas: `DRONE_SANDBOX_ALLOW_DOMAINS=a.example.com,b.example.com`.
- **Flags** — always win.

| Flag | TOML key | Default | Purpose |
|---|---|---|---|
| `--config PATH` | — | — | TOML config file to load |
| `--plan PATH` | `plan` | — (required) | Markdown plan to execute |
| `--repo DIR` | `repo` | `.` | Repository to work in |
| `--base BRANCH` | `base` | `dev` | Branch the PR targets |
| `--branch NAME` | `branch` | `auto/<plan>-<ksuid>` | Working branch (created off base) |
| `--model NAME` | `model` | `opus` | Passed to `claude --model` |
| `--budget USD` | `budget` | `0` (unlimited) | Spend cap (`--max-budget-usd`) |
| `--sandbox` | `sandbox.enabled` | `true` | OS sandbox; `--sandbox=false` disables it (dangerous) |
| `--allow-write DIR` | `sandbox.allow_write` | — | Extra sandbox-writable dir (appended; repeatable) |
| `--allow-domain D` | `sandbox.allow_domains` | — | Extra sandbox-allowed domain (appended; repeatable) |
| `--log-dir DIR` | `log_dir` | `.claude` | Directory for the run's audit logs |
| `--log-level LVL` | `log_level` | `info` | zerolog level: trace/debug/info/warn/error |
| `--dry-run` | — | off | Print plan, args, settings; don't launch |
| `--yes` | `yes` | off | Skip the pre-flight countdown |

Example `drone.toml`:

```toml
base   = "master"
model  = "opus"
budget = 20
log_dir = "/tmp/drone-logs"

[sandbox]
enabled       = true
allow_domains = ["internal.example.com"]
```

## What it does

1. **Preflight** — `claude`/`gh`/`git`/`go` present, repo is a clean work tree,
   base branch exists.
2. **Worktree** — fetch base, then create a fresh `auto/<plan>-<ksuid>` branch in
   an isolated git worktree under `.claude/worktrees/` (done by the tool, not
   the agent, so the run starts from a known point). The repo's own checkout is
   never touched, so several drone runs can execute in parallel. The worktree is
   left in place after the run for inspection — remove it with
   `git worktree remove .claude/worktrees/<name>`.
3. **Token** — read the `gh` token and pass it as `GH_TOKEN`/`GITHUB_TOKEN` to
   the child, because the macOS Seatbelt sandbox blocks the keychain reads that
   `gh`/`git` push would otherwise need.
4. **Launch** — `claude --print --output-format stream-json --verbose
   --permission-mode bypassPermissions --settings '{sandbox…}'`, plan piped on
   stdin. A readable digest streams to the terminal; raw JSON is teed to the log.
5. **PR** — the prompt has Claude build, test, commit (no attribution trailers),
   push, and `gh pr create --base <base>`; the tool then confirms with
   `gh pr view` and prints the URL.

## Sandbox notes

- The sandbox is configured via `--settings` (there is no `--sandbox` flag).
  On macOS it uses Seatbelt; no setup required.
- It restricts **Bash** child processes only (filesystem + network). `Read`,
  `Edit`, and `WebFetch` are governed by `bypassPermissions`, not the sandbox.
- Default writable paths: the run's worktree and `<repo>/.git` (shared git
  plumbing — *not* the rest of the main checkout or sibling worktrees), `/tmp`,
  `~/.knomit`, `~/.claude`, and the Go module + build caches (`go test` fails
  under sandbox without the caches).
- Default allowed domains: GitHub + the Go module proxy. Extend either with
  `--allow-write` / `--allow-domain`.
- In `--print` mode an invalid settings schema is *silently ignored*, which
  would disable the sandbox while leaving `bypassPermissions` on. The current
  schema is verified against the installed `claude` (2.1.150); re-verify after
  major `claude` upgrades, e.g. with `--dry-run` plus a quick out-of-cwd
  write probe.

## Auditing a run

Each run writes a timestamped trio into `log_dir` (default `<repo>/.claude/`),
plus the usual git/GitHub trail:

| Artifact | Contents |
|----------|----------|
| `drone-<ts>.jsonl` | Full `stream-json` transcript: every assistant message, tool call, tool result, and the final `result` event (cost, duration). |
| `drone-<ts>.stderr.log` | Claude's stderr (sandbox denials, warnings, crashes). |
| `drone-<ts>.prompt.txt` | The exact prompt that was sent. |
| `git log <branch>` | The commits Claude made. |
| The PR | Summary + diff on GitHub. |

Read the transcript with `jq`:

```bash
LOG=.claude/drone-<ts>.jsonl

# Everything Claude said:
jq -r 'select(.type=="assistant").message.content[]? | select(.type=="text").text' "$LOG"

# Every tool it ran:
jq -r 'select(.type=="assistant").message.content[]? | select(.type=="tool_use") | .name + "  " + (.input|tostring)' "$LOG"

# Final result, cost, and duration:
jq 'select(.type=="result")' "$LOG"
```

The paths are printed at launch (so a mid-run crash still tells you where to
look) and again in the closing summary.

Requires `claude` (>=2.1.150), `gh` (authenticated), `git`, and `go` on `PATH`.
