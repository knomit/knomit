package synthesize

import "fmt"

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
		if m.Merged.Title == "" {
			return fmt.Errorf("merge has empty title")
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
		if p.Title == "" {
			return fmt.Errorf("propose[%d]: title is required", i)
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
