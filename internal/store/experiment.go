// Experiments: a local `exp/<name>` branch forked from this instance's agent
// branch, with the fork recorded explicitly in the repo DB.
//
// An experiment is an isolated world. A session bound to it reads and writes
// there and every tool works on it unchanged; nothing is special-cased per
// tool. This file owns the LIFECYCLE — fork, commit, sync, rollback, expire —
// and nothing above it in the stack needs to know how a fork is assembled.
//
// The branch prefix is a NAMING convention, never the source of truth about
// what a branch is for. Parentage lives in the `experiments` row, because a
// branch holds a role only when an explicit record says so
// (kb/invariants/store/branch-roles); write eligibility one layer up reads
// that row, not the ref name.
package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"knomit/internal/fact"
)

// MaxExperimentNameLen bounds the name so a ref path stays sane and an
// accidental paste cannot become a branch.
const MaxExperimentNameLen = 64

// experimentNameRe is strict kebab-case: lowercase alphanumerics in groups
// separated by single hyphens. It rejects the empty string, uppercase,
// underscores, spaces, leading/trailing/doubled hyphens, and — because it
// admits no slash or dot — every path-traversal shape.
var experimentNameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

var (
	// ErrInvalidExperimentName is returned for a name that is not kebab-case
	// or is too long. Typed so the MCP and REST layers can answer with the
	// rule rather than a SQL or git error.
	ErrInvalidExperimentName = errors.New("experiment name must be kebab-case (lowercase letters, digits and single hyphens)")
	// ErrNoSuchExperiment is returned by commit/sync/rollback for a name that
	// was never opened — answered from the RECORD, before any ref is touched,
	// so the caller gets "no such experiment" rather than a missing-ref error
	// from deep inside the merge.
	ErrNoSuchExperiment = errors.New("no such experiment")
)

// validateExperimentName is the one gate every entry point goes through,
// BEFORE anything is created, so a rejected name leaves no ref behind.
func validateExperimentName(name string) error {
	if len(name) > MaxExperimentNameLen {
		return fmt.Errorf("%w: %q is longer than %d characters", ErrInvalidExperimentName, name, MaxExperimentNameLen)
	}
	if !experimentNameRe.MatchString(name) {
		return fmt.Errorf("%w: got %q", ErrInvalidExperimentName, name)
	}
	return nil
}

// MergeConflictError is what a StrategyRefuse merge returns instead of
// resolving a conflict: the full set of paths both sides changed relative to
// the merge base, with NOTHING written. It is a typed error because the caller
// has to render the paths — "there was a conflict" is not actionable, and the
// resolution is chosen by looking at which paths.
//
// It also carries the THREE COMMITS the caller needs to read each fact at.
// Without them a caller is told a path conflicted and has no way to see the
// three versions that made it one: the fork point, what the experiment made of
// it, and what the parent made of it. An agent resolving this reads the fact
// at each (knomit_explain takes a commit), which is the whole difference
// between adjudicating a merge and guessing at one.
type MergeConflictError struct {
	Src   string
	Dst   string
	Paths []string

	// BaseCommit is the ACTUAL merge base — dst.MergeBase(src) — and not the
	// experiment's recorded fork_commit. After a sync the two differ, and the
	// recorded fork would hand the reader a base version the merge did not
	// use, which is worse than no base at all: it looks authoritative.
	//
	// SrcCommit and DstCommit are the two tips as they stood when the merge
	// was refused. All three are full hashes, empty only if the merge failed
	// before they were resolved.
	BaseCommit string
	SrcCommit  string
	DstCommit  string

	// PathsWithoutBase are the conflicting paths that DO NOT EXIST at the
	// merge base — both sides added them independently. There is no third
	// version to read for these, and saying so matters: knomit_explain for a
	// path absent at a commit does not error, it falls back to the nearest
	// earlier version and answers with a DIFFERENT fact. A reader told to
	// "read the base version" of a dual-add would be shown something
	// unrelated and have no way to tell.
	PathsWithoutBase []string

	// Rewound says the refusal came from rebuilding an experiment that was
	// open across a rewind of its agent branch (rebuildRewoundExperiment):
	// its own commits are replayed onto the rebuilt branch, so a path the
	// rewind removed that the experiment edited conflicts as well. The base
	// is where the experiment last met the old branch; DstCommit is the
	// rebuilt branch.
	Rewound bool
}

// HasBase reports whether the given conflicting path has a base version worth
// reading.
func (e *MergeConflictError) HasBase(path string) bool {
	for _, p := range e.PathsWithoutBase {
		if p == path {
			return false
		}
	}
	return true
}

func (e *MergeConflictError) Error() string {
	return fmt.Sprintf("merge %s into %s refused: %d conflicting path(s): %s",
		e.Src, e.Dst, len(e.Paths), strings.Join(e.Paths, ", "))
}

// ResolutionSide names which side of a conflicting path survives.
//
// DELIBERATELY "src"/"dst" AND NOT "ours"/"theirs". Which branch is "ours"
// depends on who is asking: an agent committing an experiment means the
// EXPERIMENT by "ours", while git's own convention means the branch you are
// merging INTO — and for an experiment commit those are opposite sides. The
// translation happens once, at the tool boundary where the asker is known,
// and everything below here says src and dst, which cannot be read two ways.
// This is the same trap conventions/store/sync/strategy-local-wins records for
// RemoteWins ("src wins", not "the remote wins").
type ResolutionSide string

const (
	// ResolveSrc takes the merge SOURCE's version. For an experiment commit
	// (exp -> parent) that is the experiment's.
	ResolveSrc ResolutionSide = "src"
	// ResolveDst keeps the merge DESTINATION's version. For an experiment
	// commit that is the parent branch's.
	ResolveDst ResolutionSide = "dst"
)

// Resolution adjudicates ONE conflicting path.
//
// Body wins over Side when set, and is how a caller lands content that is
// neither side — the merged text. A nil Body with an empty Side is not a
// resolution and is rejected rather than defaulted: silently picking a side
// for a caller who named a path but not an answer is exactly the adjudication
// StrategyRefuse exists to refuse.
type Resolution struct {
	Side ResolutionSide
	Body []byte
}

// testHookOpenExperimentForked, when set (tests only), runs in OpenExperiment
// between the fork and the experiments row, inside the parent's lock.
var testHookOpenExperimentForked func()

// OpenExperiment creates the experiment if it is absent and RESUMES it if it
// is not — the same call either way, because an agent that reconnects should
// not have to know which of the two it is doing.
//
// On create it does three things beyond CreateBranch: it copies the parent's
// `pipeline_watermarks` rows, it records the parentage, and it records the
// fork commit. The watermark copy is the reason this is a sibling of
// CreateBranch rather than a flag on it: CreateBranch deliberately does NOT
// inherit the pipeline watermark, and a fresh branch is therefore full-corpus
// dirty on its first pipeline run (kb/architecture/synthesize/pipeline-
// watermark). That is right for a fresh AGENT branch and wrong for an
// experiment, whose whole point is to review its own delta.
//
// On resume, a non-empty description replaces the stored one and an empty one
// leaves it — clearing a note is not something an unrelated reconnect should
// do by omission.
//
// A name whose ref exists with NO row is neither: it is refused with
// ErrOrphanExperimentRef. "Resume" means resuming a recorded experiment, and
// a ref without a record is precisely the case where we cannot say what we
// would be resuming.
func (rh *repoHandler) OpenExperiment(ctx context.Context, name, description, parent string) (Experiment, error) {
	if err := validateExperimentName(name); err != nil {
		return Experiment{}, err
	}
	if parent == "" {
		return Experiment{}, fmt.Errorf("OpenExperiment %q: parent branch is required", name)
	}

	if existing, ok, err := rh.GetExperiment(ctx, name); err != nil {
		return Experiment{}, err
	} else if ok {
		if description == "" || description == existing.Description {
			return existing, nil
		}
		if _, err := conn(ctx, rh.db).ExecContext(ctx,
			`UPDATE experiments SET description = ? WHERE name = ?`, description, name); err != nil {
			return Experiment{}, fmt.Errorf("OpenExperiment %q: update description: %w", name, err)
		}
		existing.Description = description
		return existing, nil
	}

	branch := ExperimentBranch(name)
	// REFUSE an orphan ref rather than adopt it. We are past the resume path,
	// so there is no record for this name — and CreateBranch NO-OPS when the
	// ref already exists, which would leave the caller holding a branch that
	// carries whatever that ref pointed at while the row claims a fresh fork
	// of the parent. Reachable two ways: a previous open that died between
	// CreateBranch and the INSERT, and a hand-made ref. Both want a human,
	// not a silent adoption.
	if existing, err := rh.HeadCommit(ctx, branch); err == nil && existing != "" {
		return Experiment{}, fmt.Errorf("%w: %q already exists at %s with no experiments row; "+
			"roll back %q and open it again, or pick another name",
			ErrOrphanExperimentRef, branch, shortHash(existing), name)
	}
	// A rewind record left by an experiment of the same name (a drop that
	// failed after its branch went) must not make the new one look rewound.
	if err := rh.clearExperimentRewound(name); err != nil {
		return Experiment{}, fmt.Errorf("OpenExperiment %q: clear stale rewind record: %w", name, err)
	}
	// The PARENT'S LOCK is held from the fork to the experiments row. A rewind
	// records the experiments it finds as ROWS (markExperimentsRewound, under
	// the same lock), so without it a rewind could run between the fork (at
	// the pre-rewind tip) and the row: an experiment holding the scrubbed
	// commits with no record, whose commit would bring them back.
	unlockParent := rh.lockBranch(parent)
	defer unlockParent()
	if err := rh.CreateBranch(ctx, branch, parent); err != nil {
		return Experiment{}, fmt.Errorf("OpenExperiment %q: %w", name, err)
	}
	if testHookOpenExperimentForked != nil {
		testHookOpenExperimentForked()
	}
	// The fork point is read from the NEW BRANCH, after it exists — never
	// from the parent beforehand: fork_commit is the only anchor a "changed
	// since the fork" view has, and it must be exactly where the branch
	// starts.
	forkCommit, err := rh.HeadCommit(ctx, branch)
	if err != nil {
		return Experiment{}, fmt.Errorf("OpenExperiment %q: resolve new branch %q: %w", name, branch, err)
	}
	// Inherit every tool's watermark, not just review's: hypothesize keys its
	// own row under the same branch and would otherwise full-scan too.
	if _, err := conn(ctx, rh.db).ExecContext(ctx, `
		INSERT OR IGNORE INTO pipeline_watermarks (tool, branch, commit_hash)
		SELECT tool, ?, commit_hash FROM pipeline_watermarks WHERE branch = ?`,
		branch, parent); err != nil {
		return Experiment{}, fmt.Errorf("OpenExperiment %q: inherit pipeline watermarks: %w", name, err)
	}

	now := time.Now()
	if _, err := conn(ctx, rh.db).ExecContext(ctx, `
		INSERT INTO experiments (name, description, parent_branch, fork_commit, created_at, last_activity_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		name, description, parent, forkCommit, now.Unix(), now.Unix()); err != nil {
		return Experiment{}, fmt.Errorf("OpenExperiment %q: record experiment: %w", name, err)
	}

	log.Info().Str("experiment", name).Str("parent", parent).
		Str("fork", shortHash(forkCommit)).Msg("experiment opened")

	return Experiment{
		Name:           name,
		Description:    description,
		Parent:         parent,
		ForkCommit:     forkCommit,
		CreatedAt:      time.Unix(now.Unix(), 0),
		LastActivityAt: time.Unix(now.Unix(), 0),
	}, nil
}

// ListExperiments returns every experiment in this repo, ordered by name so
// the listing is stable for a UI and for a test.
func (rh *repoHandler) ListExperiments(ctx context.Context) ([]Experiment, error) {
	rows, err := conn(ctx, rh.db).QueryContext(ctx, `
		SELECT name, description, parent_branch, fork_commit, created_at, last_activity_at
		FROM experiments ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("ListExperiments: %w", err)
	}
	defer rows.Close()

	var out []Experiment
	for rows.Next() {
		var (
			e          Experiment
			created    int64
			lastActive int64
		)
		if err := rows.Scan(&e.Name, &e.Description, &e.Parent, &e.ForkCommit, &created, &lastActive); err != nil {
			return nil, fmt.Errorf("ListExperiments: scan: %w", err)
		}
		e.CreatedAt = time.Unix(created, 0)
		e.LastActivityAt = time.Unix(lastActive, 0)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListExperiments: %w", err)
	}
	return out, nil
}

// CommitExperiment merges the experiment into its RECORDED parent and, on
// success, deletes the experiment.
//
// The parent comes from the row, not from the caller — re-deriving it would
// be exactly the ref-shape inference the branch-roles invariant forbids. But
// it is not trusted blindly either: checkExperimentParentCurrent first
// compares it against the agent branch this database RECORDS as its owner,
// and refuses with ErrStaleExperimentParent when the repo was taken over
// between the fork and now. Write eligibility refuses the same case one layer
// up for a client; this one also covers an in-process caller.
//
// On CONFLICT it returns a *MergeConflictError and changes nothing: the
// refuse strategy collects the paths inside the tree merge, which runs before
// any ref write. The only two exits from that state are SyncExperiment and
// RollbackExperiment.
func (rh *repoHandler) CommitExperiment(ctx context.Context, name string, resolutions map[string]Resolution) (AgentReconcileResult, error) {
	exp, ok, err := rh.GetExperiment(ctx, name)
	if err != nil {
		return AgentReconcileResult{}, err
	}
	if !ok {
		return AgentReconcileResult{}, fmt.Errorf("%w: %q", ErrNoSuchExperiment, name)
	}
	if err := rh.checkExperimentParentCurrent(ctx, exp); err != nil {
		return AgentReconcileResult{}, err
	}

	// The parent's lock (the merge's DST) serialises this against a concurrent
	// fact write on the agent branch and against a rewind of it, and is held
	// through notifyCommit. An experiment's own merges (commit and sync) are
	// the only merges that stamp the call's agent trace (opts.trace).
	// STILL ONE MERGE. Resolutions are an input to it, not a second pass over
	// the tree afterwards: a path the caller adjudicated is decided inside the
	// same three-way walk that would otherwise have refused it, so everything
	// else merges exactly as it did before and a path with no resolution is
	// refused by construction rather than by falling through to a default.
	//
	// The agent `trace` of the call (knomit_experiment, #349 extended) is
	// stamped on the merge commit this writes — the experiment's own merge
	// opts in explicitly; a fast-forward writes no commit, so it stamps
	// nothing (the fact commits it carries keep the traces their writes had).
	//
	// THE REPO'S `conflicts` SETTING APPLIES HERE TOO. It is read where every
	// other merge site reads it — the tip of the consensus branch
	// (UpstreamBranch), never the experiment's or the parent's own ontology,
	// so an experiment cannot vote itself a merge policy. With it on, the
	// parent is the consensus side (it is the branch that reaches consensus,
	// as the host's own branch is for MergePushed), and whatever a key set to
	// off leaves is refused as before. Under `consensus: auto` an absent
	// `conflicts` reads facts: merge / state: consensus, so such a repo
	// field-merges by default. The caller's resolutions are laid over the
	// setting per path (overlayResolutions), so following a refusal never
	// switches the setting off for the paths it already settled.
	strategy, opts := StrategyRefuse, mergeOpts{}
	if rh.consensusBranch != nil {
		if s, on := rh.conflictsStrategy(ctx, rh.consensusBranch()); on {
			strategy = s
			opts = mergeOpts{factConsensus: fact.MergeDst, factFallback: StrategyRefuse, overlayResolutions: true}
		}
	}
	//
	// AN EXPERIMENT OPEN ACROSS A REWIND is rebuilt on the parent first
	// (rebuildRewoundExperiment), under both locks, so a commit never merges
	// back what the rewind scrubbed. The rebuild consumes the resolutions; the
	// merge after it is then a fast-forward.
	opts.trace = trailersFromContext(ctx)
	unlockParent := rh.lockBranch(exp.Parent)
	unlockExp := rh.lockBranch(exp.Branch())
	rebuilt, _, settled, err := rh.rebuildRewoundExperiment(ctx, exp, strategy, resolutions, opts)
	unlockExp()
	if err != nil {
		unlockParent()
		return AgentReconcileResult{}, fmt.Errorf("CommitExperiment %q: rebuild after a rewind: %w", name, err)
	}
	if rebuilt {
		resolutions = nil
	}
	res, err := rh.mergeIntoBranchLockedOpts(ctx, exp.Branch(), exp.Parent, strategy, resolutions, opts)
	unlockParent()
	if err != nil {
		return AgentReconcileResult{}, err
	}
	res.Settled = append(settled, res.Settled...)

	if err := rh.dropExperiment(ctx, exp); err != nil {
		return AgentReconcileResult{}, fmt.Errorf("CommitExperiment %q: %w", name, err)
	}
	log.Info().Str("experiment", name).Str("parent", exp.Parent).
		Str("mode", string(res.Mode)).Msg("experiment committed")
	return res, nil
}

// SyncExperiment merges the parent INTO the experiment, parent-wins on
// conflicting paths.
//
// Parent-wins is StrategyRemoteWins here and that is not a typo: the strategy
// names which SIDE of the merge wins, and src is the parent on this direction
// (kb/conventions/store/sync/strategy-local-wins — "RemoteWins: src wins").
// Getting it backwards would silently keep the experiment's version of exactly
// the path the sync exists to resolve, and the following commit would be
// refused again with no visible reason.
func (rh *repoHandler) SyncExperiment(ctx context.Context, name string) (AgentReconcileResult, error) {
	exp, ok, err := rh.GetExperiment(ctx, name)
	if err != nil {
		return AgentReconcileResult{}, err
	}
	if !ok {
		return AgentReconcileResult{}, fmt.Errorf("%w: %q", ErrNoSuchExperiment, name)
	}
	if err := rh.checkExperimentParentCurrent(ctx, exp); err != nil {
		return AgentReconcileResult{}, err
	}
	// An experiment open across a rewind is rebuilt on the parent instead of
	// merged (rebuildRewoundExperiment): a merge would keep what the rewind
	// scrubbed. Its conflicts are refused, never settled parent-wins: the
	// experiment's own version would be gone for good, since the commits that
	// held it are not kept.
	//
	// LOCKS. A plain sync holds only the experiment's lock (the merge's dst),
	// as it always has. A recorded experiment's rebuild also takes the
	// parent's, first, the order commit and the rewind use. The record is
	// read here without the parent's lock: a rewind that records the
	// experiment just after this read lets one plain sync through, which
	// keeps the scrubbed content in the experiment, but the record stays and
	// the next commit or sync rebuilds from it (its base is still where the
	// experiment met the old branch), so nothing leaks.
	o := mergeOpts{trace: trailersFromContext(ctx)}
	recorded := false
	if _, err := rh.gits.Reference(experimentRewoundRefName(exp.Name)); err == nil {
		recorded = true
		unlockParent := rh.lockBranch(exp.Parent)
		defer unlockParent()
	}
	unlockExp := rh.lockBranch(exp.Branch())
	defer unlockExp()
	rebuilt, squashed := false, 0
	var settled []SettledPath
	if recorded {
		// Never without the parent's lock: a rewind moving the parent under a
		// rebuild would leave the experiment on a tip the rewind replaced,
		// with its record already cleared.
		if rebuilt, squashed, settled, err = rh.rebuildRewoundExperiment(ctx, exp, StrategyRefuse, nil, o); err != nil {
			return AgentReconcileResult{}, fmt.Errorf("SyncExperiment %q: rebuild after a rewind: %w", name, err)
		}
	}
	if rebuilt {
		tip, err := rh.HeadCommit(ctx, exp.Branch())
		if err != nil {
			return AgentReconcileResult{}, fmt.Errorf("SyncExperiment %q: %w", name, err)
		}
		log.Info().Str("experiment", name).Str("parent", exp.Parent).
			Int("squashed", squashed).Msg("experiment rebuilt on its parent after a rewind")
		return AgentReconcileResult{Mode: ModeRebase, NumReplayed: squashed, NewTip: tip, Settled: settled}, nil
	}
	res, err := rh.mergeIntoBranchLockedOpts(ctx, exp.Parent, exp.Branch(), StrategyRemoteWins, nil, o)
	if err != nil {
		return AgentReconcileResult{}, fmt.Errorf("SyncExperiment %q: %w", name, err)
	}
	log.Info().Str("experiment", name).Str("parent", exp.Parent).
		Str("mode", string(res.Mode)).Msg("experiment synced from parent")
	return res, nil
}

// RollbackExperiment discards the experiment: the branch and every trace of
// it. There is no archive — the decision was to leave the hook, not build it.
//
// It also cleans an ORPHAN ref — an `exp/<name>` branch with no record, which
// OpenExperiment refuses to adopt. That refusal is only safe because this is
// the way out of it: the state is reachable from an ordinary failure (an open
// that died between CreateBranch and the INSERT), and the person who hits it
// is working in a UI, where "delete the ref by hand" is not an instruction
// anyone can follow. Rolling back work whose provenance is unknown is the
// same safe direction as rolling back work whose provenance is known.
//
// Genuinely absent — no ref AND no row — is still ErrNoSuchExperiment. The
// orphan path must not turn "there was never anything here" into a silent
// success.
func (rh *repoHandler) RollbackExperiment(ctx context.Context, name string) error {
	exp, ok, err := rh.GetExperiment(ctx, name)
	if err != nil {
		return err
	}
	if ok {
		if err := rh.dropExperiment(ctx, exp); err != nil {
			return fmt.Errorf("RollbackExperiment %q: %w", name, err)
		}
		log.Info().Str("experiment", name).Msg("experiment rolled back")
		return nil
	}

	// No record. Validate before constructing a ref name from caller input:
	// every RECORDED name passed this gate at open time, so this costs
	// nothing real, and it keeps a name that is not an experiment name from
	// reaching the ref database at all.
	if err := validateExperimentName(name); err != nil {
		return err
	}
	branch := ExperimentBranch(name)
	head, herr := rh.HeadCommit(ctx, branch)
	if herr != nil || head == "" {
		return fmt.Errorf("%w: %q", ErrNoSuchExperiment, name)
	}
	// Same teardown as a recorded experiment — the leftovers are the same
	// leftovers, and the delete of the (absent) experiments row is a no-op.
	if err := rh.dropExperiment(ctx, Experiment{Name: name}); err != nil {
		return fmt.Errorf("RollbackExperiment %q: clean orphan ref: %w", name, err)
	}
	log.Info().Str("experiment", name).Str("was_at", shortHash(head)).
		Msg("orphan experiment ref rolled back (no record existed)")
	return nil
}

// ExpireExperiments rolls back every experiment whose last activity is older
// than cutoff and returns the names it dropped, in listing order.
//
// SKIP ON DOUBT, in the shape the local reconcile loop uses
// (kb/invariants/store/local-reconcile/never-reset-never-exit): one
// experiment that fails to drop must not stop the others from being swept,
// and must not stop the sweeper. The per-experiment failures are joined into
// the returned error for the caller to log; the names actually dropped come
// back regardless.
func (rh *repoHandler) ExpireExperiments(ctx context.Context, cutoff time.Time) ([]string, error) {
	all, err := rh.ListExperiments(ctx)
	if err != nil {
		return nil, err
	}
	var (
		dropped []string
		errs    []error
	)
	for _, exp := range all {
		if !exp.LastActivityAt.Before(cutoff) {
			continue
		}
		if err := rh.dropExperiment(ctx, exp); err != nil {
			errs = append(errs, fmt.Errorf("expire %q: %w", exp.Name, err))
			continue
		}
		log.Info().Str("experiment", exp.Name).
			Time("last_activity", exp.LastActivityAt).Msg("experiment expired")
		dropped = append(dropped, exp.Name)
	}
	return dropped, errors.Join(errs...)
}

// dropExperiment removes the branch and every piece of state keyed to it. It
// is the shared teardown of commit, rollback and expiry — three callers, one
// definition, because a cleanup that only one of them performed would leave a
// reused name inheriting the dead experiment's state.
//
// Order matters, and it mirrors DropBranch's own reasoning: the branch goes
// FIRST, so a failure anywhere leaves the experiment row in place and the
// whole operation retryable, rather than a record-less ref nobody can name.
//
// The two extra deletes are the ones DropBranch cannot do. `pipeline_watermarks`
// and the `meta` watermark keys are keyed by branch NAME with no foreign key
// to `branches`, so DropBranch does not see them — and this fork WROTE them,
// so this teardown owns them. Leaving them behind is not inert: a later
// experiment reusing the name inherits a watermark pointing at a commit its
// branch has never contained, and its first review seeds nothing at all.
func (rh *repoHandler) dropExperiment(ctx context.Context, exp Experiment) error {
	branch := exp.Branch()
	if err := rh.DropBranch(ctx, branch); err != nil {
		return fmt.Errorf("drop experiment branch: %w", err)
	}
	if _, err := conn(ctx, rh.db).ExecContext(ctx,
		`DELETE FROM pipeline_watermarks WHERE branch = ?`, branch); err != nil {
		return fmt.Errorf("drop experiment watermarks: %w", err)
	}
	if _, err := conn(ctx, rh.db).ExecContext(ctx,
		`DELETE FROM meta WHERE key = ? OR key = ?`,
		"last_commit:"+branch, "graph_schema_version:"+branch); err != nil {
		return fmt.Errorf("drop experiment meta keys: %w", err)
	}
	if err := rh.clearExperimentRewound(exp.Name); err != nil {
		return fmt.Errorf("drop experiment rewind record: %w", err)
	}
	if _, err := conn(ctx, rh.db).ExecContext(ctx,
		`DELETE FROM experiments WHERE name = ?`, exp.Name); err != nil {
		return fmt.Errorf("drop experiment row: %w", err)
	}
	return nil
}

// shortHash abbreviates a hash for a log line without panicking on a short or
// empty one.
func shortHash(h string) string {
	if len(h) < 8 {
		return h
	}
	return h[:8]
}
