package store

import "knomit/internal/store/index"

// Compile-time interface guards. The published interfaces (interfaces.go)
// are the facade's contract; the sub-indices that implement them are checked
// here so a sub-index never has to name the facade's interfaces itself.
//
// AbstractionIndex is deliberately NOT guarded here: it is review-pipeline-only
// state, and TestConformance_ReviewPipelineOnly (internal/synthesize) scans
// every store file except a short allowlist for any mention of it. Its guard
// sits beside the implementation in abstraction.go, a file the scan permits,
// so this file stays under the rule rather than being exempted from it.

var (
	_ FactQuery          = (*index.FactQuery)(nil)
	_ GraphStore         = (*index.GraphStore)(nil)
	_ HistoryQuery       = (*index.HistoryQuery)(nil)
	_ MethodologyMatcher = (*index.MethodologyMatcher)(nil)
	_ SearchIndex        = (*index.Facade)(nil)
)
