package model

import (
	"errors"
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
