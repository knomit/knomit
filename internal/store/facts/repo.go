package facts

import "knomit/internal/store/internal/vcs"

// Repo is the facts cluster's view of the git layer: a named handle on the
// vcs.Repo (vr, never embedded, so none of its methods is promoted onto this
// type or onto whatever holds it) plus the methods that read and write the repo's fact-adjacent trees (system files,
// templates, skills) and the trigger dispatcher's rows. The parent's
// repoHandler embeds both *vcs.Repo and *Repo, so callers keep writing
// rh.SkillsAt, rh.TemplateFilesAt, rh.RecordTriggerRuns, ... by promotion;
// the vcs methods reach them through the repoHandler's own embed.
// Embedding is not virtual: nothing in here calls a method declared on the
// parent's repoHandler.
type Repo struct {
	vr *vcs.Repo
}

// NewRepo wraps the vcs handle. The wrapper holds no state of its own; the
// vcs.Repo pointer is shared with the parent's repoHandler, so setters called
// on either side configure the same handle.
func NewRepo(r *vcs.Repo) *Repo { return &Repo{vr: r} }
