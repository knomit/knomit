# relnotes

Turns a commit range into **release notes**.

## Why it exists

Release bodies are generated from the merge history rather than hand-written.
The tool has two halves with deliberately **opposite failure modes**:

- `changes` is the changelog. It fails loudly if it cannot produce one.
- `distill` is a nicety layered on top. It exits 0 having written nothing when
  anything goes wrong, so a missing API key, a quota trip, or a transport error
  degrades the notes instead of failing a release.

## Usage

```text
relnotes changes -from <rev> -to <rev> [-bodies-out <path>]
relnotes distill
relnotes version
```

### `changes`

Walks the merge commits between the two revisions, resolves each pull request
through `gh`, and renders Markdown grouped by conventional-commit type to
stdout. Non-merge commits reachable from no pull request are listed separately,
so work pushed straight to the branch is never silently dropped.

| flag | default | meaning |
|---|---|---|
| `-from` | _(required)_ | range start revision, exclusive |
| `-to` | `HEAD` | range end revision, inclusive |
| `-bodies-out` | — | also write a body-enriched changelog here, for piping into `distill` |

`-bodies-out` is written from the same `gh` pass, so it costs no extra API
calls. Its output is meant only for `distill` — it is not the changelog.

Requires `git` and an authenticated `gh` on `PATH`.

### `distill`

Reads the body-enriched Markdown on stdin and writes a short `## What's new`
section to stdout, using `$GEMINI_API_KEY`. With no key, or on any error, it
writes nothing and exits 0 — the caller keeps whatever it already had. The whole
call is bounded by a 60s timeout so a stalled connection cannot hang a release.

## In CI

```sh
go run ./tools/relnotes changes -from "$prev" -to "$GITHUB_SHA" \
  -bodies-out changes-bodies.md > changes.md
go run ./tools/relnotes distill < changes-bodies.md > whatsnew.md
```

See [`release.yml`](../../.github/workflows/release.yml) and
[`release-stable.yml`](../../.github/workflows/release-stable.yml).
