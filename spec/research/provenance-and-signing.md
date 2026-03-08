# Research: Provenance and Signing

## Problem

Knomit currently tracks authorship via `git log --author`, which is a self-declared string. This works for a single user on a local machine. But when facts flow between repos — through knowledge packs, cross-repo discovery, or collaborative learning — there's no way to verify:

1. **Who created this fact?** (authenticity)
2. **Has it been tampered with since?** (integrity)
3. **Should I trust facts from this source?** (trust)

Git's author field is trivially spoofable. Anyone can commit as anyone. For a knowledge system that influences agent decisions, this is a real gap.

## What Git Signing Provides

Git natively supports commit signing via GPG keys or SSH keys (`git commit -S`). A signed commit proves:

- The commit was made by someone holding a specific private key
- The content has not been modified since signing
- The signature can be verified against a known public key

This maps directly to the provenance problem:

| Question | Mechanism |
|----------|-----------|
| Who created this fact? | Signing key identity (GPG uid or SSH key fingerprint) |
| Has it been tampered with? | Signature verification (`git verify-commit`) |
| Do I trust this source? | A trust policy mapping keys to trust levels |

## How It Would Work in Knomit

### 1. Agents Sign Their Commits

Each agent (human or AI) has a signing key. When learning a fact:

```
git commit -S -m "learn: SQL injection prevention"
```

The commit is now cryptographically bound to the key holder. The `git log --author` field becomes redundant for identity — the signature is the proof.

### 2. Verification on Ingest

When importing facts from another repo (cross-repo discovery, knowledge pack consumption), the receiving agent verifies signatures:

```
git verify-commit <hash>
```

If the commit is unsigned or the signature doesn't match a known key, the fact can be:
- Rejected outright
- Accepted with reduced confidence
- Flagged for human review

### 3. Trust Policy

A `trust.yaml` file (or similar) maps signing keys to trust levels:

```yaml
# trust.yaml
keys:
  - fingerprint: "SHA256:abc123..."
    name: "alice"
    trust: full          # facts from alice are accepted at stated confidence
  - fingerprint: "SHA256:def456..."
    name: "bob"
    trust: partial       # facts from bob get confidence * 0.7
  - fingerprint: "SHA256:ghi789..."
    name: "unknown-contributor"
    trust: review        # facts require manual review before merge

default: reject          # unsigned or unknown keys are rejected
```

This is a local policy — each knomit instance decides who it trusts. Alice might trust Bob fully while Carol trusts him partially. The trust policy is per-repo, not global.

### 4. Confidence Modulation

Trust level modulates how imported facts are treated:

| Trust Level | Effect |
|-------------|--------|
| `full` | Accept fact at its stated confidence |
| `partial` | Accept but multiply confidence by a discount factor (e.g., 0.7) |
| `review` | Stage the fact for human/librarian review before merging |
| `reject` | Silently ignore |

A fact with `confidence: 0.9` from a `partial` trust source becomes `confidence: 0.63` in the receiving repo. The trust discount is applied once at import time, not stored — the imported fact records the adjusted confidence.

## GPG vs SSH Keys

Git supports both. For knomit:

**SSH keys** are the better fit:
- Most developers already have them (for GitHub, etc.)
- Simpler key management than GPG
- GitHub already supports SSH commit signing (since 2022)
- No web-of-trust complexity
- `git config gpg.format ssh` + `git config user.signingkey ~/.ssh/id_ed25519.pub`

**GPG** has richer identity metadata (name, email, expiry) but the complexity isn't justified here. The signing key is an opaque identifier — the trust policy provides the semantic mapping.

## Agent Identity

AI agents present a unique challenge. Who holds the key?

Options:
- **Per-machine key**: The machine the agent runs on has a key. Proves "this fact came from machine X" but not which agent or model produced it.
- **Per-agent key**: Each agent instance (e.g., "alice's claude on laptop") has its own key. More granular but more keys to manage.
- **Per-user key**: The human who owns the agent signs on behalf of all their agents. Simpler, and the human is ultimately accountable.

For knomit, **per-user key** is the pragmatic choice. The human owns the knowledge base. Their agents act on their behalf. The signing key says "this knowledge base belongs to this person" — not "this particular AI model produced this fact."

The current hardcoded identity in `src/git.ts` (`user.email: knomit@local`, `user.name: knomit`) would be replaced with the user's actual identity and signing key at init time.

## What Changes in the Spec

### Fact Schema

No changes. Provenance is in the git layer, not the file. This is consistent with the existing design principle: "Git handles identity, lineage, timestamps, and versioning — the file itself carries only what Git cannot infer."

### Learning Lifecycle

Step 2 changes from `git commit` to `git commit -S`:

```
1. Agent creates branch:      learn/sql-injection-2025
2. Agent commits signed fact:  git commit -S -m "learn: SQL injection prevention"
3. Agent opens PR
4. Review & merge (merge commit is also signed)
5. Tag learning moment
```

### Cross-Repo Discovery

The cross-repo discovery flow gains a verification step:

```
1. Clone/fetch external repo
2. For each candidate fact:
   a. Verify commit signature (git verify-commit)
   b. Look up signing key in trust.yaml
   c. Apply trust level (reject / discount confidence / accept / flag for review)
3. Import accepted facts with adjusted confidence
4. Record provenance in refs (git://external-repo#hash)
```

### Knowledge Pack Publishing

Published packs are signed by the publisher. Consumers verify the pack repo's commits against the publisher's known key. This creates an end-to-end chain:

```
Author signs fact → Pack publisher signs export → Consumer verifies both
```

If the pack publisher is the same as the fact author (typical case), one signature suffices. If a pack aggregates facts from multiple authors, each original commit retains its signature, and the pack's merge/copy commits are signed by the publisher.

## What This Does NOT Solve

- **Content truthfulness**: A signed fact proves who said it, not whether it's correct. A fully trusted source can still be wrong. This is what `confidence` and `sources` are for.
- **Key revocation**: If a key is compromised, all facts signed with it are suspect. Git doesn't have a native revocation mechanism. The trust policy would need to be updated manually.
- **Anonymity**: Signing inherently links facts to an identity. If you want to share knowledge anonymously, you'd use an unsigned pack (and consumers would apply their `default` trust policy, likely `review` or `reject`).

## Recommendation

1. **Use SSH signing** — simpler than GPG, already widely adopted
2. **Sign all commits** — `git config commit.gpgsign true` at repo init
3. **Add `trust.yaml`** — local policy mapping keys to trust levels
4. **Verify on import** — cross-repo discovery and pack consumption verify signatures
5. **Modulate confidence** — trust level adjusts imported fact confidence
6. **Per-user keys** — the human owns the key, their agents use it

This keeps provenance in the git layer (no schema changes) and adds cryptographic guarantees where `git log --author` only provides self-declaration. The trust policy is local and subjective — exactly like trust in the real world.
