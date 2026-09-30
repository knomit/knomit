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
- `title`: one line; `body`: what to do and what "done" means
- `entities: [<task id>]`: exactly one
- `expires`: an RFC 3339 instant with an offset (e.g. `2026-10-07T12:00:00Z`).
  Past it the `expire` trigger retracts the task, whether or not anyone took it.
- `refs`: the mission charter or the facts the task depends on

Every participating machine claims it; after the claim window the first by
rank takes it with one atomic move.

## Host-awarded (exactly once)

The same fact, but `topic: offers` and `category: <awarder agent id>/<lane>`.
The awarder (normally the instance that hosts this repo) hands it to exactly
one bidder. Ask the operator for the awarder's agent id if you do not know it.

## Assigned to one agent

The same fact, but `topic: inbox` and `category: <agent id>/working`. It is
never claimed, and it wakes that agent's session.

## Do not

- Edit a posted task to change it. Retract it and post a new one: a posted
  task may already be claimed, and a new id keeps the history readable.
  (An edit is not unsafe: the repo's `conflicts` setting merges concurrent
  edits and records them. It is just harder to follow.)
- Put secrets in a task. Tasks are replicated to every participant.
