package store

import (
	"context"
	"database/sql"

	"knomit/internal/store/facts"
	storegit "knomit/internal/store/git"
	"knomit/internal/store/internal/vcs"
	"knomit/internal/store/merge"
	"knomit/internal/store/remotesync"
	"knomit/internal/store/vocab"
)

// factsRepo is the embedded facts.Repo; the alias gives the embedded field a
// name that does not collide with the embedded *vcs.Repo's `Repo`. Unexported
// parent naming, not a re-export; E7 replaces the embed with a named field.
type factsRepo = facts.Repo

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
// The plain data types and sentinels callers outside store use live in
// store/model, which vcs and the callers import directly; nothing is
// re-exported here but TestFallbackAgentID, below.

// mergeRepo names the embedded merge.Repo field (the alias keeps it apart from
// the embedded *vcs.Repo's `Repo`). A parent-private name, not an export.
type mergeRepo = merge.Repo

// syncRepo names the embedded remotesync.Repo field, for the same reason as
// mergeRepo. A parent-private name, not an export; E7 replaces the embed with
// a named field.
type syncRepo = remotesync.Repo

type repoHandler struct {
	*vcs.Repo
	*factsRepo // the facts child's methods on the same handle; see the factsRepo alias above
	*mergeRepo // the merge child's methods on the same handle; Service.Merge hands it out
	*syncRepo  // the remotesync child's methods on the same handle; Service.Remote hands out its Index
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
	return &repoHandler{Repo: r, factsRepo: facts.NewRepo(r), mergeRepo: mr, syncRepo: remotesync.NewRepo(mr, r)}
}

// TestFallbackAgentID is vcs.TestFallbackAgentID (the read side; it returns
// "" outside a test binary). It stays a forward because the installed fallback
// is vcs package state, which model (a leaf) cannot hold; repos.Manager and
// web tests call it.
func TestFallbackAgentID(branch string) string { return vcs.TestFallbackAgentID(branch) }
