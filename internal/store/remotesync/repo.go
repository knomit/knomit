package remotesync

import (
	"knomit/internal/store/internal/vcs"
	"knomit/internal/store/merge"
)

// Repo is the sync cluster's view of the git layer: the merge handle (mr, which
// itself wraps the one shared git-layer handle) plus the methods that
// reconcile a local store with its origin: the fast-forward of the consensus
// branch, the merge and rebase reconcile of the agent branch, the watermark,
// the origin identity, peer admission, the publish-time own-range judge and
// the unpublished-commit drop. The parent holds it as the named Service field
// rr. The merge handle (mr) and the git-layer handle (vr) are named fields,
// never embedded, so none of their methods is promoted onto this type.
type Repo struct {
	mr *merge.Repo
	vr *vcs.Repo
}

// NewRepo wraps the merge handle and the git-layer handle it was built on. The
// wrapper holds no state of its own; both handles are shared with the parent's
// parent's other handles, so setters called on any of them configure the same
// handle.
func NewRepo(m *merge.Repo, v *vcs.Repo) *Repo { return &Repo{mr: m, vr: v} }
