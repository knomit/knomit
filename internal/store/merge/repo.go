// Package merge is the store's merge cluster: branch merges and the conflict
// strategies, the pushed-branch and consensus merge doors, and the experiment
// lifecycle (open, commit, sync, rollback, expire, the rewound-parent
// rebuild). It sits above vcs and verify in the store's child order and reads
// a store only through *vcs.Repo; the sync and gitserve clusters and the
// facade reach up to it, never the reverse.
package merge

import "knomit/internal/store/internal/vcs"

// Repo is the merge cluster's view of the git layer: the vcs handle plus the
// merge and experiment methods. The parent's repoHandler embeds both
// *vcs.Repo and *Repo, so callers keep writing rh.MergeBranch,
// rh.OpenExperiment, ... by promotion. Embedding is not virtual: nothing in
// here calls a method declared on the parent's repoHandler.
type Repo struct {
	*vcs.Repo
}

// NewRepo wraps the vcs handle. The wrapper holds no state of its own; the
// vcs.Repo pointer is shared with the parent's repoHandler, so setters called
// on either side configure the same handle.
func NewRepo(r *vcs.Repo) *Repo { return &Repo{Repo: r} }
