package synthesize

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

// isDistillPassive returns true if the distill result produced no new insights:
// empty synthesize array, or all synthesized paths match input paths with no forgets.
func isDistillPassive(r DistillResult, inputPaths []string) bool {
	if len(r.Synthesize) == 0 {
		return true
	}
	if len(r.Forget) > 0 {
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
