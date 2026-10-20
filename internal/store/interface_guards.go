package store

import "knomit/internal/store/internal/sessions"

// Compile-time interface guards. The published interfaces (interfaces.go)
// are the facade's contract; the sub-indices that implement them are checked
// here so a sub-index never has to name the facade's interfaces itself.
//
// AbstractionIndex is deliberately NOT guarded here: it is review-pipeline-only
// state, and TestConformance_ReviewPipelineOnly (internal/synthesize) scans
// every store file except a short allowlist for any mention of it. Its guard
// sits beside the implementation in abstraction.go, a file the scan permits,
// so this file stays under the rule rather than being exempted from it.

var _ BranchIndex = (*repoHandler)(nil)
var _ ExperimentIndex = (*repoHandler)(nil)
var _ FactIndex = (*factIndex)(nil)
var _ PipelineIndex = (*sessions.PipelineIndex)(nil)
var _ RemoteIndex = (*remoteIndex)(nil)
var _ ToolSessionIndex = (*sessions.ToolIndex)(nil)

var (
	_ FactQuery          = (*factQuery)(nil)
	_ GraphStore         = (*graphStore)(nil)
	_ HistoryQuery       = (*historyQuery)(nil)
	_ MethodologyMatcher = (*methodologyMatcher)(nil)
	_ SearchIndex        = (*searchFacade)(nil)
)
