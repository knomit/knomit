package store

import (
	storesync "knomit/internal/store/sync"
)

// This file is the parent's seam to the sync child (internal/store/sync):
// the origin remote and its status row, the fetch, the reconcile of the
// consensus branch and of the agent branch (merge, and the rewind replay
// with its watermark), push and publish, the origin identity, peer admission
// and the publish-time judge of this instance's own commits. The child reads
// a store only through the merge handle (which wraps *vcs.Repo) and verify.
// The callers that drive it stay in this package: the Service methods in
// service_sync.go and consensus_branch.go, Open/Clone/Init (repo.go), the
// receive-pack handler, and replay.go.
//
// repoHandler embeds *storesync.Repo next to *vcs.Repo, *facts.Repo and
// *merge.Repo (see vcs_seam.go), so the methods that used to be declared on
// repoHandler in the remote_*.go files are reached as rh.<Name> by
// promotion. Embedding is not virtual: the child never calls a method
// declared on the parent's repoHandler, so the lock order, the transaction
// scope and the order inside Sync (the remote-tracking read before the
// fetch, the fetch, the watermark seed, the reconcile, the identity record)
// are the ones the code had when it lived here.
//
// The names callers outside store use are re-exported below unchanged, so
// the public surface of package store does not move.

// syncRepo is the embedded storesync.Repo; the alias gives the embedded field
// a name that does not collide with the embedded *vcs.Repo's `Repo`.
type syncRepo = storesync.Repo

// Types.
type (
	DropForeignError      = storesync.DropForeignError
	DropMergeError        = storesync.DropMergeError
	LeaveOutChangedError  = storesync.LeaveOutChangedError
	LostCommit            = storesync.LostCommit
	MainHistory           = storesync.MainHistory
	MainReconcileResult   = storesync.MainReconcileResult
	Origin                = storesync.Origin
	OwnCommitRefusedError = storesync.OwnCommitRefusedError
	PushResult            = storesync.PushResult
	Remote                = storesync.Remote
	SyncResult            = storesync.SyncResult
)

// Constants.
const (
	OwnCommitRefusedText = storesync.OwnCommitRefusedText
)

// Sentinel errors: the same values, so errors.Is matches across the seam.
var (
	ErrDropMerge        = storesync.ErrDropMerge
	ErrNoAgentBranch    = storesync.ErrNoAgentBranch
	ErrNotUnpublished   = storesync.ErrNotUnpublished
	ErrOwnCommitRefused = storesync.ErrOwnCommitRefused
	ErrUpstreamMoved    = storesync.ErrUpstreamMoved
)
