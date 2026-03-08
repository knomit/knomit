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

## Key Architecture: Per-Machine, Not Per-User

A single "master key" on every machine is a liability. Lose the laptop, lose the key. The solution: **each machine gets its own signing key**. All keys belonging to the same person are grouped in the trust policy.

### Setup

Each machine generates its own key at `knomit init`:

```bash
# On laptop
ssh-keygen -t ed25519 -f ~/.ssh/knomit_laptop -C "knomit/laptop"
git config user.signingkey ~/.ssh/knomit_laptop.pub

# On homelab
ssh-keygen -t ed25519 -f ~/.ssh/knomit_homelab -C "knomit/homelab"
git config user.signingkey ~/.ssh/knomit_homelab.pub
```

No key ever leaves the machine it was generated on.

### Trust Policy: GitHub as the Key Server

Manually managing fingerprints in a YAML file doesn't scale. GitHub already solves key distribution — every user's public SSH keys are available at `https://github.com/<username>.keys`, and GitHub verifies commits signed with those keys.

The trust policy references **GitHub identities**, not raw fingerprints:

```yaml
# trust.yaml
identities:
  - github: "myusername"
    trust: full            # all keys registered on my GitHub account

  - github: "bob"
    trust: partial         # bob's facts get confidence * 0.7

  - github: "alice"
    trust: full

default: reject            # unknown signers are rejected
```

#### How It Works

1. **Your keys**: You upload your per-machine SSH signing keys to GitHub (Settings → SSH and GPG keys → "Signing key"). Each machine has its own key, all registered under your one GitHub account.

2. **Verification**: Git's `allowed_signers` file maps email addresses to public keys. `knomit` generates this file automatically by fetching keys from GitHub:

```bash
# Fetch keys for all trusted identities
curl -s https://github.com/myusername.keys   # returns all your public keys
curl -s https://github.com/bob.keys          # returns bob's public keys
```

These get written to `.git/allowed_signers`:

```
myusername@github ssh-ed25519 AAAA...laptop_key
myusername@github ssh-ed25519 AAAA...homelab_key
bob@github ssh-ed25519 AAAA...bob_key
```

Then git verifies natively:

```bash
git config gpg.ssh.allowedSignersFile .git/allowed_signers
git verify-commit <hash>   # → "Good signature by myusername@github"
```

3. **Trust resolution**: The verified identity (GitHub username) is looked up in `trust.yaml` to get the trust level.

#### Key Lifecycle

| Action | Where |
|--------|-------|
| Generate per-machine key | Local: `ssh-keygen -t ed25519` |
| Register key for signing | GitHub: Settings → SSH keys → "Signing key" |
| Revoke compromised key | GitHub: Delete the key from your account |
| Refresh trusted keys | `knomit trust refresh` → re-fetches from GitHub |

**Revocation is just deleting the key from GitHub.** The next `knomit trust refresh` updates the allowed signers file. No manual fingerprint management. GitHub is the source of truth for "which keys belong to this person."

#### Why This Works

- **No manual key exchange**: You trust "github.com/bob", not a fingerprint you got over Signal
- **Per-machine keys, single identity**: Five machines, five keys, one GitHub account
- **Revocation via GitHub**: Delete the key from your account, done
- **GitHub already verifies**: Commits on GitHub show "Verified" badges — same infrastructure
- **Offline fallback**: The allowed_signers file is cached locally. Verification works offline. Only `trust refresh` needs network access
- **Git-native**: Uses `gpg.ssh.allowedSignersFile`, a standard git feature. No custom verification code

#### Limitations

- Requires GitHub accounts (or any forge that exposes public keys via URL — Gitea, GitLab, etc. all do)
- Key fetch is a network operation — needs periodic refresh
- A compromised GitHub account could add malicious signing keys. But this is the same threat model as any SSH-based workflow
- For fully offline / air-gapped setups, falls back to manual fingerprint management

### AI Agent Identity

The agent doesn't get its own key — it uses the machine's key. The signing says "this fact was committed on this machine" which transitively means "by the person who controls this machine."

This is the right level of granularity. You don't need to distinguish "Claude on laptop" from "Gemini on laptop" — you need to distinguish "my laptop" from "my homelab" from "a machine I don't control."

The current hardcoded identity in `src/git.ts` (`user.email: knomit@local`, `user.name: knomit`) would be replaced with the user's actual identity and the machine's signing key at init time.

### Multi-Machine Reconciliation

This integrates with the reconciliation architecture. When machine A pulls facts from machine B's branch:

1. Verify the commits are signed by a key belonging to "me"
2. Check the key isn't revoked
3. Merge with full trust (it's your own knowledge, from a machine you control)

If a revoked key is found, the reconciliation flags those commits for review rather than auto-merging. The blast radius of a compromised machine is contained to facts it authored after compromise, and those facts don't silently propagate.

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
- **Key revocation timing**: Revocation is manual. You need to notice a device is compromised and update `trust.yaml`. Facts signed between compromise and revocation are in a gray zone.
- **Anonymity**: Signing inherently links facts to an identity. If you want to share knowledge anonymously, you'd use an unsigned pack (and consumers would apply their `default` trust policy, likely `review` or `reject`).

## Recommendation

1. **Use SSH signing** — simpler than GPG, already widely adopted
2. **Sign all commits** — `git config commit.gpgsign true` at repo init
3. **Add `trust.yaml`** — local policy mapping keys to trust levels
4. **Verify on import** — cross-repo discovery and pack consumption verify signatures
5. **Modulate confidence** — trust level adjusts imported fact confidence
6. **Per-machine keys** — each machine gets its own key, grouped under a single identity in trust.yaml
7. **Independent revocation** — compromised keys are revoked surgically without affecting other machines

This keeps provenance in the git layer (no schema changes) and adds cryptographic guarantees where `git log --author` only provides self-declaration. The trust policy is local and subjective — exactly like trust in the real world. Per-machine keys ensure that losing a device doesn't compromise the entire identity.
