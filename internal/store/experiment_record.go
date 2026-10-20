package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ExperimentPrefix is the ref namespace every experiment branch lives under.
// Deliberately NOT `agent/…`: the /git advertisement is an allowlist of the
// upstream plus `refs/heads/agent/`, so a name outside it is unreachable from
// the network by construction rather than by a second rule that could drift
// (kb/decisions/repos/git-serve/curated-advertisement).
const ExperimentPrefix = "exp/"

// ErrStaleExperimentParent is returned by commit/sync when the
// experiment's recorded parent is no longer the agent branch this
// database records as its owner — the repo was taken over between the
// fork and now. Rollback is still allowed: throwing away orphaned work is
// always safe, and it is the only way out of the state.
var ErrStaleExperimentParent = errors.New("experiment's parent is no longer this database's agent branch")

// ErrOrphanExperimentRef is returned when an exp/<name> ref exists with no
// experiments row behind it. Refused rather than adopted: the ref's
// content is unknown, and its parentage is unknowable (that is what the
// missing row means), so adopting it would present someone else's commits
// as a fresh fork of the agent branch. RollbackExperiment clears it — the
// refusal is only reasonable because the recovery needs no shell.
var ErrOrphanExperimentRef = errors.New("an experiment branch with no record already exists")

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
