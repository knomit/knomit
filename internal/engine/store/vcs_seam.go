package store

import (
	"context"
	"database/sql"

	storegit "knomit/internal/engine/store/git"
	"knomit/internal/engine/store/internal/vcs"
	"knomit/internal/engine/store/vocab"
)

// This file is the parent's seam to the vcs child (internal/vcs): the git
// layer every cluster above it reaches UP to — branches, commits, the branch
// locks, signing, trailers, the commit log and path history.
//
// repoHandler EMBEDS *vcs.Repo (one of the embeds allowlisted in
// TestStoreTypes_DoNotEmbedStorePackageTypes, with the gitserve host's own
// *vcs.Repo and index.SearchResult's facts.FactWithBody), so the parent calls the child's
// exported methods by promotion (rh.LockBranch, rh.NotifyCommit, rh.DB(), ...).
// The facts, merge and remotesync children are NOT embedded: Service holds
// them as the named fields fr, mr and rr, so a call to one of their methods
// names its child. repoHandler declares no method of its own in non-test code;
// NotifyCommit, the branch locks and commit authoring run entirely inside the
// vcs child.
//
// The plain data types and sentinels callers outside store use live in
// store/model, which vcs and the callers import directly; nothing is
// re-exported here.

type repoHandler struct {
	*vcs.Repo
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
	return &repoHandler{Repo: r}
}
