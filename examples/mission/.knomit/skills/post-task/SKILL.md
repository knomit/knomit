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
handle from `knomit_bind`, and `trace` is optional here):

```json knomit_learn
{"binding": "<mission>", "moment_name": "post <task id>", "facts": [{"topic": "tasks", "category": "<lane>", "kind": "pragmatic", "type": "signal", "title": "<one line>", "body": "<what to do and what done means>\nknowledge base: lens <name>", "entities": ["<task id>"], "expires": "2026-10-07T12:00:00Z", "refs": ["<the charter's path>"]}], "trace": {"Knomit-Trace": "<task id>"}}
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

## Tasks that write hypotheses

A task that asks for predictions (a forecast task, or a synthesis task that
ends in hypotheses) carries the hypothesis format IN ITS BODY, every time.
The session that takes it reads only the task and its skill; a format that
lives anywhere else is a format it never sees. The mission's charter says
WHAT is predicted (the subject, the granularities that matter, which
instruments settle it); the format below says how every prediction is written,
and is the same for every mission.

Paste this block into the task body, after what to do, and fill in the
subject and the granularity (`<...>` marks what to fill; the three format
lines keep their placeholders, the session fills those):

```text hypothesis-format
Hypotheses: write each prediction as one knomit_learn fact with
topic: forecast, category: <subject>/<granularity> (granularity is year,
month or day), type: hypothesis, and confidence = the probability you give it.
Its body starts with these three lines, each on its own line, unindented,
exactly as shown (no bullet, no bold), then your reasoning:
predicted: <the period: YYYY, YYYY-MM or YYYY-MM-DD, at the granularity>
settles_true_if: <an instrument that already exists and can be observed, and what it must show>
settles_false_if: <an instrument that already exists and can be observed, and what it must show>
Add a line "counters: <path>" when it counters another hypothesis.
expires = the last second of the predicted period, in UTC: YYYY-12-31T23:59:59Z
for a year, the last day of the month at 23:59:59Z for a month, the day
itself at 23:59:59Z for a day. refs: the evidence facts. Never retract a
hypothesis because it settled; people decide that.
```

The knowledge base's ontology refuses a hypothesis that breaks this
(`examples/mission-kb/`, README "The knowledge base"), by rule name, so a
session that gets it wrong is told which line and fixes it.

## Cross-checks and the fold

Hypotheses are SHARED facts: every agent reads them, and only ONE task at a
time may change them. Two tasks that update the same fact in parallel cannot
both land: each works in its own experiment, the first commit wins, and the
second is refused for conflicts (this stopped the first mission). So a round
of cross-checks is two kinds of task.

### 1. Cross-checks, in parallel

One task per checker, assigned to it (`topic: inbox`,
`category: <agent id>/working`). A cross-check NEVER updates what it checks:
it writes one new verdict fact per fact checked, under its own task id. Paste
this block into its body:

```text verdict-format
Cross-check: check exactly the facts on the check: lines below, and no
other, whoever wrote them. Never knomit_update the facts you check. For each
fact you check, write ONE new fact with knomit_learn: topic: verdicts,
category: <this task's id>, type: observation, confidence: how sure you
are of the verdict, refs: the fact you checked AND your evidence. Its body
starts with these three lines, each on its own line, unindented, exactly as
shown (no bullet, no bold), then your reasons:
verdict: <corroborate or contradict>
target: <the path of the fact you checked>
suggested_confidence: <the confidence you think it deserves, 0 to 1>
The facts to check:
check: <path>
check: <path>
```

Fill one `check: <path>` line per fact this checker checks, with the exact
path. Never write "do not check your own" instead: a session cannot tell who
wrote a fact (in the first mission a checker updated its own hypotheses
under exactly that instruction). Who wrote what is in git, so the coordinator
picks the paths, with git access to the knowledge base (a clone, or the
repo's directory under the hosting instance's knomit home):

1. The facts to check:
   `git ls-tree -r --name-only <consensus branch> -- kb/forecast/`.
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
Fold the verdicts of the cross-checks <task ids> into what they checked.
For each id, knomit_query path verdicts/<id>/. Group the verdicts by their
target line. For each target: knomit_explain it, weigh its verdicts and
their evidence, and make ONE knomit_update: the new confidence, and refs =
every ref it has now plus each verdict's path (refs replace the whole
list), moment_name "fold: <task ids>". Where the verdicts contradict it
strongly, also write a counter-hypothesis (a new hypothesis with a
counters: line). Change nothing else. If a cross-check failed, fold the
verdicts that exist and name the missing checker in your acknowledgement.
```

knomit does not hold the fold back until the acks are in: the coordinator
posts it then. Posted early, it folds a partial round, which is safe (nothing
else writes the hypotheses), only incomplete.

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
