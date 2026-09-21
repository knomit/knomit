---
name: knomit-experiment
description: Use when writing knomit facts for code that is going out as a PR, or when knowledge work might not survive review — opens an isolated exp/<name> branch, keeps facts off the agent branch until the PR merges, and commits or rolls back with the PR's fate
---

# /knomit-experiment <open|commit|rollback|sync|list> [name]

Wraps the `knomit_experiment` MCP tool and carries the project workflow that
goes with it.

An experiment is a local `exp/<name>` branch forked from this instance's agent
branch. Opening one MOVES THIS SESSION onto it: every later `knomit_learn`,
`knomit_update`, `knomit_retract`, `knomit_query`, `knomit_review` reads and
writes the experiment until you commit or roll back. It is never pushed, never
fetched, and invisible to peers.

## Preflight — the tool may not be there

`knomit_experiment` ships in #236. If the tool is absent from your MCP tool
list, the lens server you are connected to predates it: say so and stop rather
than falling back to writing on the agent branch, which is the exact
attribution this skill exists to prevent. Restarting the server against a
post-#236 build is the fix, and that is the user's call.

## When to use — trigger phrases

**Open an experiment BEFORE the first fact, not after:**

- You are about to write facts about code that is going out as a **PR**. The
  experiment is named after the branch (see the workflow below). This is the
  default path now — it replaces "write facts after the merge".
- You are recording something you are **not sure survives review**: a proposed
  convention, a synthesis you want to try, a hypothesis sweep.
- You are about to run `/knomit-review` or `/knomit-hypothesize` and want the
  output quarantined until you have read it.
- A user says "try", "experiment with", "see how it looks", "don't commit this
  yet" about knowledge work.

**Do NOT open one for:**

- A single obvious fact about code already on the default branch.
- Work on a **subscription** — it has no agent branch and `open` refuses.
- Anything you intend to keep regardless. An experiment you always commit is
  ceremony; write to the agent branch.

## The five actions

| Action | Call | Effect |
|---|---|---|
| list | `{action: "list"}` | this repo's experiments, marking the one you are in |
| open | `{action: "open", name, description}` | fork if absent, else resume; moves this session onto it |
| commit | `{action: "commit"}` | merge into the agent branch, then DELETE the experiment |
| rollback | `{action: "rollback"}` | delete the experiment and everything on it |
| sync | `{action: "sync"}` | merge the agent branch INTO the experiment, **agent branch wins** |

`name` defaults to the experiment you are in for commit/rollback/sync; `open`
always needs it. `description` is local-only and never enters git.

**`open` moves you on every mount.** A `--repo` or `--lens` bridge is scoped by
its URL, but the experiment you open is recorded against your session and that
mount, so the very next call writes inside it — no reconnect, no bridge flag.
The result says which branch that is.

TWO EXCEPTIONS, both of which refuse rather than pretend:

- A mount whose URL already NAMES an experiment
  (`/repos/{repo}/branches/exp:<name>/mcp`) serves that one. Opening a
  different experiment there creates it and refuses to switch, because the
  endpoint is an address.
- A connection with no MCP session id has nothing to attach the experiment to,
  so `open` refuses. Bind on the unscoped mount instead.

**One connection is one session.** If your client shares a connection across
several jobs — Claude Desktop does — they share the session id, so opening an
experiment moves all of them on that mount. It can never reach another
knowledge base: the URL fixed the repo before any of this applied. Say so if
you are not the only job on the connection.

**On open, warn about expiry.** An experiment with no commits for
`experiments.expiry_days` (default 30) is rolled back automatically by the
sweeper — the whole thing, not just the stale part. A long-running PR can
outlive its own experiment. Reading or binding does NOT refresh it; only a
commit on the experiment does.

## The project workflow this replaces

Facts for PR'd code used to be written only AFTER the merge, with a post-merge
PR comment as the gate. That changes:

1. **When the branch is created**, open an experiment named after it:
   `{action: "open", name: "<branch-name>", description: "Facts for <branch>. Commit when #<n> merges."}`
   Keep the name identical to the git branch — that is what lets anyone match
   an orphaned experiment to its PR later.
2. **Write facts as the work happens**, inside the experiment. This is the
   point: the knowledge is captured while it is fresh instead of being
   reconstructed days later from a diff.
3. **When the GitHub PR MERGES**, run the commit procedure below.
4. **If the PR is abandoned or closed unmerged**, `rollback`. The facts
   described code that never existed on the default branch; keeping them is
   how the corpus starts describing a world nobody is in.
5. **The post-merge PR comment stays**, but only as a LISTING of what was
   written. It is no longer the gate — the experiment's commit is.

## The commit procedure — refs must be re-pinned to the merge commit

Facts written during the PR cite `src://` refs at **branch** commits. After the
merge those commits may be unreachable, and a fact whose ref cannot be resolved
is worse than one with no ref: it looks anchored and is not.

Before `{action: "commit"}`:

1. **Find the merge commit** of the PR on the default branch.
2. **For every fact in the experiment, re-read each `src://` ref at the merge
   commit.** The blob form (`src://<repo-id>/<path>@<commit>:<blob>`) is what
   you are repointing — compare with `git rev-parse <merge-commit>:<path>`.
3. **A squash merge orphans every branch commit.** If the PR was squashed, the
   refs point at commits that are no longer reachable: they MUST be re-pinned,
   not merely checked. Refuse to commit the experiment until they are.
4. **Re-read each fact's own claim at the merge commit.** Review changes code.
   A fact that was true of the branch at the time you wrote it may be false of
   what actually merged — that is the single most common way a corpus acquires
   a confident lie. Update or retract before committing, not after.
5. Then commit.

## When commit is REFUSED

`commit` refuses whenever the agent branch and the experiment both changed the
same fact since the fork. Nothing is merged and nothing changes — a refusal is
free, and it is the ONLY reliable signal that your edits collide.

**Do not sync to avoid a refusal.** `sync` resolves every collision in the
agent branch's favour, silently: it overwrites the experiment's version of
exactly the facts under dispute. Syncing first answers on your behalf a
question you would otherwise have been asked, and you never learn which facts
you lost. Attempt the commit and let it refuse.

The refusal names each conflicting path and, for each, the commits to read it
at. Then:

1. **Read all three versions of each path** with
   `knomit_explain {file, commit}`, using the commits the refusal gave you —
   the merge base, the experiment's, and the agent branch's. Use those exact
   commits: the base is the real merge base, which after a sync is NOT the
   experiment's `fork_commit`.

   For a path the refusal marks **NO BASE VERSION**, both sides added it
   independently and there is no third version. Do not go looking for one:
   `knomit_explain` at a commit where the path does not exist does not fail,
   it answers with the nearest earlier version — a different fact, with
   nothing to tell you it substituted.

2. **Decide per path**, and say which you chose and why:
   - the agent branch's version subsumes yours → `"theirs"`
   - yours subsumes it → `"ours"`
   - both say something → compose the merged text and send it as
     `{"body": "<the full fact>"}`

   Remember which way round these words go here: `commit` merges the
   experiment INTO the agent branch, so **"ours" is the experiment** — the
   opposite of git's convention, where "ours" is the branch you merge into.

3. **Retry with the resolutions**, all in one call:

   ```
   knomit_experiment {action: "commit", resolutions: {
     "kb/a/….md": "ours",
     "kb/b/….md": "theirs",
     "kb/c/….md": {"body": "---\ntype: observation\n---\n# …\n\n…"}
   }}
   ```

   It stays ONE merge: everything you did not mention merges exactly as it
   would have. Every path the refusal listed needs an entry — leave one out
   and it refuses again, naming only what is still unresolved. A path that did
   NOT conflict is an error, not a no-op.

   A `{body}` is judged exactly as `knomit_update` judges a rewrite — parsed,
   validated against the ontology, refs gated — and what lands is the
   serialized fact, not your raw bytes. Send the whole fact, frontmatter
   included.

**If every path resolves to `"theirs"`**, the result is identical to the agent
branch: no commit is written and the branch does not move. The experiment is
still deleted, and the summary says so rather than claiming a merge. That is
correct, not a failure.

The alternative exit is `rollback`, which discards the whole experiment.

**`sync` is for the plain catch-up case** — you want the agent branch's recent
work inside your experiment and nothing is in dispute. It is not a conflict
tool, and it is never the first move after a refusal.

**Auto vs interactive is your judgement, not a flag.** Resolving a conflict in
a fact you wrote this session, where the merge is obvious, is auto. Resolving
one in a fact someone else wrote, or where the two versions disagree about a
claim rather than its wording, is interactive: show the human ours, theirs,
and your proposed merge, then act on their pick.

## Things that will bite

- **Two sessions may share an experiment.** The branch lock serialises writes,
  so this is allowed — but the other session committing it deletes it under
  you. Your next call heals onto the agent branch and SAYS SO
  (`lapsed_experiment`). Believe that marker: continuing as though you were
  still inside is how the next fact lands on the wrong branch.
- **An experiment whose recorded parent is not this instance's current agent
  branch is orphaned**: not writable, not committable, only rollback-able.
  The answer comes from the record, never from the `exp/` prefix.
- **Rollback is unrecoverable** and takes the whole experiment. Confirm with
  the user when the experiment holds work they have not seen.
- **`open` on an existing name RESUMES** rather than erroring, and replaces the
  description if you pass one. Re-opening with no description keeps the old.
- The UI can browse experiments and commit or roll one back, but it cannot
  create one and cannot resolve a conflict — that is this tool's job.

## After the merge commits

Only now do the facts exist on the agent branch. Post the PR listing comment,
and use `/knomit-decided` for any tradeoffs the PR settled that are not yet
recorded — inside the NEXT experiment if more PR work follows.
