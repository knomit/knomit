package model

import (
	_ "embed"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

// PipelineSession represents an active pipeline session for a tool on a branch.
//
// Status tracks lifecycle (active|completed|abandoned). Phase tracks
// workflow position inside an active session: "work" (prune/distill items
// being processed), "reflect" (single reflect item enqueued/being served),
// or "done" (all items including reflect answered, ready for completion).
// Phase is what makes the reviewer stateless across MCP calls — see
// AdvancePipelineSessionPhase for the CAS guarantee.
type PipelineSession struct {
	ID     string
	Tool   string
	Branch string
	Status string // "active", "completed", "abandoned"
	Phase  string // "work", "reflect", "done"
	Scoped bool   // true when session was started with a scope filter active
	Stats  PipelineSessionStats
	// Health carries corpus-health descriptor lines recorded while planning the
	// session. IN MEMORY ONLY — it is written and read inside the same
	// StartSession call, so it is deliberately not a column: nothing needs it
	// after the turn that produced it.
	Health []string
	// Abandoned is the id of the active session this one DISPLACED at creation,
	// if there was one, and AbandonedCreatedBy is that session's own
	// correlation handle. IN MEMORY ONLY, like Health, and set by
	// CreatePipelineSession alone — they describe one moment, not a stored
	// property of the row.
	//
	// Starting a session abandons any active session for the same tool+branch.
	// That is deliberate (one session per tool per branch) but it was silent on
	// both sides: the winner did not know it took the slot, and the loser found
	// out only when its next answer was rejected as "not active" — after it had
	// composed one (knomit#113 / #121 residue).
	Abandoned string
	// AbandonedCreatedBy carries the displaced session's created_by, read in
	// the same statement that finds it. The id alone tells the winner THAT it
	// took someone's slot; the handle tells it WHOSE, which is the answer a
	// resuming caller actually needs and the only one available from state
	// (knomit#123). Empty when nothing was displaced, and equally when the
	// displaced session was opened in-process and carried no handle.
	AbandonedCreatedBy string
	// CreatedBy is the correlation handle for whoever opened this session —
	// NOT an identity, NOT authentication. See the column comment in
	// session_schema.sql: over MCP the value derives from a client-supplied,
	// server-unverified session id, so it records what the opening call SAID,
	// not who the caller WAS. Empty for in-process callers (knomit#123).
	//
	// Unlike Scoped, this is known at insert time, so it rides the INSERT
	// rather than a follow-up UPDATE: a crash between two writes would leave
	// exactly the unattributable row this column exists to prevent.
	CreatedBy string
	// StartKey is what a later start must match to resume this session; see
	// the start_key column. Empty for an in-process start.
	StartKey string
	// Planning is set by every create and cleared once its start has planned
	// the work and rendered the first item (MarkPipelineSessionPlanned).
	Planning bool
	// Advancing is set by a won phase advance until its phase hook returns
	// (FinishPipelineSessionAdvance).
	Advancing bool
	CreatedAt string
	UpdatedAt string
	// LastUsedAt is the heartbeat the idle reaper and the resume window read:
	// bumped whenever the session serves a work item.
	LastUsedAt string
}

// ErrPipelineSlotChanged is returned by CreatePipelineSessionReplacing when
// the active session for the tool+branch is not the one the caller decided to
// replace: another start got there between the caller's read and its write.
// Nothing was written; the caller re-reads the slot and decides again.
var ErrPipelineSlotChanged = errors.New("the active pipeline session changed")

// PipelineSessionStats are the running totals of what a session's applied work
// items actually changed in the corpus.
//
// They live on the session row rather than on the engine because the engine is
// per-call stateless: the MCP handler constructs a fresh Reviewer for every
// continue call, so counters held on that struct would be discarded between
// items (see invariants/synthesize/per-call-objects-no-session-state).
//
// This is deliberately a store-owned struct rather than synthesize.ReviewStats:
// store cannot import synthesize (synthesize imports store), and the wire-facing
// ReviewStats belongs to the tool result, not to the DB row. Callers convert at
// the boundary — the two shapes are the same four counters by design.
type PipelineSessionStats struct {
	Pruned      int
	Merged      int
	Updated     int
	Synthesized int
}

// PipelineWorkItem represents a single work item within a pipeline session.
type PipelineWorkItem struct {
	ID         int64
	SessionID  string
	StepType   string // "prune", "distill", "reflect", "hypothesize", etc.
	ClusterKey string
	FactsJSON  string
	Response   *string // nil until answered
	Priority   float64
	Depth      int // RAPTOR depth level (0 = initial)
	CreatedAt  string
}

// SessionDBSuffix is the filename suffix of a Service's ephemeral session database,
// written as a sibling of the main DB at "<name>.sessions.db". Repo discovery
// uses IsSessionDBFile to skip these sidecars, which share the repos directory with
// real repo DBs.
const SessionDBSuffix = ".sessions.db"

// IsSessionDBFile reports whether base names a session sidecar database (a
// "<name>.sessions.db" file). Repo names can never contain a '.', so this
// suffix unambiguously identifies a session DB rather than a repo DB.
func IsSessionDBFile(base string) bool {
	return strings.HasSuffix(base, SessionDBSuffix)
}

// SessionDBPathFor derives the ephemeral session DB path from the main DB path.
// For a file DB it is a sibling "<name>.sessions.db"; for the :memory: fallback
// (tests, DB-only mode) it is a unique temp file so multiple in-memory Services
// don't collide on one session file.
func SessionDBPathFor(mainPath string) string {
	if mainPath == ":memory:" || mainPath == "" {
		return filepath.Join(os.TempDir(), "knomit-sessions-"+uuid.New().String()+".db")
	}
	return strings.TrimSuffix(mainPath, ".db") + SessionDBSuffix
}
