package store

import (
	"context"
	"database/sql"

	"knomit/internal/embeddings/params"
	"knomit/internal/store/facts"
	storegit "knomit/internal/store/git"
	"knomit/internal/store/internal/vcs"
	"knomit/internal/store/merge"
	storesync "knomit/internal/store/sync"
	"knomit/internal/store/vocab"
)

// This file is the parent's seam to the vcs child (internal/vcs): the git
// layer every cluster above it reaches UP to — branches, commits, the branch
// locks, signing, trailers, the commit log and path history.
//
// repoHandler EMBEDS *vcs.Repo, so the sub-indices keep their `rh
// *repoHandler` back-reference and call the child's exported methods by
// promotion (rh.LockBranch, rh.NotifyCommit, rh.DB(), ...); the methods that
// belong to higher clusters (merge, reconcile, triggers, verify, gitserve)
// stay declared on repoHandler here, in the parent. Go embedding is not
// virtual: the child never calls a method declared up here, so no ordering
// on the commit path depends on this wrapper — NotifyCommit, the branch
// locks and commit authoring run entirely inside the child, as before.
//
// The names callers outside store use are re-exported below unchanged, so
// the public surface of package store does not move.
type repoHandler struct {
	*vcs.Repo
	*factsRepo // the facts child's methods on the same handle; see facts_seam.go
	*mergeRepo // the merge child's methods on the same handle; see merge_seam.go
	*syncRepo  // the sync child's methods on the same handle; see sync_seam.go
}

func newRepoHandler(db *sql.DB, gits *storegit.Storer) *repoHandler {
	r := vcs.New(db, gits)
	// DropBranchLocked deletes a branch's rows in the tables without an ON
	// DELETE CASCADE between its branch_facts and branches deletes; the git
	// layer does not know those tables, the vocab child owns them. Registered
	// here, where the Repo is built, so no repoHandler exists without it (a
	// Repo without it refuses every drop).
	r.SetOnDropBranchRows(func(ctx context.Context, branchID int64) error {
		return vocab.DropBranchReviewState(ctx, db, branchID)
	})
	mr := merge.NewRepo(r)
	return &repoHandler{Repo: r, factsRepo: facts.NewRepo(r), mergeRepo: mr, syncRepo: storesync.NewRepo(mr)}
}

// Types.
type (
	Accept          = vcs.Accept
	AcceptList      = vcs.AcceptList
	BatchEmbedder   = vcs.BatchEmbedder
	Branch          = vcs.Branch
	ChangesQuery    = vcs.ChangesQuery
	ChangesResult   = vcs.ChangesResult
	ChangesScope    = vcs.ChangesScope
	Embedder        = vcs.Embedder
	Experiment      = vcs.Experiment
	IndexManager    = vcs.IndexManager
	PathChange      = vcs.PathChange
	RebuildProgress = vcs.RebuildProgress
	RevisionDiff    = vcs.RevisionDiff
	RevisionMeta    = vcs.RevisionMeta
	TrailerEntry    = vcs.TrailerEntry
	Trailers        = vcs.Trailers
)

// Constants.
const (
	ChangeAdded           = vcs.ChangeAdded
	ChangeDeleted         = vcs.ChangeDeleted
	ChangeModified        = vcs.ChangeModified
	ExperimentPrefix      = vcs.ExperimentPrefix
	TrailerCause          = vcs.TrailerCause
	TrailerRun            = vcs.TrailerRun
	TrailerTemplate       = vcs.TrailerTemplate
	TrailerTemplateSource = vcs.TrailerTemplateSource
	TrailerTrace          = vcs.TrailerTrace
	TrailerTrigger        = vcs.TrailerTrigger
)

// Sentinel errors: the same values, so errors.Is matches across the seam.
var (
	ErrBranchNotFound        = vcs.ErrBranchNotFound
	ErrHistoryChanged        = vcs.ErrHistoryChanged
	ErrInvalidChangesCursor  = vcs.ErrInvalidChangesCursor
	ErrInvalidPrefix         = vcs.ErrInvalidPrefix
	ErrNoAgentID             = vcs.ErrNoAgentID
	ErrNoConsensusBranch     = vcs.ErrNoConsensusBranch
	ErrNoSigner              = vcs.ErrNoSigner
	ErrOrphanExperimentRef   = vcs.ErrOrphanExperimentRef
	ErrPathNotFound          = vcs.ErrPathNotFound
	ErrSinceNotBehind        = vcs.ErrSinceNotBehind
	ErrStaleExperimentParent = vcs.ErrStaleExperimentParent
	ErrTraceOverride         = vcs.ErrTraceOverride
	ErrTraceTrigger          = vcs.ErrTraceTrigger
	ErrUnknownSince          = vcs.ErrUnknownSince
)

// ExperimentBranch is vcs.ExperimentBranch.
func ExperimentBranch(name string) string { return vcs.ExperimentBranch(name) }

// IsExperimentBranch is vcs.IsExperimentBranch.
func IsExperimentBranch(branch string) bool { return vcs.IsExperimentBranch(branch) }

// ExperimentNameOf is vcs.ExperimentNameOf.
func ExperimentNameOf(branch string) (string, bool) { return vcs.ExperimentNameOf(branch) }

// IsRoleBranch is vcs.IsRoleBranch.
func IsRoleBranch(branch string) bool { return vcs.IsRoleBranch(branch) }

// ChooseConsensusBranch is vcs.ChooseConsensusBranch.
func ChooseConsensusBranch(requested, head string, branches []string) string {
	return vcs.ChooseConsensusBranch(requested, head, branches)
}

// NormalizeChangesPrefix is vcs.NormalizeChangesPrefix.
func NormalizeChangesPrefix(ontologyRoot, prefix string) (string, error) {
	return vcs.NormalizeChangesPrefix(ontologyRoot, prefix)
}

// EncodeChangesCursor is vcs.EncodeChangesCursor.
func EncodeChangesCursor(scope ChangesScope, since, head, prefix, after string) string {
	return vcs.EncodeChangesCursor(scope, since, head, prefix, after)
}

// DecodeChangesCursor is vcs.DecodeChangesCursor.
func DecodeChangesCursor(s string) (ChangesQuery, ChangesScope, error) {
	return vcs.DecodeChangesCursor(s)
}

// ChangesBookmark is vcs.ChangesBookmark.
func ChangesBookmark(repo12, head string) string { return vcs.ChangesBookmark(repo12, head) }

// ParseChangesSince is vcs.ParseChangesSince.
func ParseChangesSince(since string) (repo, commit string, err error) {
	return vcs.ParseChangesSince(since)
}

// EmbedderThresholds is vcs.EmbedderThresholds.
func EmbedderThresholds(emb Embedder) params.Thresholds { return vcs.EmbedderThresholds(emb) }

// TrailerValue is vcs.TrailerValue.
func TrailerValue(message, key string) string { return vcs.TrailerValue(message, key) }

// WithTrailers is vcs.WithTrailers.
func WithTrailers(ctx context.Context, t Trailers) context.Context { return vcs.WithTrailers(ctx, t) }

// WithAgentTrace is vcs.WithAgentTrace.
func WithAgentTrace(ctx context.Context, t Trailers) (context.Context, error) {
	return vcs.WithAgentTrace(ctx, t)
}

// TestFallbackAgentID is vcs.TestFallbackAgentID (the read side; it returns
// "" outside a test binary).
func TestFallbackAgentID(branch string) string { return vcs.TestFallbackAgentID(branch) }
