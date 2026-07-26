# knomit-okf

`knomit-okf` turns a knomit knowledge base into a publishable
[OKF](https://github.com/GoogleCloudPlatform/knowledge-catalog) repository, and
keeps it in sync — using nothing but a git URL.

```sh
knomit-okf clone -b main http://localhost:19278/git/knomit-kb my-kb
cd my-kb
git remote add origin git@github.com:me/my-kb-okf.git
git push -u origin main

# later — by you, or by anyone who cloned your published repo
knomit-okf sync
git push
```

## Why it exists

A knomit KB is a private, agent-shaped thing: facts under `kb/`, one agent
branch per author, provenance edges, an embedding index. OKF is the portable,
consumer-shaped thing: plain markdown concept documents with standard
frontmatter, directory indexes, and a changelog, readable by any tool that
knows the format and by any human with a browser.

`knomit-okf` is the mapper between them. It is a **standalone CLI**, not a
server feature: the export runs against a plain git clone of the KB, so no
knomit server, database, or cache is involved beyond serving the git URL.

It also **never pushes**. It commits; you push — your remote, your credentials,
your cadence.

## Mental model

The **output directory _is_ the export**. There is no second checkout, no cache
directory, and no state held by a knomit server.

```
my-kb/
├── .git/
│   ├── refs/heads/main                  ← the bundle: what you publish
│   └── refs/knomit-okf/source/main      ← the KB's own history, private
├── index.md      ┐
├── log.md        │
├── kb/           ├─ owned by knomit-okf: written completely, pruned
├── views/        │  of anything it did not write
├── .knomit-okf.yaml ┘
├── README.md     ┐  yours. Never read, never written, never staged.
└── LICENSE       ┘
```

The knowledge base's own history is fetched into the same `.git` under
`refs/knomit-okf/source/*`, which is **outside `refs/heads/*`** — so git's
default push refspec never publishes it, and it is never checked out. That is
the whole reason one directory can be both the export and its own source.

> **The one leak path** is an explicit `git push --mirror` or a `refs/*`
> refspec, which would publish the source refs. This is not defended against in
> code — don't do it.

## Install

`knomit-okf` is built by the top-level `make build` and ships in the server
tarball and the desktop bundles:

```sh
make build            # → dist/<platform>/knomit-okf
```

Or on its own — it is pure Go, no CGO, no native libraries:

```sh
go build -o knomit-okf ./tools/okf/
```

## Commands

```
knomit-okf clone    [-b <branch>] [--publish-source] <kb-url> <dir>
knomit-okf sync     [-b <branch>] [--source <url>] [--publish-source]
knomit-okf branches [--source <url>] [--no-fetch]
knomit-okf version
```

`sync` and `branches` run **inside** an export directory; `clone` creates one.

### `clone`

Creates `<dir>` as an OKF repository for `<kb-url>`. The directory must be
missing or empty — refusing a non-empty one is deliberate, because `clone`
writes a whole repository and silently merging into an existing directory is not
recoverable.

| Flag | Default | Description |
|---|---|---|
| `-b <branch>` | the source's HEAD branch | Source branch to export |
| `--publish-source` | off | Record the KB URL in `.knomit-okf.yaml` so a stranger who clones the published repo can sync it |

`clone` is the only command that records **where the knowledge comes from**: it
creates the `knomit-source` git remote in `.git/config`. That remote is local
and never travels.

Without `-b`, the source's own advertised HEAD is used, falling back to
`main`/`master`, and finally to the sole fetched branch. Note that a knomit
server's HEAD is usually the *agent* branch, not consensus — pass `-b main`
when you mean to publish consensus.

### `sync`

Re-exports the branch this directory tracks and commits the result.

| Flag | Default | Description |
|---|---|---|
| `-b <branch>` | the branch in `.knomit-okf.yaml` | Source branch to export |
| `--source <url>` | the `knomit-source` remote | Override the KB URL **for this run only** |
| `--publish-source` | off | Record the KB URL in `.knomit-okf.yaml` |

`--source` is genuinely one-off: it does **not** repoint the stored remote for
future runs. A KB that has genuinely moved is repointed deliberately, with
`git remote set-url knomit-source <url>`.

Source URL precedence: `--source` → the `knomit-source` remote → the `source`
field in the committed config.

### `branches`

Lists every source branch and how far each exported bundle has fallen behind.

| Flag | Default | Description |
|---|---|---|
| `--source <url>` | the `knomit-source` remote | Override the KB URL for this run |
| `--no-fetch` | off | List from what is already fetched, without contacting the remote |

Fetching by default is the point — "behind by N" computed against stale refs
would be worse than not reporting it. `--no-fetch` is for offline use, and says
so in its output.

Statuses: `up to date`, `N commits behind`, `not exported`, `never synced`,
`diverged — re-sync rewrites the bundle` (the source moved in a way that does
not contain what was exported), and `source branch gone`.

## The branching model

**One output repository carries every branch of one KB**, and each output
branch is named after the source branch it exports:

| source branch | output branch |
|---|---|
| `main` | `main` |
| `agent/mindev.local-8ef0cd32` | `agent/mindev.local-8ef0cd32` |

Output branches are **orphans** — unrelated snapshots, not descendants of one
another. Bundles for different source branches share no history, because a
shared one would produce only meaningless diffs between them.

The join between a source branch and its output branch is each output branch's
committed `.knomit-okf.yaml`, not its name — which is why `branches` can
inspect every branch without checking any of them out.

## What gets written

`knomit-okf` owns exactly five entries, and prunes *inside* them:

```
index.md              root index, links kb/ and views/
log.md                changelog: creations and updates, grouped by date
kb/                   concept documents + per-directory index.md
views/                derived cross-cutting views:
  ├── index.md
  ├── domains/        one document per domain tag
  ├── entities/       one document per entity
  ├── retired.md      retracted and superseded facts (index only, never documents)
  ├── synthesis.md    higher-order facts distilled from clusters, newest first
  ├── hypotheses.md   falsifiable predictions, newest first
  └── methodology.md  lessons about how to reason over this KB
.knomit-okf.yaml      sync state: branch, synced_commit, tool_version, source?
```

A digest with no facts produces no page rather than an empty one, so a small KB
will not have all three of `synthesis.md` / `hypotheses.md` / `methodology.md`.

**Everything else in the repository belongs to the publisher** — `README.md`,
`LICENSE`, `.github/` — and is never read, written, or staged. Staging with
`--all` would sweep a publisher's uncommitted edits into an okf commit; the tool
stages only the paths it owns.

Deleting inside the owned paths is not optional: overlaying files can never
remove them, so a retired fact's document would stay published forever,
contradicting the `views/retired.md` in the same bundle.

### Concept documents

One markdown file per fact, at the fact's ontology path, with a
slugified-title filename. Frontmatter carries meaning in **standard OKF keys**
and knomit specifics in `knomit_*`:

```yaml
---
type: invariant                       # from the TOPIC (kb/invariants/…), singularized
title: 'Embeddings are mandatory: …'
resource: knomit://3ec012f5b4d2/kb/invariants/embeddings/mandatory/f34da09a.md
tags: [embeddings, store, app, …]     # domains, then entities, then kind
timestamp: "2026-06-08T15:02:45Z"
generated:
  by: knomit/authored
  at: "2026-06-08T15:02:45Z"
knomit_type: policy                   # the LEAF fact.Type, preserved
knomit_kind: pragmatic
knomit_confidence: 0.95
knomit_origin: authored
knomit_domain: [embeddings, store, app]
knomit_entities: [app.New, embeddings.NewEmbedder, …]
knomit_refs: [src://knomit/internal/app/app.go, …]
knomit_path: kb/invariants/embeddings/mandatory/f34da09a.md
---
```

OKF's `type` maps from the fact's **topic** (the first path segment under
`kb/`), not from its leaf `fact.Type` — `type` is OKF's only required key and
answers "what this document is". The leaf type is preserved as `knomit_type`.

The body carries the authored text, then `# Related` (domains and entities,
linked into `views/`), `# Citations` (refs, with cited facts resolved to titled
relative links), and `# History` when the fact has more than one revision.

Retirements are rendered as an **index only**, never as concept documents:
their claims have been disavowed, and a conformant consumer may ignore
`status: deprecated`, so an ingestible document would invite re-ingestion of
withdrawn knowledge.

## Determinism

Rendering is a pure function of the source commit, and the export commit is
timestamped from the **source commit**, never the clock. Two people exporting
the same knowledge get byte-identical repositories.

That is also what makes a re-sync cheap: an unchanged source needs no work at
all, rather than a full render whose output happens to match.

```
✓ Fetching    4 branches  7ms
· Checking    already up to date at ea7c116b

✓ Nothing to do in 72ms
```

Every bundle is validated against OKF conformance rules **before** it is
committed — a non-conformant bundle is an error, not a commit. The whole value
of the export is that a consumer can trust the format without re-validating.

## Authentication

Two remotes are involved, and they authenticate very differently.

### Pushing to your publish remote — full git auth

`knomit-okf` never pushes. You run `git push` yourself, so the git CLI handles
it with everything you normally have: SSH keys, `ssh-agent`, credential
helpers, `~/.ssh/config`, tokens. Publishing to a private GitHub/GitLab repo
needs no special handling.

### Fetching the knowledge base — go-git defaults

Fetching the KB happens **in-process via go-git**, and `knomit-okf` does not
supply credentials. That means you get go-git's defaults, which are narrower
than the git CLI's:

| Source URL | Works? | Notes |
|---|---|---|
| `http://host:19278/git/<repo>` | ✅ | The normal case — a knomit server's git endpoint is unauthenticated |
| `/path/to/repo` or `file://…` | ✅ | Local filesystem |
| `https://user:TOKEN@host/kb.git` | ✅ | Credentials embedded in the URL |
| `git@host:me/kb.git` | ⚠️ | **ssh-agent only** — see below |
| `https://host/private.git` | ❌ | No credential helper, no `.netrc` — this is anonymous |

**SSH uses the agent, and only the agent.** go-git falls back to
`NewSSHAgentAuth`, which reads signers from `ssh-agent`. It does **not** open
`~/.ssh/id_ed25519` from disk and never prompts for a passphrase. Load your key
first:

```sh
ssh-add ~/.ssh/id_ed25519      # then knomit-okf clone git@host:me/kb.git …
ssh-add -l                     # "The agent has no identities" ⇒ SSH will fail
```

**`~/.ssh/config` is mostly ignored.** Only `Hostname` and `Port` are honored.
`IdentityFile`, `User`, `ProxyJump`, and `ProxyCommand` are not — so a `Host`
alias resolves its address but not its key or user.

**`known_hosts` is enforced, with no prompt.** There is no interactive
"continue connecting (yes/no)?" — an unknown host is a hard failure. Add it
first:

```sh
ssh-keyscan git.example.com >> ~/.ssh/known_hosts
```

**HTTPS has no credential helper.** Embed the credentials in the URL instead:

```sh
knomit-okf clone https://me:$TOKEN@git.example.com/kb.git my-kb
```

The URL is stored in `.git/config` as the `knomit-source` remote, which is
local and never pushed.

> ⚠️ **Never combine URL credentials with `--publish-source`.** That flag writes
> the source URL into the committed `.knomit-okf.yaml` — token and all — and you
> would then push it. Use `--publish-source` only with a URL that is safe to
> publish.

If go-git's defaults don't fit your setup, the escape hatch is to fetch with
the git CLI, which has full credential support, and point `knomit-okf` at a
local clone:

```sh
git clone --mirror git@host:me/kb.git /srv/kb-mirror.git   # git CLI: full auth
knomit-okf clone -b main /srv/kb-mirror.git my-kb
# refresh: git -C /srv/kb-mirror.git fetch && (cd my-kb && knomit-okf sync)
```

Because the export is deterministic, this produces the **same commit** as
exporting the same branch directly over HTTP or SSH — the transport is not part
of the output.

## Privacy

A private KB's address must never travel just because someone exported it.

- The `knomit-source` remote lives in `.git/config` and is **never pushed**.
- Source history lives at `refs/knomit-okf/source/*` and is **never pushed** by
  the default refspec.
- The KB URL is written into the committed `.knomit-okf.yaml` **only** with
  `--publish-source`.

Use `--publish-source` when the KB is publicly reachable and you want strangers
who clone your published repo to be able to `knomit-okf sync` it themselves.

## Gotchas

- **Flags come before positional arguments.** Go's `flag` package stops parsing
  at the first non-flag argument, so `clone <url> <dir> -b main` fails with a
  usage error — `-b main` is read as a third positional. Write
  `clone -b main <url> <dir>`.
- **`knomit-okf` never pushes.** After a successful `clone` or `sync` you still
  need `git push`. This is deliberate: your remote, your credentials.
- **`git push --mirror` leaks the source history.** So does any `refs/*`
  refspec. Push branches, not refs.
- **`--publish-source` is a no-op on an up-to-date sync.** The tool
  short-circuits before rendering when nothing changed, so the config is never
  rewritten. Combine it with a run that actually re-renders, or use it on
  `clone`.
- **`--source` does not stick.** It overrides the URL for one run only. Use
  `git remote set-url knomit-source <url>` to move a KB permanently.
- **Uncommitted publisher edits don't force a re-render**, and a damaged bundle
  does: `sync` compares the working tree against the index for the owned paths
  only.

## Scope

v1 is **per-repo and export-only**. Lenses (federated multi-repo views) are
explicitly out of scope, and there is no import path — OKF out, not OKF in.

## See also

- [OKF specification](https://github.com/GoogleCloudPlatform/knowledge-catalog/blob/main/okf/SPEC.md)
- [knomit.io/docs](https://knomit.io/docs) — the rest of knomit
