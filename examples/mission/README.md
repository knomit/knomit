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
| `.knomit/ontology.yaml` | Topics, validations (signals only), the repo settings (`consensus`, `conflicts`, `sync`) and every trigger. |
| `.knomit/triggers/claims.js` | Pattern 1, claim / wait / take: `offer`, `decide`, `dup-check`. |
| `.knomit/triggers/awards.js` | Pattern 2, host-awarded: `bid`, `award`, `take-award`. |
| `.knomit/skills/post-task/SKILL.md` | How a session posts a task. |
| `.knomit/skills/work-task/SKILL.md` | How a session drains its queue: take each copy, work it in a knowledge-base experiment, acknowledge it. |
| `.knomit/recipes/work-task.js` | The sample recipe the `wake` and `lease` triggers run: one headless Claude Code session that drains the queue. |

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
   whatever its name, and the loop that moves an origin-less repo's consensus
   branch forward starts as soon as the origin is removed.
2. Enroll the other instances (fleet certificates, `push:own`) and let each
   clone the repo from the host. A peer pushes only its own agent branch.
3. `consensus: auto` (already in the ontology) makes the host merge every
   branch a peer pushes, as soon as the push lands, into its own agent branch;
   its consensus branch follows within about a second. Peers pick it up at
   their next sync round.
4. `sync: {push: realtime, pull: realtime}` (also in the ontology) makes
   that round frequent. Every commit on an instance's own agent branch goes
   out about 1 s after it lands, and every instance runs a sync round every
   `[git].realtime_pull_interval` (`knomit.toml`, 3 s by default) instead of
   every 300 s. On the host, which has no origin, that round is its local
   round: it fast-forwards the consensus branch and runs the `on: due` sweep.
   A host with `[git].local_reconcile_interval = 0` runs no local rounds at
   all, so nothing there fires a decide or moves the consensus branch on a
   timer; keep it above 0.

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
  sync:
    push: realtime
    pull: realtime
```

**`sync` against GitHub costs requests.** GitHub's documented recommendations
(docs.github.com, "Repository limits") are at most **15 git reads per second
per repository** and at most **6 pushes per minute per repository**. GitHub
publishes no hard limit or abuse threshold for git over HTTPS or SSH, and
knomit has not tested where one lies.

- `pull: realtime` at the 3 s default is one fetch every 3 s per
  participant: about **1,200 fetches an hour** each, idle or not. About 45
  participants on one repository reach the read recommendation on their own.
- `push: realtime` pushes about 1 s after each commit. A participant that
  writes steadily can push up to about 30 times a minute, five times the
  push recommendation from one machine. A mission's writes come in bursts
  (a claim, a take, an ack), but a busy fleet adds them up.
- knomit does not detect a GitHub origin and does not refuse it. Real-time
  sync is meant for knomit-hosted missions. On GitHub, keep it to a small
  fleet; dropping `sync` puts every write back on the 300 s round, and X
  must then follow the rule below with that interval.

On a forge, the forge's own merge of an agent branch into the consensus
branch is part of a claim's travel, and real-time pull does not shorten it.
Add the time that merge takes (a workflow run is often a minute or more) to
X; the 15 s in the template is for a knomit-hosted mission.

## Set it once and forget

```yaml
attributes:
  consensus: auto
  conflicts:
    facts: merge
    state: consensus
  sync:
    push: realtime
    pull: realtime
```

Set these when the mission repo is created, and do not change them
mid-mission. Every instance reads them at the tip of the repo's consensus
branch, and an instance that has not synced yet uses the old values for one
round. Nothing reconciles a change made mid-flight.

Under `consensus: auto`, these `conflicts` values are also what an absent
`conflicts` means. The template writes them out so that the rule is visible.

`sync` needs every participant to run a knomit that knows it (F21, 2026-10).
An older knomit warns once about the unknown key and keeps syncing every
300 s, so its claims arrive minutes late and the 15 s window does not hold
for it: see "The timing rule".

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
queue, `inbox/<agent-id>/working/`, with a lease in `expires` (see "The
queue"). It is never claimed, and it wakes that agent only. Generally available work needs a way to decide who takes it. The
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
   `claims/<task-id>/<agent-id>/` with `expires` = now + X. Realtime push
   sends it out about 1 s later.
2. **Wait.** The claim's own `expires` is the timer: at it, `decide` runs on
   the claimer's machine only (`{agent}` in its `match`).
3. **Decide.** If the task is gone or already being worked, withdraw the
   claim. Otherwise rank every live claim by FNV-1a of `task|agent-id`, with
   ties broken by the agent id. Every machine computes the same order.
4. **Take.** The first-ranked claimer takes with ONE atomic move: it writes
   its working copy (with its lease), and deletes the task and its own
   claims, in one commit. Either all of it lands or none of it does. Before
   the move it counts its backlog again: if it filled up since the claim
   (assigned work, another take), it withdraws its claim instead, and the
   next claimer by rank takes at its next decide.
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
4. **Take.** The winner moves the award into `inbox/<agent-id>/working/`
   (with its lease), deleting its bid in the same commit. It takes it even
   over capacity: the award already deleted the offer, so refusing it would
   lose the task, and the extra copy just waits in the queue. A losing bid is
   withdrawn when it expires (`withdraw-bid`, an inline script).

## The timing rule

**X ≥ the LARGEST sync round interval of any participant + the push
countdown (1 s) + the host's merge and consensus-branch fast-forward (≈1 s) +
one due tick, plus a margin.**

- **Why the interval:** participants see a new task at their own round, so
  their claims start up to one interval apart. A claim made one interval after
  mine must reach the consensus branch before my decide. It is the largest
  interval of ANY participant that counts: one slow peer sets X for everyone.
- **Why the push countdown and the fast-forward:** under `push: realtime` a
  claim goes out about 1 s after it is written. The host merges a pushed
  branch as soon as the push lands, and that merge wakes the host's own round
  with the same 1 s countdown, which fast-forwards the consensus branch.
- **Why the due tick costs one interval:** the due sweep rides the claimer's
  own sync round, and that round fetches and merges BEFORE it sweeps. A
  decide therefore sees everything the consensus branch held when its round
  fetched, and runs at most one round after the claim's `expires`.
- **The dead-winner threshold (2X) must exceed the winner's own due latency.**
  That latency is up to one interval after its claim's `expires`, plus one
  more interval if its decide was dropped by the script rate cap and retried.
  A participant with a longer interval must raise X for everyone. Otherwise a
  live but slow winner is presumed dead, and the re-offer itself causes the
  double take.

**Worked numbers** (knomit-hosted, `sync: {push: realtime, pull: realtime}`,
the default `[git].realtime_pull_interval` of 3 s on every participant):

| Term | Time |
|---|---|
| the largest pull interval (N) | 3 s |
| the push countdown | 1 s |
| the host's merge, then its fast-forward after the same countdown | ≈1 s |
| one due tick (one round, every N) | ≤ 3 s |
| round times on a healthy origin | tens of ms |
| **floor** | **≈ 8 s** |

The template sets **X = 15 s** (`WINDOW_SECONDS` in `claims.js`), almost 2× the
floor. 2X = 30 s against a winner's due latency of 2N = 6 s. A test
(`TestMissionTemplate_TimingRule`) computes the floor from knomit's own
constants and fails if X drops below 1.5× it.

When the inputs change, X must change with them:
- **A participant with a longer `realtime_pull_interval`** raises N for
  everyone: X ≥ 1.5 × (2N + 2 s).
- **A peer running a knomit older than F21** ignores `sync` and syncs every
  300 s. Either upgrade it or raise X to the rule with N = 300 s (X ≥ about
  360 s, the template's old value).
- **A host with `[git].local_reconcile_interval = 0`** runs no local rounds:
  no due sweep on a timer and no consensus fast-forward after its own writes.
  The rule does not hold there; keep the interval above 0.
- **On a forge**, add the time its merge of an agent branch takes (see
  "GitHub-hosted").

**This holds only while every participant's origin is healthy.** Against a
failing origin, one round can take up to 240 s (2 × the 120 s network
timeout), and after three failures in a row the circuit breaker stops that
origin from being hammered: it skips the step for 2N (6 s), then doubles on
every failed probe, up to 30 minutes. Both are far above 2X. A participant
whose origin is failing sees claims late and sends its own late, so a slow
winner can be presumed dead and two peers can take the same task. The
template's backstops cover that case, not the timing rule:
- **the losers' re-arm:** a loser re-arms its claim each window, so a claim
  seen late is ranked at the next decide;
- **the dup-check:** when two working copies of one task meet, the
  lower-ranked holder deletes its copy. The work may have started twice.

There is no `do: push` trigger in the template: `push: realtime` already
sends every commit on an instance's own agent branch, and only a round's
fetch brings the others' claims in.

## The queue

`inbox/<agent-id>/` is a queue, and a session consumes it.

- **`working/`** holds the copies this machine took or was assigned and no
  session has started yet.
- **`active/`** holds the copies a session has taken. A session takes a copy
  with ONE move: it writes the copy into `active/` and deletes it from
  `working/`, in one commit. A second session making the same move is
  refused, because the copy it would delete is already gone, so two sessions
  never work one copy.
- **The ack** is the last move: write `acks/<task-id>/` and delete the
  `active/` copy, in one commit. A failed task is acknowledged too ("Failed:
  …"), so the queue always moves on.

**One session drains the queue.** `wake` starts a session when a copy lands
in `working/`. The `work-task` skill then takes EVERY copy in `working/`, one
at a time, and stops only when a query of `working/` comes back empty after
its last ack. The copy that woke it is only where it starts. So a wake that
is dropped because a session is already running (a recipe at its
`concurrent` limit drops the fire with a `busy` row; there is no queue in
knomit) loses nothing while that session runs.

**The lease is the safety net.** Every copy in the inbox carries `expires`
(the ontology refuses one without it). It is a LEASE, never a deadline:
nothing retracts a copy because its lease ran out. The `lease` trigger (`on:
due`, `match: inbox/{agent}/**`) runs the same recipe when a copy is still
there at its lease:

- A copy that landed after the running session's last look, and whose wake
  was dropped as `busy`: its lease runs out, and a session starts for it. A
  due fire dropped as `busy` is retried at every sweep until the slot is
  free (a learn fire is not).
- A copy whose session died (killed at its timeout, a crash): the copy's
  lease runs out, and a session starts again for it.

The leases:
- **A copy in `working/`**: `LEASE_SECONDS` (300 s) from its take, in both
  scripts. An assigned copy gets the lease its poster sets (the `post-task`
  skill says about 5 minutes).
- **When a session starts**, the recipe pushes the lease of the copy that
  woke it out to the session's end (the recipe's `timeout_ms` plus a
  minute), and a session writes that same lease on every copy it takes
  into `active/`. A due fire that started a session counts as processed, so
  this is what makes the copy fire again if that session dies.
- **An acknowledged copy** is gone, so it never fires.

The tradeoff is the length of the working lease. Shorter picks up a dropped
copy sooner. But while a session runs, every copy waiting past its lease
logs a `busy` row on every sweep (each tick and each write) until the
session takes it, and the fire log keeps 10,000 rows per repo. 300 s is a
few minutes' latency in the rare case, for little log churn in the common
one.

## Capacity and parallelism

- **`CAPACITY`** (`claims.js`, `awards.js`) is the BACKLOG: how many copies,
  `working/` and `active/` together, a machine holds before it stops
  claiming and bidding. `decide` checks it again right before it takes.
- **`concurrent`** (the recipe's header) is the PARALLELISM: how many
  sessions run at once on a machine. Each session drains the same queue;
  the take's move keeps them off each other's copies, so raising it above 1
  is safe. The template runs 1.
- The recipe's `timeout_ms` bounds ONE session, and a session drains the
  whole queue: keep it above the time a full backlog takes, or let the lease
  restart the rest.

## The session

`.knomit/recipes/work-task.js` starts one headless Claude Code session:

- **In a fresh, empty temporary folder** (`mktemp -d`, the `cwd` of the
  claude call), removed afterwards. Nothing of this machine's working tree
  is in reach. `mktemp` and `rm` must be on the PATH knomit runs with; on
  Windows, run knomit from a shell that has them (Git Bash), or edit the
  recipe.
- **With ONE MCP server, unbound**: knomit's bridge `kb` with no `--repo` and
  no `--lens`, built by the recipe. `kb` reaches THIS knomit through the
  `KNOMIT_SERVER` knomit puts in the recipe's environment (its own local
  socket, or its TCP address when the socket is not its own); Claude Code
  passes that environment on to `kb`. `KNOMIT_HOME` alone would not do it:
  `kb` does not choose its server by home, so with several instances on one
  machine it would reach whichever one the desktop lockfile names.
  `--strict-mcp-config` keeps the user's own MCP servers out.
- **Allowed tools**: that server and `WebFetch`, `WebSearch`. Nothing else.
- **A budget**: `--max-budget-usd` from `MAX_BUDGET_USD` in the recipe. Set
  it for your mission before the first run.

The prompt says: bind the mission repo, call the `knomit_skill` tool with
name `work-task`, follow it. It carries one JSON literal of data: the mission
repo's name (read from the args of knomit's own MCP entry for this repo, so
a copy of the template under any name works), the copy's path, its
experiment name, the session's lease and the trace. The task's text never
enters argv or the prompt; the session reads it through knomit.

**Two handles.** The session calls `knomit_bind` twice: once for the mission
repo, once for the knowledge base the task names in its body (`knowledge
base: repo <name>` or `knowledge base: lens <name>`). It passes the mission
handle for copies and acks, and the knowledge-base handle for results.
**The limit:** an unbound session can bind any repo or lens this knomit
serves. The skill binds only what the task names; a hard limit is to serve
only what the mission may touch from the knomit that runs the sessions.

**One experiment per task.** The session opens a `knomit_experiment` on the
knowledge-base handle, named after the task id in strict kebab-case
(`Task_42.b` → `task-42-b`), does the work there, and commits it before the
ack, or rolls it back and acknowledges the task as failed. A task's results
land on the knowledge base whole or not at all. `open` on a name that
already exists RESUMES it: a task retried after its session died continues
that session's half-done experiment.

## The trace

The prompt hands the session a `trace`: `Knomit-Cause` (the commit that
fired), `Knomit-Run` (this run's id) and `Knomit-Trace` (the task id,
included only when it is a plain id). The `work-task` skill tells the
session to pass it as the `trace` argument on every knomit write for that
copy (the take, the results, the experiment's `open`, `commit` and
`rollback`, the ack), so a task's story (the trigger
scripts' writes, the session's results and the ack) reads back with
`git log --all --grep='^Knomit-Trace: <task id>'` (or
`--grep='^Knomit-Run: <run id>'` for one run). For every other copy it takes,
the session builds the trace itself: `Knomit-Trace` = that copy's task id,
the same `Knomit-Run`, and no `Knomit-Cause`.

`knomit_experiment` takes the same `trace` and stamps it on the merge commit
its `commit` (or `sync`) writes. A `commit` that fast-forwards writes no new
commit, so there is nothing new to stamp: the fact commits it lands already
carry the trace their writes were given. `open` and `rollback` make no
commit; the trace is still validated there, so pass it anyway.

**Known gap:** a write the session makes without the trace is untraced.

## Re-offering

knomit never re-offers a task. A task that expired with nobody taking it,
or whose ack says "Failed:", is the coordinator's decision. To offer it
again, the coordinator posts it again under a NEW task id, in one
`knomit_learn` that also retracts the old task if it is still there. No
script is needed.

## The knowledge base

The mission repo holds signals; what the agents learn goes to a knowledge
base the tasks name (`knowledge base: repo <name>` or `lens <name>`).

### The hypothesis format

Every task that asks for predictions carries the hypothesis format in its
body (the `post-task` skill has the block to paste, marked `hypothesis-format`).
A session reads only its task and its skill, so a format kept anywhere else is
never seen. The format is the same for every mission; the mission's charter
supplies the subject, the granularities that matter and the instruments that
settle a prediction.

- `topic: forecast`, `category: <subject>/<granularity>`, the granularity
  being `year`, `month` or `day`;
- `type: hypothesis`, `confidence` = the probability;
- the body starts with three lines, unindented:
  - `predicted: <period>`: `YYYY`, `YYYY-MM` or `YYYY-MM-DD`, at the granularity;
  - `settles_true_if: <instrument>` and `settles_false_if: <instrument>`, each
    naming an instrument that already exists and can be observed;
  - plus `counters: <path>` for a counter-hypothesis;
- `expires` = the last second of the predicted period in UTC (31 December, the
  last day of the month, or the day itself, at `23:59:59Z`);
- `refs`: the evidence.

Nobody retracts a hypothesis because it settled: people decide that.

## Skills

`.knomit/skills/<name>/SKILL.md` are served by knomit's MCP server from the
tip of the consensus branch: as MCP prompts (slash commands), and through the
`knomit_skill` tool a model can call itself. `knomit_skill` with no name lists
them. Nothing is installed into any harness.

- `post-task`: how to post a task, an offer, or an assigned task.
- `work-task`: drain the queue. For each copy: take it (one move into
  `active/`), do the work in a knowledge-base experiment, commit it, then
  acknowledge with one move (write `acks/<task-id>/`, delete the `active/`
  copy). Stop when `working/` is empty.

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
