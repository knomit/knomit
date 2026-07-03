# `knomit-bridge claw`

`knomit-bridge claw init` scaffolds the OpenClaw integration for a knomit
repo: an OpenClaw plugin that proxies the 7 knomit MCP tools through
`knomit-bridge`, plus the `.agents/skills/knomit-*` skill set that teaches an
agent when to call them. It mirrors `knomit-bridge claude init` (same
snapshot-then-render model, same flags, same merge policy) but targets
OpenClaw's plugin + skills layout instead of Claude Code's.

## What `claw init` scaffolds

Run from the project root (or see `-scope user` below):

```
knomit-bridge claw init -repo <name> -source <slug> [-profile code|chat|generic] [-scope project|user]
```

This does two things against the **live knomit server**, then renders
everything from templates:

1. **Snapshots the server** at `/api/v1/repos/{repo}/branches/{branch}/mcp?profile={profile}`:
   - `SnapshotTools` runs a minimal MCP handshake (`initialize` + `tools/list`)
     and captures the tool manifest.
   - `SnapshotInstructions` re-runs `initialize` and captures the
     profile-scoped `instructions` string the server returns — the same
     guidance text an MCP client would see live.
2. **Renders templates + writes files**:

```
.agents/skills/knomit-bootstrap/SKILL.md
.agents/skills/knomit-decided/SKILL.md
.agents/skills/knomit-guidance/SKILL.md      # source-binding preamble + snapshotted Instructions
.agents/skills/knomit-hypothesize/SKILL.md
.agents/skills/knomit-principle/SKILL.md
.agents/skills/knomit-recall/SKILL.md
.agents/skills/knomit-remember/SKILL.md
.agents/skills/knomit-retract/SKILL.md
.agents/skills/knomit-review/SKILL.md
.agents/skills/knomit-update/SKILL.md
.agents/skills/knomit-why/SKILL.md
openclaw-plugins/knomit/index.mjs            # thin entry point
openclaw-plugins/knomit/register.mjs         # registerKnomit(api) — sync registration
openclaw-plugins/knomit/tools.mjs            # buildToolDefs(manifest, call)
openclaw-plugins/knomit/mcp-client.mjs       # McpStdioClient — stdio JSON-RPC to knomit-bridge
openclaw-plugins/knomit/forward.mjs          # makeCallFn(client) — task-mode-aware forwarder
openclaw-plugins/knomit/knomit-tools.json    # snapshotted tool manifest (7 tools)
openclaw-plugins/knomit/bridge-config.json   # {repo, source, profile} baked in at init time
openclaw-plugins/knomit/openclaw.plugin.json
openclaw-plugins/knomit/package.json
openclaw.json                                # plugin allow-list + knomit_review timeout
```

`index.mjs`, `register.mjs`, `tools.mjs`, `mcp-client.mjs`, and `forward.mjs`
are copied byte-for-byte from `tools/bridge/claw/plugin-src/` (the tested
runtime — R2: the tested files are the shipped files). Everything else is
rendered from `tools/bridge/claw/templates/` with `{{.RepoName}}`,
`{{.Source}}`, and `{{.Instructions}}` substituted.

## The `-scope` flag

`-scope` controls where the scaffold lands, mirroring `claude init`:

| Scope | Skills go to | Plugin goes to | `openclaw.json` goes to |
|---|---|---|---|
| `project` (default) | `<cwd>/.agents/skills/` | `<cwd>/openclaw-plugins/knomit/` | `<cwd>/openclaw.json` |
| `user` | `~/.openclaw/skills/` | `~/.openclaw/extensions/knomit/` | `~/.openclaw/openclaw.json` |

Use `project` for a single-repo setup checked into that repo; use `user` to
install once and have it apply across every project OpenClaw opens for that
user.

## After `claw init`: install plugin dependencies

The scaffolded plugin imports `@sinclair/typebox` and `openclaw/plugin-sdk`,
so it won't load until dependencies are installed. After running `claw init`,
`cd` into the plugin directory and run `npm install`:

```
cd openclaw-plugins/knomit && npm install         # -scope project (default)
cd ~/.openclaw/extensions/knomit && npm install   # -scope user
```

`claw init` prints a one-line reminder of this at the end of its output.

## Plugin -> bridge -> server data path

```
OpenClaw agent
      │  calls a registered tool, e.g. knomit_query({...})
      ▼
register.mjs  (registered synchronously from knomit-tools.json, no await —
               works around bug #47683 where async registration drops tools)
      │
      ▼
forward.mjs   makeCallFn(client) → call(name, args)
      │  ordinary tools:  client.request("tools/call", {name, arguments})
      │  knomit_review:   opts into MCP task mode (task: {ttl}), returns
      │                   {status:"working", resume:taskId} immediately
      ▼
mcp-client.mjs  McpStdioClient — newline-delimited JSON-RPC over the stdio
                pipes of a spawned `knomit-bridge` subprocess (one persistent
                session per OpenClaw process, started once by
                api.registerService)
      │  spawn("knomit-bridge", ["--repo", ..., "--source", ..., "--profile", ...])
      ▼
knomit-bridge   (default mode: stdio↔HTTP proxy — see ../README.md)
      │  POST /api/v1/repos/{repo}/branches/{branch}/mcp?profile={profile}
      ▼
knomit server (HTTP, MCP over streamable-HTTP)
```

Every tool call goes through the same `knomit-bridge` subprocess — the
OpenClaw plugin never speaks HTTP directly. That keeps a single choke point
for future auth (the bridge, not the plugin, would carry credentials) and
means the plugin logic stays generic: all knomit-specific behavior (routing,
task-mode handling, session lifecycle) lives in Go/the bridge and in
`forward.mjs`/`mcp-client.mjs`, not scattered across OpenClaw-specific glue.

`bridge-config.json` is what pins a scaffolded plugin to the repo/source/
profile that `claw init` was run with — without it the plugin would spawn
`knomit-bridge` with defaults (`repo=core`, `profile=code`) regardless of
what was actually requested.

## The `knomit_review` working/resume contract

`knomit_review` is long-running (a fresh session can take 60–120s: clustering
+ dedup over the whole knowledge base), so it's the only tool that opts into
MCP task mode. `forward.mjs`'s `reviewCall` implements the multi-turn loop:

1. **Fresh start** — `call("knomit_review", {})` (no `resume`): issues
   `tools/call` with `task: {ttl: 300000}`. If the server returns a task
   handle (`res.task.taskId`), the forwarder returns
   `{status: "working", resume: taskId}` immediately instead of blocking —
   `execute()` always returns fast so the agent isn't stuck waiting on a
   possibly 2-minute call.
2. **Resume** — `call("knomit_review", {resume: taskId})`: polls
   `tasks/get({taskId})`. While `status` is `working` or `input_required`,
   it returns `{status: "working", resume: taskId}` again — the agent is
   expected to re-call with the same `resume` token until the status
   changes.
3. **Terminal** — once the task is no longer `working`/`input_required`
   (completed, failed, or cancelled), the forwarder calls
   `tasks/result({taskId})`, which blocks server-side until the result is
   ready and returns the real review payload (or throws, for a
   failed/cancelled task).
4. If the server has no task support and ran the call synchronously, the
   forwarder just passes the direct result through — the contract degrades
   gracefully to the original synchronous `knomit_review` behavior.

This mirrors the server-side session contract in `internal/mcp/review.go`:
`knomit_review` with no `session_id` starts a session; with `session_id` +
`response` it continues one; the loop runs until the response carries
`"done": true`. The task-mode wrapper in `forward.mjs` is a transport-level
concern layered on top — the underlying review session semantics
(session_id/response/done) are unchanged.

## "Re-run `claw init` to refresh" model

Files are split into two ownership classes (`isOwnedByIntegration` in
`init.go`):

- **Owned files** — everything under `.agents/skills/` (or
  `.openclaw/skills/`) and `openclaw-plugins/knomit/` (or
  `.openclaw/extensions/knomit/`). These are always overwritten on re-run
  (`init.go` reports them as `Restored:`, not `Created:`). If you hand-edit
  one of these and want it back to stock, delete it (or the whole tree) and
  re-run `claw init`. If the server's tool manifest or Instructions changed
  (new tool added, profile guidance updated), re-running `claw init` is how
  you pick that up — there's no separate "sync" command.
- **Merge-required files** — currently just `openclaw.json`. On first run
  it's created outright. On a later run, if it already exists, `claw init`
  does **not** overwrite it: it writes the freshly-rendered version to
  `openclaw.json.knomit` alongside it and prints a `WARNING:` telling you to
  merge by hand. This protects any manual edits you made to `openclaw.json`
  (e.g. adding other plugins to `allow`) from being clobbered by a refresh.

In short: re-run `claw init` any time you want the skills/plugin runtime and
tool manifest refreshed from the live server; check for a `.knomit`
companion afterwards in case `openclaw.json` needs a manual merge.

## Step 2 (deferred): installing into a real OpenClaw

This repo's live E2E only exercises `claw init` scaffolding against a real
knomit server — **OpenClaw itself is not installed in this environment**, so
the following steps are documented but not run here. To validate a
scaffolded plugin end-to-end in a real OpenClaw install:

1. Run `knomit-bridge claw init -repo <repo> -source <slug> -profile code`
   in the project OpenClaw will open (or with `-scope user` for a global
   install).
2. Point OpenClaw's plugin loader at `openclaw-plugins/knomit/` (or
   `~/.openclaw/extensions/knomit/` for `-scope user`) per however OpenClaw
   discovers local plugins — `openclaw.json`'s `plugins.allow` list already
   includes `"knomit"`.
3. Ensure `knomit-bridge` is on `$PATH` (or adjust `mcp-client.mjs`'s
   `spawnFn` / the plugin's packaging) — `register.mjs` spawns it by bare
   name.
4. Start OpenClaw and confirm:
   - The 7 `knomit_*` tools are visible to the agent and callable — this is
     the residual risk carried forward from Task 1 Step 6: `tools.mjs`
     builds each tool's `input` via `Type.Unsafe(t.inputSchema)`, wrapping
     the raw MCP JSON Schema as a TypeBox schema **without validating it
     through TypeBox's own schema builders**. This has only been verified
     against OpenClaw's plugin SDK types, not against a running OpenClaw
     agent actually surfacing and invoking these tools. If tools don't
     appear, or appear with a malformed/rejected input schema, the fallback
     is `jsonSchemaToTypeBox` (converting the raw schema into a proper
     TypeBox builder call instead of an unsafe passthrough) — see Task 1
     Step 6 probe.
   - `knomit_query({...})` returns real search results.
   - `knomit_review({})` returns `{status: "working", resume: <taskId>}`,
     and re-calling with `{resume: <taskId>}` eventually returns a real
     work item (or `done: true`) instead of hanging or erroring — i.e. the
     multi-turn loop described above actually completes against a live
     OpenClaw task-mode client, not just against the Go-side task-mode
     plumbing this repo's tests cover.

Until this is run against a real OpenClaw install, treat both the
`Type.Unsafe` tool-surfacing path and the task-mode resume loop as verified
only up to the OpenClaw boundary (this repo's own tests + the live scaffold
above), not across it.
