package vcs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"knomit/internal/store/internal/sqlite"
	"knomit/internal/store/model"
)

// GetExperiment reads one record. The bool distinguishes "no such experiment"
// from an error, so eligibility can answer "not writable" without treating a
// DB failure as an answer.
func (rh *Repo) GetExperiment(ctx context.Context, name string) (model.Experiment, bool, error) {
	var (
		e          model.Experiment
		created    int64
		lastActive int64
	)
	err := sqlite.Conn(ctx, rh.db).QueryRowContext(ctx, `
		SELECT name, description, parent_branch, fork_commit, created_at, last_activity_at
		FROM experiments WHERE name = ?`, name,
	).Scan(&e.Name, &e.Description, &e.Parent, &e.ForkCommit, &created, &lastActive)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Experiment{}, false, nil
	}
	if err != nil {
		return model.Experiment{}, false, fmt.Errorf("GetExperiment %q: %w", name, err)
	}
	e.CreatedAt = time.Unix(created, 0)
	e.LastActivityAt = time.Unix(lastActive, 0)
	return e, true, nil
}

// CheckExperimentParentCurrent refuses an experiment whose recorded parent is
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
// would refuse every commit on such a database. Same guard fsck/integrity.go applies
// to the same value. So the check bites only on a KNOWN, different owner.
func (rh *Repo) CheckExperimentParentCurrent(ctx context.Context, exp model.Experiment) error {
	owner, err := rh.AgentBranchOwner(ctx)
	if err != nil {
		return fmt.Errorf("experiment %q: read agent branch owner: %w", exp.Name, err)
	}
	if owner == "" || owner == exp.Parent {
		return nil
	}
	return fmt.Errorf("%w: experiment %q was forked from %q, but this database is now written by %q "+
		"(roll it back, or re-open it from the current branch)",
		model.ErrStaleExperimentParent, exp.Name, exp.Parent, owner)
}

// touchExperimentActivity records that a commit landed on branch, when branch
// is an experiment. Called from NotifyCommit, the single chokepoint every ref
// mutation goes through, so "activity" means exactly "a commit happened here"
// with no second definition to keep in sync.
//
// It runs on EVERY branch's every commit, so the shape test comes first and
// costs a prefix compare; only an experiment branch reaches SQL. A branch with
// no row updates nothing, which is the correct answer for a hand-made `exp/…`
// ref nobody opened.
func (rh *Repo) touchExperimentActivity(ctx context.Context, branch string) error {
	if !model.IsExperimentBranch(branch) {
		return nil
	}
	name, _ := model.ExperimentNameOf(branch)
	if _, err := sqlite.Conn(ctx, rh.db).ExecContext(ctx,
		`UPDATE experiments SET last_activity_at = ? WHERE name = ?`,
		time.Now().Unix(), name); err != nil {
		return fmt.Errorf("touch experiment %q: %w", name, err)
	}
	return nil
}
