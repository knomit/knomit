# Research: Selective Sharing

## Problem

Not all knowledge is shareable. Some facts are private (personal preferences, internal project details, sensitive observations). Others are worth publishing for others to browse and learn from. The system needs a way to maintain one authoritative knowledge base while controlling what's visible to others.

## Approaches

### Option A: Two Separate Repos

A private repo (full brain) and a public repo (curated subset). Facts are manually duplicated or copied between them.

**Problems:**
- Two sources of truth. Which one is canonical?
- Updating a shared fact means updating it in two places
- Forgetting a fact means forgetting it in two places
- Drift is inevitable

This is the wrong model. Reject.

### Option B: One Repo, Publish a Projection

Everything lives in one repo. A `visibility` field in frontmatter controls what gets published. A publish step filters and pushes shared facts to a separate public repo.

```yaml
---
domain: [security]
confidence: 0.85
visibility: shared       # private (default) | shared
---
```

The public repo is read-only — a derived artifact, never edited directly. Publishing is:

```
1. Filter facts where visibility: shared
2. Copy them to the public repo
3. Commit and push
```

**Pros:**
- Single source of truth
- Safe by default (private unless explicitly shared)
- Publishing is a deterministic projection — no judgment needed
- Public repo is a clean, standalone knowledge base others can clone and browse

**Cons:**
- Requires a publish step (cron, hook, or manual)
- Two repos to manage, even if one is derived
- Publish step must handle deletions (fact goes private → remove from public repo)

### Option C: Git Submodules

The shared facts live in a separate repo, mounted as a submodule inside the main brain.

```
my-knomit/
  know/
    personal/          ← private facts (main repo)
    projects/          ← private facts (main repo)
    shared/            ← submodule → github.com/me/knomit-public
      security/
      programming/
```

When you learn a shared fact, it lands in `know/shared/` which is the submodule. The commit goes to the submodule's repo directly. Others clone just the submodule repo.

**Pros:**
- No publish step — commits to shared facts are immediately in the public repo
- Single editing experience (files are in your tree, you edit them normally)
- Git handles the boundary natively

**Cons:**
- Submodules are painful (git's worst feature by consensus)
- Forces ontology to reflect visibility: `know/shared/security/` vs `know/security/`. Visibility leaks into the path, same problem as namespacing imports by source
- Moving a fact from private to shared means moving it between directories (and repos)
- `knomit_learn` needs to know which repo to commit to based on visibility
- Submodule pointer in the parent repo needs updating after each commit

### Option D: One Repo, Visibility as Metadata, Filtered Clone

Same as Option B (visibility in frontmatter) but instead of a separate publish step, use git's sparse checkout or a filtered clone mechanism. Others get a view of your repo that only contains shared facts.

This is more elegant in theory but git doesn't natively support content-level access control. You'd need a proxy or a custom git server that filters on clone/fetch. Over-engineered.

## Recommendation: Option B

One repo. `visibility` field in frontmatter. Default `private`. A publish step projects shared facts to a public repo.

### Why

- **Single source of truth** — all knowledge lives in one place, tools operate on one repo
- **Safe by default** — forgetting to set visibility means private, not public
- **Clean separation** — the public repo is a standalone knowledge base with no traces of private facts
- **Simple** — the publish step is a script, not infrastructure
- **Ontology stays clean** — `know/security/sql-injection.md` is the path whether it's private or shared. Visibility doesn't affect organization

### The `visibility` Field

Added to the fact schema:

| Value | Meaning |
|-------|---------|
| `private` | Default. Not published. Only visible in your local repo. |
| `shared` | Published to the public repo. Others can browse and learn from it. |

Omitting the field means private. This is critical — existing facts without the field remain private automatically.

### Publishing

```
knomit publish [--to <repo-url>]
```

Steps:
1. Scan all facts for `visibility: shared`
2. Compare against the current state of the public repo
3. Add new shared facts, update changed ones, remove facts that went private or were forgotten
4. Commit and push to the public repo

This can run as:
- A manual command (`knomit publish`)
- A post-commit hook (publish on every change)
- A cron job (publish periodically)
- A GitHub Action (publish when main is updated)

### Changing Visibility

```
knomit share know/security/sql-injection.md     # sets visibility: shared
knomit unshare know/security/sql-injection.md   # sets visibility: private
```

Or just edit the frontmatter directly. The next publish picks up the change.

### What the Public Repo Looks Like

A normal knomit repo. Same structure, same fact format. Someone browsing it (or an agent doing cross-repo knowledge discovery) doesn't know or care that it's a projection. It's just a knowledge base they can clone and learn from.

```
knomit-public/
  know/
    security/
      sql-injection-prevention.md
      xss-mitigation.md
    programming/
      bun-sqlite-usage.md
```

No metadata about the private repo. No submodule pointers. No "imported from" markers. Just facts.

## Knowledge Packs

The `visibility: shared` field answers "can this be published?" but not "published *where* and *for whom*?" A flat shared/private toggle is too coarse. You might want:

- A "security hardening" pack for your team
- A "Bun patterns" pack for the community
- A "project onboarding" pack for new hires on a specific project

A **knowledge pack** is a named, curated projection of your knowledge base.

### Definition

A pack is a file in the repo:

```yaml
# packs/bun-patterns.yaml
name: bun-patterns
description: "Patterns and gotchas for Bun runtime development"
target: https://github.com/me/knomit-bun-patterns
include:
  # by domain
  - domain: bun
  - domain: javascript
    entities: [bun, bun:sqlite, bun:test]
  # by explicit path
  - path: know/programming/bun-html-imports.md
  - path: know/programming/bun-sqlite-macos.md
exclude:
  - path: know/programming/bun-internal-workaround.md
```

A pack is a query against your knowledge base. `include` selects facts by domain, entities, path globs, or combinations. `exclude` removes specific facts from the result. The pack definition itself lives in your private repo — the target repo only gets the matching facts.

### Why Packs Instead of Just `visibility: shared`

The `visibility` field is **per-fact, per-audience**. But you don't have one audience — you have many. A fact about SQL injection might be shared with your security team but not in your public "Bun patterns" pack. Encoding this as `visibility: [bun-patterns, security-team]` on every fact pushes audience management into every file. It doesn't scale and it clutters the frontmatter.

Packs invert the relationship: the pack declares what it wants, not the fact. The fact stays clean. The pack is the lens.

This also means the same fact can appear in multiple packs without the fact knowing about any of them. No coupling between content and distribution.

### Publishing a Pack

```
knomit publish bun-patterns
```

Steps:
1. Load `packs/bun-patterns.yaml`
2. Resolve include/exclude rules against the knowledge base
3. Copy matching facts to the target repo
4. Remove facts from the target that no longer match
5. Commit and push

Multiple packs can target different repos, or the same repo with different branches/directories.

### Relationship to `visibility`

The `visibility` field still exists as a hard safety boundary:

- `visibility: private` — **never** published, regardless of what any pack says. This is the override. If a pack's include rules match a private fact, it's silently excluded.
- `visibility: shared` — eligible for publication. Packs select from the shared pool.
- Omitted — treated as private (safe default).

So the flow is:
1. A fact must be `visibility: shared` to be publishable at all
2. A pack selects from the shared facts using its include/exclude rules
3. `knomit publish <pack>` projects the result to the target repo

This gives two layers: the fact author controls *whether* something can leave the repo. The pack author controls *where* it goes.

### What a Pack Looks Like as a Published Repo

Same as before — a normal knomit repo. The receiver doesn't know it came from a pack. They clone it, their agent learns from it. The pack mechanism is invisible to consumers.

```
knomit-bun-patterns/           ← target repo
  know/
    programming/
      bun-html-imports.md
      bun-sqlite-macos.md
      bun-test-patterns.md
    javascript/
      module-resolution.md
```

### Pack as the Unit of Sharing

This reframes sharing. You don't "make facts public." You curate packs — themed, intentional bundles of knowledge — and publish them. Each pack has a name, a purpose, and a target. This is closer to how people actually share knowledge: "here's my security playbook" not "here are all 47 facts I've marked as shared."
