package store

import (
	"knomit/internal/store/internal/sessions"
)

// Compile-time interface guards. The published interfaces (interfaces.go)
// are the facade's contract; the implementations that satisfy them are checked
// here so an implementation never has to name the facade's interfaces itself.
//
// The query services (index.FactQuery and its siblings) and the vocab indexes
// (vocab.MotifIndex, vocab.AbstractionIndex) are handed out as their concrete
// types, so they have no interface and no guard.

var _ BranchIndex = (*repoHandler)(nil)

var _ PipelineIndex = (*sessions.PipelineIndex)(nil)
var _ ToolSessionIndex = (*sessions.ToolIndex)(nil)
