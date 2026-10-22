// Package merge is the store's merge cluster: branch merges and the conflict
// strategies, the pushed-branch and consensus merge doors, and the experiment
// lifecycle (open, commit, sync, rollback, expire, the rewound-parent
// rebuild). It sits above vcs and verify in the store's child order and reads
// a store only through *vcs.Repo; the sync and gitserve clusters and the
// facade reach up to it, never the reverse.
package merge

import "knomit/internal/engine/store/internal/vcs"

// Repo is the merge cluster's view of the git layer: a named handle on the
// vcs.Repo (vr, never embedded, so none of its methods is promoted onto this
// type or onto whatever holds it) plus the merge and experiment methods.
// Service.Merge hands this type out to callers outside the store, which is why
// it must stay free of vcs methods; the parent holds it as the named Service
// field mr for its own calls.
type Repo struct {
	vr *vcs.Repo
}

// NewRepo wraps the vcs handle. The wrapper holds no state of its own; the
// vcs.Repo pointer is shared with the parent's other handles, so setters called
// on any of them configure the same handle.
func NewRepo(r *vcs.Repo) *Repo { return &Repo{vr: r} }
