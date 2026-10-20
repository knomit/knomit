package store

// Compile-time interface guards. The published interfaces (interfaces.go)
// are the facade's contract; the sub-indices that implement them are checked
// here so a sub-index never has to name the facade's interfaces itself.

var _ AbstractionIndex = (*abstractionIndex)(nil)
var _ BranchIndex = (*repoHandler)(nil)
var _ ExperimentIndex = (*repoHandler)(nil)
var _ FactIndex = (*factIndex)(nil)
var _ PipelineIndex = (*pipelineIndex)(nil)
var _ RemoteIndex = (*remoteIndex)(nil)
var _ ToolSessionIndex = (*toolIndex)(nil)

var (
	_ FactQuery          = (*factQuery)(nil)
	_ GraphStore         = (*graphStore)(nil)
	_ HistoryQuery       = (*historyQuery)(nil)
	_ MethodologyMatcher = (*methodologyMatcher)(nil)
	_ SearchIndex        = (*searchFacade)(nil)
)
