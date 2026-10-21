package remotesync

import (
	"knomit/internal/store/internal/vcs"
	"knomit/internal/store/merge"
)

// Repo is the sync cluster's view of the git layer: the merge handle (which
// itself wraps the one shared git-layer handle) plus the methods that
// reconcile a local store with its origin: the fast-forward of the consensus
// branch, the merge and rebase reconcile of the agent branch, the watermark,
// the origin identity, peer admission, the publish-time own-range judge and
// the unpublished-commit drop. The parent's repoHandler embeds it next to the
// git-layer, facts and merge handles, so callers keep writing
// rh.AdvanceBranchTo, rh.PromotePeer, ... by promotion. The git-layer handle
// is the named field vr, never embedded, so none of its methods is promoted
// onto this type. Embedding is not virtual: nothing in here calls a method
// declared on the parent's repoHandler.
type Repo struct {
	*merge.Repo
	vr *vcs.Repo
}

// NewRepo wraps the merge handle and the git-layer handle it was built on. The
// wrapper holds no state of its own; both handles are shared with the parent's
// repoHandler, so setters called on either side configure the same handle.
func NewRepo(m *merge.Repo, v *vcs.Repo) *Repo { return &Repo{Repo: m, vr: v} }
