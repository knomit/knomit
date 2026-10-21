package merge

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"knomit/internal/store/internal/sqlite"
)

// Test seams for the parent's white-box tests, which reach into the merge
// cluster's unexported cores. Each panics outside a test binary
// (testing.Testing), so no production path can use the door.

func mustBeTest(name string) {
	if !testing.Testing() {
		panic("merge." + name + " called outside a test binary")
	}
}

// MergeIntoBranchForTest is mergeIntoBranch: it takes the dst lock and merges
// src into dst with the strategy.
func (rh *Repo) MergeIntoBranchForTest(ctx context.Context, src, dst string, strategy ConflictStrategy) (AgentReconcileResult, error) {
	mustBeTest("MergeIntoBranchForTest")
	return rh.mergeIntoBranch(ctx, src, dst, strategy)
}

// MergeIntoBranchResolvedForTest is mergeIntoBranchResolved: the dst lock plus
// per-path resolutions.
func (rh *Repo) MergeIntoBranchResolvedForTest(ctx context.Context, src, dst string, strategy ConflictStrategy, resolutions map[string]Resolution) (AgentReconcileResult, error) {
	mustBeTest("MergeIntoBranchResolvedForTest")
	return rh.mergeIntoBranchResolved(ctx, src, dst, strategy, resolutions)
}

// SetOpenExperimentForkedHookForTest installs f to run in OpenExperiment
// between the fork and the experiments row, inside the parent's lock; nil
// removes it.
func SetOpenExperimentForkedHookForTest(f func()) {
	mustBeTest("SetOpenExperimentForkedHookForTest")
	testHookOpenExperimentForked = f
}

// ConflictsStrategyForTest is the strategy value a merge site builds from a
// `conflicts` setting whose facts and state keys are the given values.
func ConflictsStrategyForTest(facts, state string) ConflictStrategy {
	mustBeTest("ConflictsStrategyForTest")
	return ConflictsPolicy{facts: facts, state: state}.strategy()
}

// ExperimentRewoundRefNameForTest is the bookkeeping ref a rewind records an
// experiment's old agent tip under.
func ExperimentRewoundRefNameForTest(name string) plumbing.ReferenceName {
	mustBeTest("ExperimentRewoundRefNameForTest")
	return experimentRewoundRefName(name)
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
// NotifyCommit, which is what keeps "activity" meaning exactly "a commit
// landed here".
func (rh *Repo) SetExperimentActivityForTest(ctx context.Context, name string, when time.Time) error {
	mustBeTest("SetExperimentActivityForTest")
	res, err := sqlite.Conn(ctx, rh.vr.DB()).ExecContext(ctx,
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
