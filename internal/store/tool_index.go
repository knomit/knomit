package store

type toolIndex struct {
	rh *repoHandler
}

var _ ToolSessionIndex = (*toolIndex)(nil)
