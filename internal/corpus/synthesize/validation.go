package synthesize

import (
	"fmt"
	"path"
	"strings"

	"knomit/internal/domain/fact"

	"github.com/google/uuid"
)

// validPruneActions is the set of allowed decision actions.
var validPruneActions = map[string]bool{
	"keep": true, "retract": true, "update": true,
}

// validatePrunePaths checks that all decision paths and merge source paths
// reference facts that were actually sent to the LLM, that actions are valid,
// and that merges are well-formed. Returns an error on the first violation.
func validatePrunePaths(r PruneResult, inputPaths []string) error {
	valid := make(map[string]bool, len(inputPaths))
	for _, p := range inputPaths {
		valid[p] = true
	}
	for _, d := range r.Decisions {
		if !valid[d.Path] {
			return fmt.Errorf("decision references unknown path %q", d.Path)
		}
		if !validPruneActions[d.Action] {
			return fmt.Errorf("decision has unknown action %q for path %q", d.Action, d.Path)
		}
	}
	for _, m := range r.Merges {
		if len(m.Paths) == 0 {
			return fmt.Errorf("merge has empty source paths")
		}
		// The same rule SerializeFact applies, checked before the item is
		// applied so a blank or multi-line model title fails here.
		if err := fact.ValidateTitle(m.Merged.Title); err != nil {
			return fmt.Errorf("merge title: %w", err)
		}
		for _, p := range m.Paths {
			if !valid[p] {
				return fmt.Errorf("merge references unknown source path %q", p)
			}
		}
	}
	return nil
}

// validateDistillPaths checks that all retract paths reference facts that were
// actually sent to the LLM.
func validateDistillPaths(r DistillResult, inputPaths []string) error {
	valid := make(map[string]bool, len(inputPaths))
	for _, p := range inputPaths {
		valid[p] = true
	}
	for _, p := range r.Retract {
		if !valid[p] {
			return fmt.Errorf("retract references unknown path %q", p)
		}
	}
	return nil
}

// validateReflectResponse runs the structural checks for a reflect step.
// DB-backed checks (reinforce path resolves to type=methodology; propose
// novelty against existing methodology embeddings; topic_path under the
// configured ontology root) live in ApplyReflectDecisions because they
// need the store and embedder.
//
// transitionPaths is the set of paths the session recorded transitions
// for; reinforce/propose may only reference paths in this set. proposeCap
// is the maximum number of new methodologies the agent may propose in one
// reflect — defaults to 1 for the first revision; configurable via env.
func validateReflectResponse(r ReflectResult, transitionPaths []string, proposeCap int) error {
	if proposeCap < 0 {
		proposeCap = 0
	}
	if len(r.Propose) > proposeCap {
		return fmt.Errorf("propose cap exceeded: got %d, max %d", len(r.Propose), proposeCap)
	}

	allowed := make(map[string]bool, len(transitionPaths))
	for _, p := range transitionPaths {
		allowed[p] = true
	}

	for i, e := range r.Reinforce {
		if e.MethodologyPath == "" {
			return fmt.Errorf("reinforce[%d]: methodology_path is required", i)
		}
		if len(e.TransitionPaths) == 0 {
			return fmt.Errorf("reinforce[%d]: transition_paths must be non-empty", i)
		}
		for _, tp := range e.TransitionPaths {
			if !allowed[tp] {
				return fmt.Errorf("reinforce[%d]: transition path %q not in this session's transitions", i, tp)
			}
		}
	}

	for i, p := range r.Propose {
		// Checked here, before the item is claimed: refused later by
		// SerializeFact, the whole item — valid reinforcements included —
		// would be consumed with nothing applied.
		if err := fact.ValidateTitle(p.Title); err != nil {
			return fmt.Errorf("propose[%d]: %w", i, err)
		}
		if p.Body == "" {
			return fmt.Errorf("propose[%d]: body is required", i)
		}
		if p.TopicPath == "" {
			return fmt.Errorf("propose[%d]: topic_path is required", i)
		}
		if p.NoveltyArgument == "" {
			return fmt.Errorf("propose[%d]: novelty_argument is required (justify why no existing methodology fits)", i)
		}
		if len(p.TransitionPaths) == 0 {
			return fmt.Errorf("propose[%d]: transition_paths must be non-empty", i)
		}
		if p.Confidence < 0 || p.Confidence > 1 {
			return fmt.Errorf("propose[%d]: confidence %.2f outside [0, 1]", i, p.Confidence)
		}
		for _, tp := range p.TransitionPaths {
			if !allowed[tp] {
				return fmt.Errorf("propose[%d]: transition path %q not in this session's transitions", i, tp)
			}
		}
	}

	return nil
}

// normalizeFactPath replaces the filename component of a path with an 8-char
// UUID, matching the convention used by learn. This prevents LLM-generated
// filenames (e.g. "chrome-extension-threat-surface-2026.md") from clashing
// with each other or with learn-generated facts. Directory case is normalised
// by fact.NewFact, which lowercases the full path unconditionally.
//
// path.Dir, NOT filepath.Dir. A fact path is a git-style repo path: always
// forward-slashed, on every OS, because it names a blob in a tree rather than
// a file on this machine. filepath.Dir answers in the host separator, so on
// Windows "kb/synthesis/x.md" came back as "kb\synthesis" and this returned
// the mongrel "kb\synthesis/<uuid>.md". validateOutputPath then looked for the
// "kb/" prefix, did not find it, and every merge, distill, propose and
// emergent-fact write was rejected — silently, as a warning on the progress
// channel rather than an error, so synthesis simply produced nothing.
func normalizeFactPath(p string) string {
	id := uuid.New().String()[:8]
	return path.Dir(p) + "/" + id + ".md"
}

// validateOutputPath rejects LLM-emitted fact paths that don't live under
// the configured ontology root. Returns nil on success or a descriptive
// error suitable for the onProgress warn channel. Paths are compared
// case-insensitively because fact.NewFact lowercases all paths
// unconditionally before persisting.
func validateOutputPath(path, ontologyRoot string) error {
	root := strings.TrimRight(ontologyRoot, "/")
	if root == "" {
		return fmt.Errorf("ontology root not configured")
	}
	prefix := strings.ToLower(root) + "/"
	if !strings.HasPrefix(strings.ToLower(path), prefix) {
		return fmt.Errorf("path %q is outside ontology root %q", path, ontologyRoot)
	}
	// A dot-prefixed segment is private: it would be written and then skipped
	// by the indexer, Verify and the exporter alike, so it is refused here
	// rather than silently discarded. All four of this function's callers
	// (merge, distill, propose, and discovery's emergent-fact write) land on
	// this one rule.
	if fact.IsPrivatePath(path) {
		return fmt.Errorf("%s is private: a path segment beginning with '.' cannot hold a fact", path)
	}
	return nil
}

// validateOutputType rejects LLM-emitted facts with empty or invalid
// epistemic type. Returns nil on success. Empty type is the most common
// failure (LLM omits the field) and historically slipped through to disk
// as `type:` blanks; the API silently masked these by defaulting to
// "observation" on read, hiding the bug.
func validateOutputType(t string) error {
	if t == "" {
		return fmt.Errorf("epistemic type is empty")
	}
	if !fact.Epistemic.AllowsType(fact.Type(t)) {
		return fmt.Errorf("invalid epistemic type %q", t)
	}
	return nil
}
