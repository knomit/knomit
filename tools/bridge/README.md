# knomit-bridge

`knomit-bridge` is a stdio↔HTTP adapter that lets stdio-only MCP clients (Claude Desktop, VS Code extensions, and any other client that only supports process-based MCP) talk to a running knomit server.

## Why it exists

The knomit server speaks MCP over streamable-HTTP (`/api/v1/{repo}/mcp`). Many MCP clients only support stdio transport — they launch a subprocess, write JSON-RPC to its stdin, and read responses from its stdout. `knomit-bridge` is that subprocess: it translates between the two transports so no client-side changes are needed.

```
MCP client (stdio)
      │  JSON-RPC over stdin/stdout
      ▼
knomit-bridge
      │  POST /api/v1/{repo}/mcp
      ▼
knomit server (HTTP)
```

## How it works

1. Reads newline-delimited JSON-RPC messages from stdin.
2. POSTs each message to the knomit MCP endpoint, carrying the `Mcp-Session-Id` header once the session is established.
3. The server responds with either `application/json` (simple response) or `text/event-stream` (SSE, used for streaming tool results). Both are normalised to newline-delimited JSON written to stdout.
4. Notifications (HTTP 202, no body) are silently acknowledged.
5. HTTP errors are translated into JSON-RPC error responses so the client always receives well-formed output.

Port discovery follows this priority:

1. Positional `base-url` argument (explicit override).
2. Lockfile written by the knomit server (`~/Library/Application Support/knomit/server.json` on macOS, `$XDG_STATE_HOME/knomit/server.json` on Linux).
3. Default `http://localhost:19278`.

The target is resolved once at startup. If the server later quits or relaunches
on a new port, restart the MCP client so the bridge re-resolves `server.json`.

> **Run one server, not two.** The bridge follows a single `server.json`. Run
> *either* `knomit serve` *or* the desktop app, not both at once — the desktop
> app falls back to an ephemeral port when `:19278` is taken, which would leave
> two servers running and `server.json` pointing at only one of them.

## What the bridge tells the server

Every outgoing request — not just the first — carries two identifying headers,
so the server's Sessions page can say which clients are connected and a session
that outlived a server restart is fully described on its next request:

```
User-Agent:      knomit-bridge/<version>
X-Knomit-Client: id=<instance>;transport=stdio;pid=…;ppid=…;parent=…;host=…;user=…;cwd="…";branch=…;v=<version>
```

`id` is the first 16 hex of a SHA-256 over host, user, cwd, parent app name,
pid and process start time. It is computed once at startup and never
persisted: every bridge PROCESS is its own instance, so three windows on one
project are three sessions. The value pairs are RFC 7239 `Forwarded` style —
`key=value` separated by `;`, values quoted when they contain `;`, `=`, `"`
or whitespace — and the server ignores keys it does not know.

All of it is **self-declared and unverified**. It is there to correlate
sessions, never to authenticate one.

When stdin closes (the MCP client exited), the bridge sends one HTTP `DELETE`
with its `Mcp-Session-Id` — the MCP session-termination request — with a 2s
timeout, then exits. A server that is gone or has moved simply misses it, and
the session goes dead by silence instead. There is no reconnect and no retry.

## Usage

Without a command, `knomit-bridge` runs as the MCP stdio↔HTTP proxy:

```
knomit-bridge [--repo <name> | --lens <name>] [--log <path>] [base-url]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--repo` | none | Repository name; connects to `/api/v1/repos/<repo>/branches/<branch>/mcp` |
| `--lens` | none | Lens name; connects to `/api/v1/lenses/<lens>/mcp` (mutually exclusive with `--repo`) |
| *(neither)* | — | Connects to the session-bound mount `/api/v1/mcp`; the agent binds with `knomit_bind` |
| `--log` | platform default (see below) | Log file path (lumberjack 4 MB rotation) |
| `base-url` | `http://localhost:19278` | Base URL of the knomit server |

With neither flag the bridge connects to `/api/v1/mcp`; the agent binds a repo
or lens with `knomit_bind` and may switch later. Until it does, every other
tool fails. A bound subscription (a read-only follower of a remote branch)
serves reads at the branch it follows and refuses writes.

Flags accept both `-flag value` and `--flag value` styles.

### Subcommands

The bridge also wraps agent-host integration helpers (typically invoked by the
host, not by hand):

```
knomit-bridge claude init [-repo <name>]
                                  # scaffold Claude Code integration files here
knomit-bridge claude hook <event>       # event ∈ session-start, post-edit,
                                        #         post-ask, pre-compact,
                                        #         memory-guard

knomit-bridge antigravity init [-repo <name>|-lens <name>]
                                  # scaffold the Antigravity plugin here
knomit-bridge antigravity hook <event>  # event ∈ pre-invocation
```

`memory-guard` is a PreToolUse hook (matcher `Write|Edit|MultiEdit|Bash`). It
denies a tool call that would write a team-relevant note into Claude Code's
private auto-memory directory (`~/.claude/projects/<cwd>/memory/`), pointing at
`/knomit-remember` instead — that directory belongs to one session on one
machine, so a note left there is invisible to every other agent. `MEMORY.md` and
`type: user` memories are allowed through. It fails OPEN on anything it does not
positively recognise: a malformed payload, an unreadable file, a Bash command
that only READS the directory. A guard on those four tools sees nearly every
action an agent takes, so a false deny would block real work and get it removed.

`claude init` writes two kinds of file. The skills under `.claude/skills/` are
**owned**: overwritten on every run, so deleting one and re-running restores it.
`.mcp.json`, `.claude/settings.json` and `CLAUDE.md` are **merge-required** — they
are mostly yours — and init now MERGES into them in place rather than dropping a
`.knomit` companion beside them:

| File | What a re-`init` does |
|------|-----------------------|
| `.claude/settings.json` | Appends any hook this build ships that the file does not already register. Your own hook entries, matchers, `permissions` and every other key are untouched, and so is your formatting — the merge is a byte-level insertion, not a re-encode. |
| `CLAUDE.md` | Replaces the region between `<!-- knomit:integration vN -->` and `<!-- /knomit:integration -->` with the current block; everything outside it is left alone. A file with no knomit block gains one, appended. |
| `.mcp.json` | Refreshes the entry under the derived server key, or adds it beside your other servers. |

Re-running init is therefore a no-op when nothing has changed: the files come
back byte-identical and nothing is printed for them. When something IS merged,
init says what — `Updated: .claude/settings.json (+PreToolUse memory-guard)`.

Two consequences worth knowing. **A knomit hook you deliberately deleted comes
back on the next `init`**, because init cannot tell "removed on purpose" from
"scaffolded before this hook existed" — and the second is what this merge exists
to fix. To keep a hook off, disable it on your side rather than deleting the
entry. **A hook is matched by its `claude hook <event>` suffix, not by its
command string**, so registering `/path/to/knomit-bridge claude hook post-edit`
under a matcher of your own counts as having that hook and init will not add a
second copy.

A companion file is still written for the two cases a merge cannot decide: a
`.mcp.json` carrying a knomit-bridge entry under some OTHER key (adding ours
beside it would give the project two knomit scopes, which disables the hooks —
see below), and a `CLAUDE.md` whose knomit block has no closing marker, which
init can recognise but not bound. An unparseable `.claude/settings.json` is
neither merged nor companioned: init fails with the file named and writes
nothing at all, because a companion beside a settings.json is exactly the silent
failure this merge removes.

`agy` is accepted as an alias for `antigravity`. Global flags such as `--log`
are accepted before any subcommand.

## Antigravity (`agy`)

`knomit-bridge antigravity init` writes a single owned plugin directory:

```
.agents/plugins/knomit/
├── plugin.json
├── mcp_config.json      knomit-bridge --repo <name> (or --lens <name>)
├── hooks.json           PreInvocation → knomit-bridge antigravity hook pre-invocation
├── rules/AGENTS.md      the "Working with knomit memory" block
└── skills/knomit-*/SKILL.md
```

Unlike the Claude Code scaffold, **nothing here is merge-required**: every file
belongs to the integration and is overwritten on re-run, with no merging and no
companion files. Delete the directory
and re-run `init` to restore it. Use `agy plugin disable knomit` to switch it
off — that setting lives in your own `config.json` and survives a re-`init`.

The hook binds to a project by reading the `mcp_config.json` beside its own
`hooks.json` and trusts its working directory unconditionally, so a
*project-local* plugin (the `.agents/plugins/knomit/` layout above) only ever
reads that project's scope. Do not install this plugin globally with
`agy plugin install .agents/plugins/knomit` — that copies it to a
machine-global location that still carries this project's `mcp_config.json`,
so every project it then runs against would be bound to this project's scope.
It is designed to live project-local; leave it there.

> **Antigravity must have a registered workspace.** Running `agy -p …` from
> inside the project directory is not enough — cwd alone registers nothing, and
> agy then loads no skills, rules, hooks, or MCP tools at all, silently. Launch
> `agy` interactively from the directory, or pass
> `--add-dir <project directory>` on every headless run.

Antigravity exposes MCP tools under bare names (`knomit_query`, not
`mcp__…__knomit_query`), so the `/knomit-*` skills work unmodified.

One further limitation: a markdown-defined custom agent that sets
`inheritCustomizations: false` adopts none of your skills, rules, plugins, or
MCP servers — knomit's included. Run such an agent and knomit is absent by
design.

## MCP client configuration

### Claude Desktop

Add to `~/Library/Application Support/Claude/claude_desktop_config.json` (macOS) or `%APPDATA%\Claude\claude_desktop_config.json` (Windows):

```json
{
  "mcpServers": {
    "knomit-personal": {
      "command": "/path/to/dist/knomit-bridge"
    }
  }
}
```

Multiple repos. Name each key `knomit-repo-<name>` (or `knomit-lens-<name>` for
a lens), matching what `knomit-bridge claude init` generates — the axis is part
of the key so a repo and a lens sharing a name cannot collide:

```json
{
  "mcpServers": {
    "knomit-repo-personal": {
      "command": "/path/to/dist/knomit-bridge",
      "args": ["--repo", "personal"]
    },
    "knomit-repo-work": {
      "command": "/path/to/dist/knomit-bridge",
      "args": ["--repo", "work"]
    }
  }
}
```

Claude Code turns the key into the tool-name prefix `mcp__<key>__knomit_learn`,
and the API caps tool names at 64 characters, so the repo or lens name may be at
most 27 characters. `claude init` checks this before writing anything and fails
with the limit named. In repo mode the name defaults to the directory basename,
so a deeply-named directory can trip it — pass `--repo <shorter>`; the repo name
does not have to match the directory.

> **Claude Code projects: one knomit SCOPE per project.** The above is Claude
> Desktop config, where multiple entries are fine. A Claude Code project's
> `.mcp.json` is different — the hooks (`session-start`, `post-edit`) have to
> bind to exactly one repo, so a project whose knomit entries name two different
> repos or lenses disables them rather than guess which repo to read and write.
> The MCP tools themselves keep working for both; only the hooks stand down, and
> `session-start` says so. Duplicate entries resolving to the *same* scope — what
> the obvious merge of a `.mcp.json.knomit` companion produces — are not
> ambiguous and keep the hooks on. This is why `claude init` refuses to merge a
> `.mcp.json` that already names a knomit scope under a different key: it would
> be creating the ambiguous configuration rather than resolving one.

## Debugging

Set `KNOMIT_MCP_DEBUG=1` to log traffic to stderr:

```
KNOMIT_MCP_DEBUG=1 knomit-bridge
```

Each stdin message, outgoing HTTP request, response status, session ID capture, and stdout write is logged with direction arrows (`←` stdin, `→` stdout).

## Building

```
make build        # produces dist/knomit-bridge
go build -o dist/knomit-bridge ./tools/bridge/
```
