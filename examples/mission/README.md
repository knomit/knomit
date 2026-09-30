# Mission repo template

A mission repo is where the agents of one mission coordinate: tasks, claims,
working copies and acknowledgements. It is a knomit repo of its own, separate
from any knowledge base. It holds signals (`kind: pragmatic`, `type: signal`),
facts that are consumed once and never believed. The knowledge an agent
produces goes to a knowledge base; the mission repo only points at it.

Nothing here is compiled into knomit. The template is built from knomit's
generic primitives only: an ontology with validations and triggers, trigger
scripts (inline `js:` and files), repo skills, and a recipe. Copy it, then edit
it. A test in knomit (`internal/repos/mission_*_test.go`) runs these exact
files on two instances, so they do not rot.

| File | What it does |
|---|---|
| `.knomit/ontology.yaml` | Topics, validations (signals only), the repo settings (`consensus`, `conflicts`) and every trigger. |
| `.knomit/triggers/claims.js` | Pattern 1, claim / wait / take: `offer`, `decide`, `dup-check`. |
| `.knomit/triggers/awards.js` | Pattern 2, host-awarded: `bid`, `award`, `take-award`. |
| `.knomit/skills/post-task/SKILL.md` | How a session posts a task. |
| `.knomit/skills/work-task/SKILL.md` | How a session works a task and acknowledges it. |
| `.knomit/recipes/work-task.js` | The sample recipe the `wake` trigger runs: one headless Claude Code session per task. |

## Copy it

```sh
git init my-mission
cp -R examples/mission/. my-mission/   # the trailing /. copies .knomit/ too
git -C my-mission add -A
git -C my-mission commit -m "mission template"
```

(`cp -r examples/mission/* …` would skip `.knomit/`, which is the whole template.)

A repo's ontology is read when knomit creates the repo, so create the knomit
repo FROM this git repository rather than adding the ontology later.

### Knomit-hosted (the default in this template)

One instance hosts the repo; the others are its peers.

1. On the hosting instance, create the repo by cloning the git repository you
   just made, then remove its origin (repo settings, or
   `DELETE /api/v1/repos/<repo>/origin`). With no origin, this instance owns
   the repo's consensus branch. The repo keeps the branch it was cloned on,
   whatever its name. Then restart the hosting instance: the loop that moves
   an origin-less repo's consensus branch forward starts when the repo opens.
2. Enroll the other instances (fleet certificates, `push:own`) and let each
   clone the repo from the host. A peer pushes only its own agent branch.
3. `consensus: auto` (already in the ontology) makes the host merge every
   branch a peer pushes, as soon as the push lands, into its own agent branch;
   its consensus branch follows within about a second. Peers pick it up at
   their next sync.

### GitHub-hosted (or GitLab, or any forge)

Push the repository to the forge and let every instance clone it. Delete the
`consensus: auto` line: the forge owns the consensus branch, and something
there merges the agents' branches into it (for example the knomit-kb
`merge-agent-branches.yml` workflow). Keep `conflicts` exactly as it is: every
instance's own sync still settles conflicts with it.

```yaml
attributes:
  conflicts:
    facts: merge
    state: consensus
```

## Set it once and forget

```yaml
attributes:
  consensus: auto
  conflicts:
    facts: merge
    state: consensus
```

Set these when the mission repo is created, and do not change them
mid-mission. Every instance reads them at the tip of the repo's consensus
branch, and an instance that has not synced yet uses the old values for one
round. Nothing reconciles a change made mid-flight.

Under `consensus: auto`, these `conflicts` values are also what an absent
`conflicts` means. The template writes them out so that the rule is visible.

## Conflicts: merged and recorded, never stalled

Two instances can change the same path between two syncs, for example two
edits of one task. Such a conflict does not stop anything. It is settled by
rule at whichever merge meets it (the host's merge of a pushed branch, or a
peer's own sync), and it is recorded in that merge commit:

- **A fact both sides changed** (`facts: merge`): the two versions are merged
  field by field. A field only one side changed takes that side's value; a
  field both changed takes the more confident side's. Lists (entities, refs)
  are united. The commit names it: `Knomit-Merge: <path> strategy=merge …`.
- **A retraction against an edit**: the retraction wins, under `merge` too.
- **Anything that is not a fact** (`.knomit/` files, the ontology, skills,
  recipes) **and any fact that cannot be merged without loss**
  (`state: consensus`): the consensus side's version is kept whole. The
  commit names it: `Knomit-Conflict: <path> … strategy=consensus`.

The other values and what "the consensus side" is in each topology:
`.claude/future/fleet/proposals/F20-conflict-merge.md` in the knomit source
repository. Its cautions apply here: `off` under `auto` races, `auto` is
ignored on a repo with an origin, and `merge` trusts confidence.

Good practice, not a correctness rule: **do not edit a posted task. Retract
it and post a new one.** A task may already be claimed, and a new id keeps
the history easy to read. An edit is safe; it is merged and recorded, but it
is harder to follow.

## Two ways to take a task

A task that is assigned to one agent is posted straight into that agent's
queue, `inbox/<agent-id>/working/`. It is never claimed, and it wakes that
agent only. Generally available work needs a way to decide who takes it. The
template ships both patterns below; no knomit code picks one. Keep the topics
of the one you use and delete the other's.

| | Pattern 1: claim / wait / take | Pattern 2: host-awarded |
|---|---|---|
| Post to | `tasks/<lane>/` | `offers/<awarder-agent-id>/<lane>/` |
| Who decides | every claimer, from what it has fetched | the awarder, alone |
| Guarantee | a heuristic plus a backstop: a double take is possible and is undone afterwards | exactly one award per offer |
| Latency | one claim window (X, below) | the bids' round trip to the awarder and the award's back |
| Needs | nothing beyond the participants | the awarder to be up (normally the hosting instance) |

### Pattern 1: claim / wait / take

1. **Claim.** When a task arrives, every machine with capacity writes
   `claims/<task-id>/<agent-id>/` with `expires` = now + X, and pushes it
   (`push-claim`).
2. **Wait.** The claim's own `expires` is the timer: at it, `decide` runs on
   the claimer's machine only (`{agent}` in its `match`).
3. **Decide.** If the task is gone or already being worked, withdraw the
   claim. Otherwise rank every live claim by FNV-1a of `task|agent-id`, with
   ties broken by the agent id. Every machine computes the same order.
4. **Take.** The first-ranked claimer takes with ONE atomic move: it writes
   its working copy, and deletes the task and its own claims, in one commit.
   Either all of it lands or none of it does.
5. **Lose.** Everyone else re-arms its claim for one more window. At the next
   decide it sees the take and withdraws. If the winner never takes (it
   crashed), its claim ages more than 2X past its `expires`, stops ranking,
   and the next claimer takes instead: the re-offer. That take also deletes
   the dead claims.
6. **Backstop.** If two working copies of one task ever meet (a claim was
   seen late), `dup-check` makes the lower-ranked holder delete its copy.

**The honest limit.** This is a heuristic plus a backstop, never exclusion.
Each machine decides at its own due, from its own fetched state. "Every machine
picks the same winner" holds only when every machine holds the same claims. A
claim seen late (X too short, a partitioned peer, a slow host) can produce a
double take; the backstop undoes it after both working copies have merged, and
the work may have started twice. Directly assigned tasks never race.

### Pattern 2: host-awarded, exactly once

1. **Offer.** Post under `offers/<awarder-agent-id>/<lane>/`. The awarder is
   normally the instance that hosts the repo.
2. **Bid.** Every machine with capacity writes
   `bids/<awarder>/<task-id>/<agent-id>/`.
3. **Award.** `award` matches `bids/{agent}/**`, so it runs on the awarder
   only. Each award is the atomic move "write `awards/<winner>/…`, delete the
   offer". A second award for the same offer finds the offer gone and is
   refused as a whole, under the awarder's own branch lock. One writer and one
   precondition make it exactly once.
4. **Take.** The winner moves the award into `inbox/<agent-id>/working/`,
   deleting its bid in the same commit. A losing bid is withdrawn when it
   expires (`withdraw-bid`, an inline script).

## The timing rule

**X ≥ the longest sync interval of any participant + the push countdown
(1 s) + the host's consensus-branch fast-forward (≈1 s after a merge) + one
due tick.**

- **Why the interval:** participants see a new task at their own fetch, so
  their claims start up to one interval apart. A claim made one interval after
  mine must reach the consensus branch before my decide.
- **Why the due tick costs seconds, not an interval:** the due sweep rides the
  claimer's own sync tick, and that tick fetches and merges BEFORE it sweeps.
  A decide therefore sees everything the consensus branch held when its tick
  fetched.
- **The dead-winner threshold (2X) must exceed the winner's own due latency.**
  That latency is up to one sync interval after its claim's `expires`, plus one
  more interval if its decide was dropped by the script rate cap and retried.
  With the rule above, X is more than one interval, so 2X is more than two
  intervals and the constraint holds. A participant with a longer interval
  must raise X for everyone. Otherwise a live but slow winner is presumed dead,
  and the re-offer itself causes the double take.
- **Worked numbers.** A knomit peer syncs with its origin every 300 s (the
  interval is not configurable today). So X ≥ 300 + 1 + 1 + a few seconds;
  the template sets **X = 360 s** (`WINDOW_SECONDS` in `claims.js`), and 2X =
  720 s covers a winner that is up to two intervals late. The hosting
  instance's own ticks follow its local reconcile interval (30 s by default).
  On a forge, add the time its merge of an agent branch takes.
- The losers' re-arm also tolerates an X that was too short: a claim seen late
  is ranked at the next decide.

The `push-*` triggers shorten the common case (each write goes out within about
a second), but only a fetch brings the others' claims in. X must still follow
the rule.

## Waking a session

`wake` runs the recipe `work-task` (`.knomit/recipes/work-task.js`) on the
machine a working copy belongs to. That covers tasks it took and tasks
assigned to it. Recipes are read from the tip of the repo's consensus branch
(a `<home>/recipes/work-task.js` on that machine is the fallback), so a
recipe change takes effect once it is merged there.

The sample starts `claude -p` with this repo's MCP server and a prompt that
names the working copy by PATH and says: call the `knomit_skill` tool with
name `work-task` and follow it. The task's text never enters argv or the
prompt; the session reads it through knomit. Change the argv for another
harness, and keep the task out of it.

## Skills

`.knomit/skills/<name>/SKILL.md` are served by knomit's MCP server from the
tip of the consensus branch: as MCP prompts (slash commands), and through the
`knomit_skill` tool a model can call itself. `knomit_skill` with no name lists
them. Nothing is installed into any harness.

- `post-task`: how to post a task, an offer, or an assigned task.
- `work-task`: read the working copy, check it is still yours, do the work,
  write the results to the knowledge base, then acknowledge with one move
  (write `acks/<task-id>/`, delete the working copy).

## Editing the template

- A trigger that references a script (`script: claims`) reads
  `.knomit/triggers/claims.js` from the agent branch's head. An inline trigger
  (`js:`) carries its code in the ontology. A trigger has exactly one of them.
- Match patterns are relative to the ontology root and may use `{agent}`, which
  is this instance's agent id (its agent branch without the `agent/` prefix).
- A trigger that first appears starts at the head it appears at: a machine
  claims tasks posted after it joined the mission, not the ones already there.
- Keep `learn_dedup: off` on every signal topic. Signals are near-identical
  by design (templated text, different ids). With dedup on, a second task in a
  lane is refused, and a second working copy is folded into the first.
- Expiry is a trigger script (`expire`: `on: due` + `knomit.retract`). knomit
  itself never acts on `expires`.
