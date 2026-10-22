package facts

import "knomit/internal/engine/store/internal/vcs"

// Repo is the facts cluster's view of the git layer: a named handle on the
// vcs.Repo (vr, never embedded, so none of its methods is promoted onto this
// type or onto whatever holds it) plus the methods that read and write the repo's fact-adjacent trees (system files,
// templates, skills) and the trigger dispatcher's rows. The parent holds it as
// the named Service field fr, and Service.FactsRepo hands it out.
type Repo struct {
	vr *vcs.Repo
}

// NewRepo wraps the vcs handle. The wrapper holds no state of its own; the
// vcs.Repo pointer is shared with the parent's other handles, so setters called
// on any of them configure the same handle.
func NewRepo(r *vcs.Repo) *Repo { return &Repo{vr: r} }
