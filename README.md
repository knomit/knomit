# knomit

> ⚠️ **Work in progress.** knomit is under active development. It is fully
> functional and used internally every day — but interfaces may change and
> **no guarantees, warranties, or stability promises are made.** Use at your
> own risk.

Git-backed knowledge base for AI agents. **Knowledge + commit.**

knomit stores structured facts as markdown files in a Git repository, organized
by an ontological hierarchy. Each agent gets its own branch (derived from its
SSH key fingerprint); consensus lives on `main`. It speaks MCP, so agents read
and write long-term memory through the tools they already use.

📚 **Full documentation lives at [knomit.io](https://knomit.io) — guides and
reference are under [knomit.io/docs](https://knomit.io/docs).**

## Quick start

Requires **Go 1.24+**, **Node.js + npm**, the **Git CLI**, and a **C compiler**
(the build is CGO-based). See [the docs](https://knomit.io/docs) for the full
prerequisites and platform notes.

```sh
git clone https://github.com/knomit/knomit.git
cd knomit
make setup    # one-time: download native libs (ONNX Runtime + graphqlite)
make build    # build the web frontend, then the Go binaries
make run      # start the server on http://localhost:19278
```

Open <http://localhost:19278/> for the web UI — the default repo is created
automatically on first run. To connect Claude Code or another MCP client, see
[MCP setup in the docs](https://knomit.io/docs).

## What you get

- **MCP server** exposing cognitive tools — `learn`, `query`, `explain`,
  `update`, `retract`, `review`, `hypothesize` — for any MCP client (Claude
  Code, Claude Desktop, Cursor, VS Code, …).
- **Facts as markdown + Git** — every learning moment is an atomic commit with
  full provenance.
- **Ontological hierarchy** — facts inherit context from their place in the
  tree; a fact at `kb/geography/` applies to everything beneath it.
- **Automated synthesis** — prune duplicate/stale facts and distill
  higher-order insights with an LLM.
- **Web UI + REST API** (HAL+JSON) and a native **desktop app** (Wails v3).

Installation, configuration, MCP setup, the HTTP API, synthesis, remote sync,
and environment variables are all documented at
**[knomit.io/docs](https://knomit.io/docs)**.

## License

knomit is source-available under the
[Functional Source License (FSL-1.1-ALv2)](LICENSE).

In short: **you can use, copy, modify, and self-host knomit freely for almost
anything — including internal commercial use.** The one thing you may not do is
make a **Competing Use** — offering knomit (or a substantially similar
substitute) to others as a commercial product or service. Two years after each
release, that version automatically becomes available under Apache 2.0.

Want to offer knomit commercially, or do something the FSL doesn't permit? A
commercial license is available — see
[COMMERCIAL-LICENSE.md](COMMERCIAL-LICENSE.md).
