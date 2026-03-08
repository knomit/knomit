# Research: Cross-Repo Knowledge Discovery

## Problem

Someone else runs their own knomit repo. Their agents have learned facts I might find valuable. I want to point an LLM at their repo and say "see what that person knows that's worth knowing, and remember it."

This is not sync. Sync assumes shared ownership of the same knowledge base across machines. This is **discovery** — browsing a foreign knowledge base and selectively learning from it.

## Key Insight: No New Infrastructure

Bob's repo is already structured facts in files. There is no need for an export format, an API, or a protocol. The repo *is* the format. The agent already has `knomit_recall` (to check what it knows) and `knomit_learn` (to absorb new facts). The only missing piece is read access to Bob's files — a git clone to a temp directory.

## Flow

```
User: "Go look at Bob's knomit and learn anything useful about security"

Agent:
  1. git clone https://github.com/bob/knomit /tmp/bob-knowledge
  2. Read files under /tmp/bob-knowledge/worlds/**
  3. For each fact:
     a. Do I already know this?  →  knomit_recall to check  →  skip if yes
     b. Is this relevant to the user's instruction?  →  LLM judges
     c. Does this contradict something I know?  →  flag or resolve
     d. Is this worth knowing?  →  knomit_learn
  4. Commit message captures the batch: "learned N facts from bob/knomit"
```

## Why Not Export?

An export tool (JSON dump, patch file, etc.) adds indirection without value. The repo is already:

- Structured (YAML frontmatter + markdown body)
- Browsable (files in directories following the ontology)
- Versioned (git history)
- Shareable (git clone)

An agent reading files in a cloned repo is equivalent to an agent reading a JSON export — except the repo also gives you commit history, authorship, and the ability to pull updates later.

## Ontology Mapping

Imported facts go into the canonical ontology path, not a namespaced import directory. A fact about SQL injection prevention is `worlds/security/sql-injection-prevention.md` regardless of who discovered it. "Who told me" is provenance metadata, not ontology structure.

The agent handles ontology differences naturally. If Bob organizes his facts differently (different directory structure, different entity naming), the LLM re-contextualizes when calling `knomit_learn` — the same way a human would translate someone else's framing into their own.

## Provenance

When the agent learns a fact from Bob's repo, the source is tracked through:

1. **Git commit message** — "learned from bob/knomit" on the commit that adds the fact
2. **refs field** — Can include `git://github.com/bob/knomit#abc123` pointing to the original commit
3. **Natural language** — The fact body itself might note "according to Bob's analysis..." where relevant

No special provenance machinery is needed. The existing `refs` field and git history are sufficient.

## Collision Handling

The LLM handles collisions through judgment, not rules:

| Situation | What the agent does |
|-----------|-------------------|
| Bob knows something I don't | Learn it if relevant |
| I already know it | Skip (knomit_recall confirms) |
| Bob's version has more detail | Update mine (knomit_update) |
| Bob's fact contradicts mine | Judge which is more credible, or keep both with lower confidence |
| Bob's fact is stale | Skip — my version is newer/better |

This is the same judgment an agent applies when learning from any source. Bob's repo is just another source.

## Confidence

Bob's `confidence: 0.85` reflects *his* evidence. You haven't seen that evidence. Options:

1. **Import at face value** — Let future reinforcements/contradictions adjust naturally. Simplest. Most knomit-like.
2. **Apply a discount** — `your_confidence = bob_confidence * 0.7`. Acknowledges secondhand knowledge.
3. **Start fresh** — Import at a baseline (e.g., 0.5) regardless of Bob's confidence. Treats it as a new claim.

Option 1 is the pragmatic default. The agent can always choose to learn with a lower confidence if it has reason to doubt.

## Convenience Command

The entire flow can be triggered with a single instruction to an agent. But a shorthand is nice:

```
knomit browse <repo-url> "<instruction>"
```

This is sugar for: clone the repo, hand it to an agent with read access to those files + write access to my knomit tools, with that instruction as the prompt.

## Ongoing Discovery

For one-time browsing, clone and discard. For ongoing awareness of what Bob learns:

```
knomit browse <repo-url> "<instruction>" --watch
```

Which periodically pulls Bob's repo and re-runs discovery on new commits since the last check. This is a cron job wrapping the same flow — no new architecture.

## Comparison to Multi-Machine Reconciliation

| | Multi-machine sync | Cross-repo discovery |
|-|-------------------|---------------------|
| Ownership | Same person, multiple machines | Different people |
| Direction | Bidirectional | One-way (pull) |
| Scope | Everything | Filtered by instruction |
| Ontology | Shared (same repo) | Potentially different |
| Merge strategy | Deterministic rules + LLM | LLM judgment only |
| Transport | Git push/pull on shared remote | Git clone of foreign repo |
| Filter | None — all facts merge | LLM decides what's worth knowing |

## What This Enables

- Browse a colleague's knomit to absorb their domain expertise
- Aggregate knowledge from multiple public knomit repos overnight
- "Subscribe" to someone's evolving knowledge on a topic
- Build a personal knowledge base that compounds from multiple sources, curated by an LLM that knows what *you* care about
