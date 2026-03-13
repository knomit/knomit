package synthesize

import "fmt"

// isPrunePassive returns true if the prune result made no meaningful changes:
// all decisions are "keep" (or empty) and no merges proposed.
func isPrunePassive(r PruneResult) bool {
	if len(r.Merges) > 0 {
		return false
	}
	for _, d := range r.Decisions {
		if d.Action != "keep" {
			return false
		}
	}
	return true
}

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

// isDistillPassive returns true if the distill result produced no new insights:
// empty synthesize array, or all synthesized paths match input paths with no forgets.
func isDistillPassive(r DistillResult, inputPaths []string) bool {
	if len(r.Synthesize) == 0 {
		return true
	}
	if len(r.Retract) > 0 {
		return false
	}
	// Check if all synthesized paths are just echoing input paths
	inputSet := make(map[string]bool, len(inputPaths))
	for _, p := range inputPaths {
		inputSet[p] = true
	}
	for _, s := range r.Synthesize {
		if !inputSet[s.Path] {
			return false
		}
	}
	return true
}
