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
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

// ExperimentPrefix is the ref namespace every experiment branch lives under.
// Deliberately NOT `agent/…`: the /git advertisement is an allowlist of the
// upstream plus `refs/heads/agent/`, so a name outside it is unreachable from
// the network by construction rather than by a second rule that could drift
// (kb/decisions/repos/git-serve/curated-advertisement).
const ExperimentPrefix = "exp/"

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
	// ErrStaleExperimentParent is returned by commit/sync when the
	// experiment's recorded parent is no longer the agent branch this
	// database records as its owner — the repo was taken over between the
	// fork and now. Rollback is still allowed: throwing away orphaned work is
	// always safe, and it is the only way out of the state.
	ErrStaleExperimentParent = errors.New("experiment's parent is no longer this database's agent branch")
	// ErrOrphanExperimentRef is returned when an exp/<name> ref exists with no
	// experiments row behind it. Refused rather than adopted: the ref's
	// content is unknown, and its parentage is unknowable (that is what the
	// missing row means), so adopting it would present someone else's commits
	// as a fresh fork of the agent branch.
	ErrOrphanExperimentRef = errors.New("an experiment branch with no record already exists")
)

// Experiment is one row of the `experiments` table: an `exp/<name>` branch
// plus the fork it came from.
type Experiment struct {
	Name        string
	Description string
	// Parent is the branch this experiment was forked FROM, recorded at fork
	// time. Eligibility compares it against the instance's current agent
	// branch; it is never re-derived.
	Parent string
	// ForkCommit is the parent's tip at fork time — the point a "changed since
	// the fork" view is computed against.
	ForkCommit     string
	CreatedAt      time.Time
	LastActivityAt time.Time
}

// Branch returns the git branch name this experiment lives on.
func (e Experiment) Branch() string { return ExperimentPrefix + e.Name }

// ExperimentBranch maps a bare experiment name to its branch name.
func ExperimentBranch(name string) string { return ExperimentPrefix + name }

// IsExperimentBranch reports whether a branch name sits in the experiment
// namespace. This is a cheap SHAPE test and answers nothing about
// eligibility — a branch in the namespace may still have no record, or a
// record naming another instance's agent branch as its parent. Callers
// deciding whether a branch may be WRITTEN must read the record.
func IsExperimentBranch(branch string) bool {
	return strings.HasPrefix(branch, ExperimentPrefix) && len(branch) > len(ExperimentPrefix)
}

// ExperimentNameOf returns the bare name for an experiment branch.
func ExperimentNameOf(branch string) (string, bool) {
	if !IsExperimentBranch(branch) {
		return "", false
	}
	return strings.TrimPrefix(branch, ExperimentPrefix), true
}

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
// only two exits (sync, or rollback) are chosen by looking at which paths.
type MergeConflictError struct {
	Src   string
	Dst   string
	Paths []string
}

func (e *MergeConflictError) Error() string {
	return fmt.Sprintf("merge %s into %s refused: %d conflicting path(s): %s",
		e.Src, e.Dst, len(e.Paths), strings.Join(e.Paths, ", "))
}

// Experiments returns the experiment lifecycle sub-service.
func (s *Service) Experiments() ExperimentIndex { return s.rh }

// Compile-time assertion: repoHandler must implement ExperimentIndex.
var _ ExperimentIndex = (*repoHandler)(nil)

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
			"delete that ref or pick another name", ErrOrphanExperimentRef, branch, shortHash(existing))
	}
	if err := rh.CreateBranch(ctx, branch, parent); err != nil {
		return Experiment{}, fmt.Errorf("OpenExperiment %q: %w", name, err)
	}
	// The fork point is read from the NEW BRANCH, after it exists — never
	// from the parent beforehand. CreateBranch resolves the parent itself and
	// neither step holds the parent's lock, so a commit landing on the parent
	// in between would make a fork point read here an ANCESTOR of where the
	// branch actually starts. fork_commit is the only anchor a "changed since
	// the fork" view has, so an ancestor there shows a delta that includes
	// work the experiment never did.
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

// GetExperiment reads one record. The bool distinguishes "no such experiment"
// from an error, so eligibility can answer "not writable" without treating a
// DB failure as an answer.
func (rh *repoHandler) GetExperiment(ctx context.Context, name string) (Experiment, bool, error) {
	var (
		e          Experiment
		created    int64
		lastActive int64
	)
	err := conn(ctx, rh.db).QueryRowContext(ctx, `
		SELECT name, description, parent_branch, fork_commit, created_at, last_activity_at
		FROM experiments WHERE name = ?`, name,
	).Scan(&e.Name, &e.Description, &e.Parent, &e.ForkCommit, &created, &lastActive)
	if errors.Is(err, sql.ErrNoRows) {
		return Experiment{}, false, nil
	}
	if err != nil {
		return Experiment{}, false, fmt.Errorf("GetExperiment %q: %w", name, err)
	}
	e.CreatedAt = time.Unix(created, 0)
	e.LastActivityAt = time.Unix(lastActive, 0)
	return e, true, nil
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

// checkExperimentParentCurrent refuses an experiment whose recorded parent is
// no longer the agent branch this database records as its owner.
//
// This is the STORE's half of a two-layer refusal, the same shape as the
// subscription write refusal: RepoInstance.WritableBranch answers the client
// from the live instance's agent branch, and this answers from the record, so
// an in-process caller that never passed through a binding gate cannot merge
// an orphaned experiment into a branch this database has stopped writing.
// The record is the right authority here because the store has no instance to
// ask (kb/invariants/store/branch-roles: the role comes from the record).
//
// An EMPTY owner is UNKNOWN, not a mismatch. AgentBranchOwner's contract says
// so explicitly — a database created before the key existed, or one that has
// never completed a boot, reports "" — and reading it as "no previous branch"
// would refuse every commit on such a database. Same guard verify.go applies
// to the same value. So the check bites only on a KNOWN, different owner.
func (rh *repoHandler) checkExperimentParentCurrent(ctx context.Context, exp Experiment) error {
	owner, err := rh.AgentBranchOwner(ctx)
	if err != nil {
		return fmt.Errorf("experiment %q: read agent branch owner: %w", exp.Name, err)
	}
	if owner == "" || owner == exp.Parent {
		return nil
	}
	return fmt.Errorf("%w: experiment %q was forked from %q, but this database is now written by %q "+
		"(roll it back, or re-open it from the current branch)",
		ErrStaleExperimentParent, exp.Name, exp.Parent, owner)
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
func (rh *repoHandler) CommitExperiment(ctx context.Context, name string) (AgentReconcileResult, error) {
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

	// mergeIntoBranch takes the DST lock — the parent's — which is what
	// serialises this against a concurrent fact write on the agent branch, and
	// it carries that lock through notifyCommit.
	res, err := rh.mergeIntoBranch(ctx, exp.Branch(), exp.Parent, StrategyRefuse)
	if err != nil {
		return AgentReconcileResult{}, err
	}

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
	res, err := rh.mergeIntoBranch(ctx, exp.Parent, exp.Branch(), StrategyRemoteWins)
	if err != nil {
		return AgentReconcileResult{}, fmt.Errorf("SyncExperiment %q: %w", name, err)
	}
	log.Info().Str("experiment", name).Str("parent", exp.Parent).
		Str("mode", string(res.Mode)).Msg("experiment synced from parent")
	return res, nil
}

// RollbackExperiment discards the experiment: the branch and every trace of
// it. There is no archive — the decision was to leave the hook, not build it.
func (rh *repoHandler) RollbackExperiment(ctx context.Context, name string) error {
	exp, ok, err := rh.GetExperiment(ctx, name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %q", ErrNoSuchExperiment, name)
	}
	if err := rh.dropExperiment(ctx, exp); err != nil {
		return fmt.Errorf("RollbackExperiment %q: %w", name, err)
	}
	log.Info().Str("experiment", name).Msg("experiment rolled back")
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
	if _, err := conn(ctx, rh.db).ExecContext(ctx,
		`DELETE FROM experiments WHERE name = ?`, exp.Name); err != nil {
		return fmt.Errorf("drop experiment row: %w", err)
	}
	return nil
}

// touchExperimentActivity records that a commit landed on branch, when branch
// is an experiment. Called from notifyCommit, the single chokepoint every ref
// mutation goes through, so "activity" means exactly "a commit happened here"
// with no second definition to keep in sync.
//
// It runs on EVERY branch's every commit, so the shape test comes first and
// costs a prefix compare; only an experiment branch reaches SQL. A branch with
// no row updates nothing, which is the correct answer for a hand-made `exp/…`
// ref nobody opened.
func (rh *repoHandler) touchExperimentActivity(ctx context.Context, branch string) error {
	if !IsExperimentBranch(branch) {
		return nil
	}
	name, _ := ExperimentNameOf(branch)
	if _, err := conn(ctx, rh.db).ExecContext(ctx,
		`UPDATE experiments SET last_activity_at = ? WHERE name = ?`,
		time.Now().Unix(), name); err != nil {
		return fmt.Errorf("touch experiment %q: %w", name, err)
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

// SetExperimentActivityForTest backdates an experiment's last_activity_at.
//
// TEST SEAM, and exported only because the sweeper's tests live in
// internal/repos and cannot reach this table any other way. Expiry is
// measured in days, so the alternative is a test that either sleeps for days
// or asserts nothing; backdating the row makes the assertion about WHICH
// experiment the cutoff selects, which is the part that can be wrong.
//
// Not part of ExperimentIndex: production code records activity through
// notifyCommit, which is what keeps "activity" meaning exactly "a commit
// landed here".
func (s *Service) SetExperimentActivityForTest(ctx context.Context, name string, when time.Time) error {
	res, err := conn(ctx, s.rh.db).ExecContext(ctx,
		`UPDATE experiments SET last_activity_at = ? WHERE name = ?`, when.Unix(), name)
	if err != nil {
		return fmt.Errorf("SetExperimentActivityForTest %q: %w", name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("SetExperimentActivityForTest %q: %w", name, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %q", ErrNoSuchExperiment, name)
	}
	return nil
}
