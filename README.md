# knomit

> ⚠️ **Work in progress.** knomit is under active development. It is fully
> functional and used internally every day — but interfaces may change and
> **no guarantees, warranties, or stability promises are made.** Use at your
> own risk.

**A distributed, decentralized knowledge distillation factory.** Knowledge +
commit.

📚 **Full documentation lives at [knomit.io](https://knomit.io) — guides and
reference are under [knomit.io/docs](https://knomit.io/docs).**

## Why

Today every agent starts from nothing, re-derives what a thousand others
already worked out, and forgets it when the session ends. Knowledge that costs
real tokens, time and judgment to produce evaporates on contact.

knomit is a fabric that stitches together many agents and many knowledge bases
so that what any one of them learns is accumulated, distilled, and put to use
by all of them. Learning is paid once, across the whole network, instead of
once per agent.

## What the factory does

**Accumulate.** An agent that learns something writes it down as a typed,
signed fact on its own branch; peers fetch and merge it; the corpus converges
on one best statement of each truth rather than accumulating copies.

**Distill.** Accumulation alone would just be a bigger pile. The factory
continuously reviews what has been gathered, merges what is redundant, surfaces
where facts about unrelated subjects follow the same pattern, and derives
higher-order conclusions that no single source states.

**Use.** Distilled knowledge flows back into agents' working context and into
the decisions of the people who work alongside them. Every fact carries its
provenance, its confidence, and the full history of how it came to be believed.

## Git is the substrate

Because the substrate is git, each operation means what it says:

| Git          | In knomit                                              |
|--------------|--------------------------------------------------------|
| commit       | a signed assertion of belief                           |
| branch       | an agent's identity                                    |
| merge        | consensus                                              |
| conflict     | a disagreement surfaced for someone to settle          |
| log          | the timeline of what the fabric believed, and when     |

Git is the only source of truth; indexes, embeddings and search are
rebuildable caches.

## Not tied to a domain or a host

A knowledge base can hold facts about a codebase, a market, a supply chain or a
research field, and a participant can be a coding agent, a research agent, an
analyst or a team. Knowledge bases can be viewed together through **lenses**,
so a fabric can span machines, teams and eventually organizations while each
participant keeps control of what it publishes and what it trusts.

The aim is not a better memory for a single assistant. It is the shared,
auditable, ever-improving body of knowledge that agents and people build
together — and that every new participant inherits on arrival.

## What's in the box today

- **MCP server** for any MCP client (Claude Code, Claude Desktop, Cursor,
  VS Code, …) with tools to `learn`, `query`, `explain`, `update`, `retract`,
  `review`, `hypothesize`, follow `changes`, and stage work in `experiment`
  branches that commit or roll back as a unit.
- **Typed facts as markdown in git** — each fact sits in an ontology
  (`kb/<topic>/<category>/…`), carries a type, confidence, refs and motifs,
  and every write is an atomic commit with full provenance.
- **Per-agent branches** derived from the agent's SSH key fingerprint;
  consensus lives on `main`.
- **Synthesis pipeline** — prune duplicates and stale facts, distill
  higher-order insights, and discover cross-subject patterns with an LLM.
- **Lenses** — mount several knowledge bases and query them as one.
- **Web UI + REST API** (HAL+JSON) and a native **desktop app** (Wails v3).
- **OKF export** — `knomit-okf` publishes a knowledge base as a portable
  [Open Knowledge Format](https://github.com/GoogleCloudPlatform/knowledge-catalog)
  repository of plain markdown: clone once from a git URL, `sync` to refresh,
  push to any git host. See [tools/okf/README.md](tools/okf/README.md).

Installation, configuration, MCP setup, the HTTP API, synthesis, remote sync,
and environment variables are all documented at
**[knomit.io/docs](https://knomit.io/docs)**.

## Quick start

Requires **Go 1.24+**, **Node.js + npm**, the **Git CLI**, and a **C compiler**
(the build is CGO-based) with the **SQLite development header** on its include
path — `sqlite3.h`, from `libsqlite3-dev` on Debian/Ubuntu, the SDK on macOS,
or `mingw-w64-x86_64-sqlite3` on Windows/MSYS2. See
[the docs](https://knomit.io/docs) for the full prerequisites and platform
notes.

```sh
git clone https://github.com/knomit/knomit.git
cd knomit
make setup    # one-time: fetch native libs (on Windows this also builds
              # libtokenizers.a from source, which needs Rust/cargo plus
              # `rustup target add x86_64-pc-windows-gnu`)
make build    # build the web frontend, then the Go binaries
make run      # start the server on http://localhost:19278
```

Open <http://localhost:19278/> for the web UI. A fresh knomit serves no
repositories — there is no default repo — so create your first one from the UI,
or over the API:

```sh
curl -X POST http://localhost:19278/api/v1/repos \
  -H 'Content-Type: application/json' \
  -d '{"name":"work","mode":"preset","ontology_preset":"default"}'
```

To connect Claude Code or another MCP client, see
[MCP setup in the docs](https://knomit.io/docs).

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
