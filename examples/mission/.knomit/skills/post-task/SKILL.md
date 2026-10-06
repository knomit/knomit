---
name: post-task
description: Post a task to this mission repo, for anyone to take (claim/wait/take), for the awarder to hand out (host-awarded), or for one agent directly. Use when asked to hand work to the fleet.
---
# Post a task

Every fact in this repo is a signal: `kind: pragmatic`, `type: signal`. The
ontology refuses anything else, by rule name.

Pick a task id first: short, kebab-case, unique in this mission (for example
`task-auth-refresh-1`). It is the task's ONE entity, and every claim, bid,
working copy and ack for it carries the same entity.

## Generally available (claim / wait / take)

Call `knomit_learn` with one fact:

- `topic: tasks`, `category: <lane>` (a lane groups tasks, e.g. `docs`, `backend`)
- `kind: pragmatic`, `type: signal`
- `title`: one line; `body`: what to do and what "done" means, and ONE line
  naming the knowledge base the results go to: `knowledge base: repo <name>`
  or `knowledge base: lens <name>` (the session binds it; a task without it is
  acknowledged as failed)
- `entities: [<task id>]`: exactly one
- `expires`: an RFC 3339 instant with an offset (e.g. `2026-10-07T12:00:00Z`).
  Past it the `expire` trigger retracts the task, whether or not anyone took it.
- `refs`: the mission charter or the facts the task depends on

The exact call (fill the `<...>` values; `binding` is the mission repo's
handle from `knomit_bind`). `trace` is REQUIRED: `Knomit-Trace` is the
mission repo's name and `Mission-Task` the task id (README "The trace").
Without it, knomit gives the scripts' writes for this task (claims, copies)
the task id as their trace, and they drop out of the mission's story.

```json knomit_learn
{"binding": "<mission>", "moment_name": "post <task id>", "facts": [{"topic": "tasks", "category": "<lane>", "kind": "pragmatic", "type": "signal", "title": "<one line>", "body": "<what to do and what done means>\nknowledge base: lens <name>", "entities": ["<task id>"], "expires": "2026-10-07T12:00:00Z", "refs": ["<the charter's path>"]}], "trace": {"Knomit-Trace": "<mission repo name>", "Mission-Task": "<task id>"}}
```

For an offer or an assigned task, change only `topic` and `category` as
below.

Every participating machine claims it; after the claim window the first by
rank takes it with one atomic move.

## Host-awarded (exactly once)

The same fact, but `topic: offers` and `category: <awarder agent id>/<lane>`.
The awarder (normally the instance that hosts this repo) hands it to exactly
one bidder. Ask the operator for the awarder's agent id if you do not know it.

## Assigned to one agent

The same fact, but `topic: inbox` and `category: <agent id>/working`. It is
never claimed, and it wakes that agent's session. Its `expires` is a LEASE,
not a deadline: set it about 5 minutes ahead. If no session has taken the copy
by then, the `lease` trigger wakes one; nothing ever retracts a copy because
its lease ran out.

## Where the knowledge goes

The mission's knowledge goes to the TARGET knowledge base: an existing
knowledge base the charter names (made from any template), which stays after
the mission. Every task names it on its `knowledge base:` line. Findings,
syntheses and hypotheses are ordinary facts there, under that knowledge
base's own topics, written the way knomit writes them: a hypothesis is
`type: hypothesis`, with no format of the mission's to paste.

This mission repo holds only coordination and ANNOTATIONS: facts about the
target knowledge base's facts, under `annotations/<task id>/`, each with a
ref to the fact it is about and a `context` that says what kind of
annotation it is (today one kind, `verdict`). When a mission needs more data
about a knowledge-base fact, that data is an annotation here, never a format
in the knowledge base.

## Cross-checks and the fold

Hypotheses are SHARED facts: every agent reads them, and only ONE task at a
time may change them. Two tasks that update the same fact in parallel cannot
both land as written: each works in its own experiment, and the later
commit is refused, or field-merged with one side's change dropped (the
refusal stopped the first mission). So a round of cross-checks is two kinds
of task.

### 1. Cross-checks, in parallel

One task per checker, assigned to it (`topic: inbox`,
`category: <agent id>/working`). A cross-check NEVER updates what it checks
and writes nothing to the target knowledge base: it writes one annotation
per fact checked into THIS mission repo, under its own task id. Paste this
block into its body:

```text cross-check-task
Cross-check: check exactly the facts on the check: lines below, and no
other, whoever wrote them. They are in the knowledge base this task names.
Never knomit_update them, and write nothing to that knowledge base. For
each fact you check, write ONE annotation with knomit_learn on the MISSION
handle: topic: annotations, category: <this task's id>, type: observation,
context: {"kind": "verdict", "verdict": "corroborate" or "contradict",
"confidence": the confidence you think the fact deserves, 0 to 1},
confidence: how sure you are of your verdict, refs: the fact you checked as
kb://<its repo id>/<its path> AND your evidence, body: your reasons.
The facts to check:
check: <path>
check: <path>
```

Fill one `check: <path>` line per fact this checker checks, with the exact
path in the target knowledge base. Never write "do not check your own"
instead: a session cannot tell who wrote a fact (in the first mission a
checker updated its own hypotheses under exactly that instruction). Who
wrote what is in git, so the coordinator picks the paths, with git access to
the target knowledge base (a clone, or the repo's directory under the
hosting instance's knomit home):

1. The facts to check: the paths the hypothesize tasks' acks list, or
   `git ls-tree -r --name-only <consensus branch> -- kb/<topic>/`.
2. Who wrote each one: the author of the commit that ADDED it,
   `git log --diff-filter=A --format=%an -1 <consensus branch> -- <path>`
   (git does not diff merge commits here, so this is the writer).
3. Each checker's own author name: the author who added a fact its own
   earlier task wrote (the same command on that path), or
   `git log --no-merges -1 --format=%an <its agent branch>`. Never the last
   commit with merges: a sync merge on an agent branch can carry someone
   else's name.
4. Give each checker paths whose author is not its own, so that every fact
   gets at least one checker other than its writer.

### 2. The fold, after them, alone

ONE task, assigned to ONE agent, posted only after EVERY cross-check of the
round has acknowledged ("Done:" or "Failed:"). It is the only writer of the
hypotheses. Paste this block into its body, with the cross-check task ids:

```text fold-task
Fold the annotations of the cross-checks <task ids> into what they checked.
For each id, knomit_query on the MISSION handle with path
<root>/annotations/<id>/ (<root> is the first segment of your working
copy's path) and context {"kind": "verdict"}, and repeat with the cursor
until has_more is false: one page holds at most 5. Group the verdicts by the
kb:// ref that names the fact they are about. Everything below goes into
this task's ONE experiment on the knowledge base it names. For each fact:
knomit_explain it, weigh its verdicts and their evidence, and make ONE
change: knomit_update it (the new confidence, the evidence the verdicts
cite added to its refs - refs replace the whole list - and a sentence in
its body saying what the verdicts found), or merge it into a hypothesis
that says the same (update that one, retract this one), or knomit_retract
it. Never ref the annotations from the knowledge base. Where the verdicts
contradict it strongly, also write a counter-hypothesis with knomit_learn,
type: hypothesis, with distinct_from naming the hypothesis it counters.
Use moment_name "fold: <task ids>". Change nothing else. If a cross-check
failed, fold the verdicts that exist and name the missing checker in your
acknowledgement.
```

knomit does not hold the fold back until the acks are in: the coordinator
posts it then. Posted early, it folds a partial round, which is safe
(nothing else writes the hypotheses), only incomplete.

## Re-offering

knomit never re-offers. A task that expired untaken, or whose ack says
"Failed:", is the coordinator's to decide on. To offer it again, post it again
under a NEW task id (one `knomit_learn`), with `retract` naming the old task if
it is still there.

## Do not

- Edit a posted task to change it. Retract it and post a new one: a posted
  task may already be claimed, and a new id keeps the history readable.
  (An edit is not unsafe: the repo's `conflicts` setting merges concurrent
  edits and records them. It is just harder to follow.)
- Put secrets in a task. Tasks are replicated to every participant.
