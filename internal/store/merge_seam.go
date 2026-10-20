package store

import (
	"knomit/internal/store/merge"
)

// This file is the parent's seam to the merge child (internal/store/merge):
// branch merges and the conflict strategies, the pushed-branch and consensus
// merge cores, and the experiment lifecycle (open, commit, sync, rollback,
// expire, the rewound-parent rebuild). The child reads a store only through
// *vcs.Repo (and verify for the merge-time validation); the callers that
// drive it stay in this package: Service.MergePushed / MergeConsensus
// (service_merge.go, which hold the dst lock and pick the host's strategy),
// the reconcile and sync paths live in internal/store/remotesync and the
// receive-pack handler in internal/store/gitserve, which reaches merge only
// for the Mode constants a branch move reports.
//
// repoHandler embeds *merge.Repo next to *vcs.Repo and *facts.Repo (see
// vcs_seam.go), so the methods that used to be declared on repoHandler in the
// merge files (MergeBranch, OpenExperiment, CommitExperiment, ...) are still
// reached as rh.<Name> by promotion, and the BranchIndex / ExperimentIndex
// guards hold for *repoHandler as before. Embedding is not virtual: the child
// never calls a method declared on the parent's repoHandler, so the lock
// order (dst lock -> NotifyCommit) and the tx scope of every merge are the
// ones the code had when it lived here.
//
// The names callers outside store use are re-exported below unchanged, so
// the public surface of package store does not move. Names only the parent's
// own files need (merge.MergeOpts, merge.FactMerge, merge.ConflictsPolicyOf,
// merge.AppendTrailerLines) are NOT re-exported; the parent spells them with
// the package qualifier.

// mergeRepo is the embedded merge.Repo; the alias gives the embedded field a
// name that does not collide with the embedded *vcs.Repo's `Repo`.
type mergeRepo = merge.Repo

// Types.
type (
	AgentReconcileResult  = merge.AgentReconcileResult
	BranchMovedError      = merge.BranchMovedError
	ConflictStrategy      = merge.ConflictStrategy
	MergeConflictError    = merge.MergeConflictError
	Mode                  = merge.Mode
	PrivatePathMergeError = merge.PrivatePathMergeError
	PushedBranchInfo      = merge.PushedBranchInfo
	Resolution            = merge.Resolution
	ResolutionSide        = merge.ResolutionSide
	SettledPath           = merge.SettledPath
)

// Constants.
const (
	MaxExperimentNameLen = merge.MaxExperimentNameLen
	ModeFF               = merge.ModeFF
	ModeMerge            = merge.ModeMerge
	ModeNoop             = merge.ModeNoop
	ModeRebase           = merge.ModeRebase
	ModeRewound          = merge.ModeRewound
	ResolveDst           = merge.ResolveDst
	ResolveSrc           = merge.ResolveSrc
	StrategyLocalWins    = merge.StrategyLocalWins
	StrategyRefuse       = merge.StrategyRefuse
	StrategyRemoteWins   = merge.StrategyRemoteWins
	TrailerConflict      = merge.TrailerConflict
	TrailerMerge         = merge.TrailerMerge
)

// Sentinel errors: the same values, so errors.Is matches across the seam.
var (
	ErrBranchMoved           = merge.ErrBranchMoved
	ErrInvalidExperimentName = merge.ErrInvalidExperimentName
	ErrNoSuchExperiment      = merge.ErrNoSuchExperiment
	ErrPrivatePathMerge      = merge.ErrPrivatePathMerge
	ErrUnrelatedHistories    = merge.ErrUnrelatedHistories
)

// TrailerValues returns every value of the trailer key in a commit message.
func TrailerValues(message, key string) []string { return merge.TrailerValues(message, key) }
