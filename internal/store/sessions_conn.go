package store

import (
	"knomit/internal/store/internal/sessions"
)

// This file is the parent's seam to the sessions child
// (internal/store/internal/sessions). The child owns the ephemeral session DB
// (schema, open, reap), the pipeline work-stealing index (plus its watermarks
// in the main DB) and the tool-session paging index. This package keeps the
// accessors (Service.Pipeline, Service.ToolSession), the published interfaces
// and the Service wiring, and re-exports the names callers outside store use
// so the public surface is unchanged.

// PipelineSession, PipelineWorkItem and PipelineSessionStats are the pipeline
// row shapes (see the sessions child).
type (
	PipelineSession      = sessions.PipelineSession
	PipelineWorkItem     = sessions.PipelineWorkItem
	PipelineSessionStats = sessions.PipelineSessionStats
	// ToolSession and QueueItem are the tool-session paging row shapes.
	ToolSession = sessions.ToolSession
	QueueItem   = sessions.QueueItem
)

// ErrPipelineSlotChanged is returned by CreatePipelineSessionReplacing when
// the active session for the tool+branch is not the one the caller decided to
// replace. Nothing was written; the caller re-reads the slot and decides again.
var ErrPipelineSlotChanged = sessions.ErrPipelineSlotChanged

// SessionDBSuffix is the filename suffix of a Service's ephemeral session
// database ("<name>.sessions.db").
const SessionDBSuffix = sessions.DBSuffix

// IsSessionDBFile reports whether base names a session sidecar database.
func IsSessionDBFile(base string) bool { return sessions.IsDBFile(base) }

// SessionDBPathFor derives the ephemeral session DB path from the main DB path.
// Exported so callers that need to locate a repo's session sidecar without
// opening it (repos.Manager.Archive) derive the same path this package uses.
func SessionDBPathFor(mainPath string) string { return sessions.PathFor(mainPath) }
