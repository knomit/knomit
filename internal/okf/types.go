// internal/okf/types.go
package okf

import (
	"time"

	"knomit/internal/fact"
)

// RepoIdentity is the machine-independent identity of the source repo.
// ID is the root commit hash in 12-hex wire form (never the repo name).
type RepoIdentity struct {
	ID string
}

// FactInput pairs a parsed fact with its authoring time, resolved by the
// caller from git history. Timestamp is the time of the commit that first
// introduced the fact's path.
type FactInput struct {
	Fact      fact.Fact
	Timestamp time.Time
	// Revisions is this fact's recorded history, used to render the History
	// section. Empty or single-element ⇒ no section is emitted.
	Revisions []Revision
}

// LogEntry is one changelog row for log.md.
type LogEntry struct {
	Date time.Time // the commit time of the event
	// Kind is "Creation", "Update" or "Deprecation" — the three labels §9 names
	// as its convention. A knowledge base that only ever reported what it added
	// and edited was describing half of what happened to it.
	Kind  string
	Title string // the fact's title at the time (best-effort; current title is acceptable)
	Path  string // the fact's knomit path, e.g. "kb/decisions/okf/x/ab12cd34.md"
	// Delta is what this event changed, in MeaningfulRevisions' wording
	// ("confidence 0.9 → 0.85"), or for a Deprecation how the fact left
	// ("retracted", "superseded by …"). Empty on an Update means nothing worth
	// reporting changed, and RenderLog drops it: a bare "**Update** <title>"
	// row states that something happened while being unable to say what.
	Delta string
}

// Revision is one recorded change to a fact. Revisions are supplied by the
// caller in any order; rendering sorts them. The fields are exactly what the
// delta wording needs — the mapper never sees a revision's body text, only a
// digest, so a corpus with thousands of revisions costs nothing to hold.
type Revision struct {
	Date       time.Time
	Operation  string // "learn" | "distill" | "review" | "human" | "edit" | …; "" when unknown
	Confidence float64
	Title      string
	BodyDigest string // equality only
	RefCount   int
}

// Retirement is a fact the knowledge base has withdrawn. Kind is "retracted"
// (dropped outright) or "superseded" (replaced by SuccessorPath) — the same
// distinction knomit_explain draws between a deleted and a superseded source.
//
// Retirements are rendered as an INDEX ONLY, never as concept documents: their
// claims have been disavowed, and a conformant consumer may ignore
// `status: deprecated`, so an ingestible document would invite re-ingestion of
// withdrawn knowledge.
type Retirement struct {
	Date          time.Time
	Title         string
	Path          string // the retired fact's knomit path
	Kind          string // "retracted" | "superseded"
	SuccessorPath string // knomit path of the replacement; "" when unknown
}

// The two retirement kinds, mirroring knomit's own vocabulary.
const (
	RetiredRetracted  = "retracted"
	RetiredSuperseded = "superseded"
)

// FactRef is a resolved pointer to another fact's document in this bundle:
// where it lives and what it is called. The title matters as much as the path —
// a citation labelled with a raw fact path tells a reader nothing, while the
// title says what they would be opening.
type FactRef struct {
	Path  string // bundle path, e.g. "kb/decisions/okf/scope/export-…-d9d6557d.md"
	Title string // the target fact's title
}

// OntologyDoc carries the knowledge scheme's own authored documentation, read
// from the ontology file in the source tree. Descriptions exist at every
// level (the scheme itself, each topic, each category), which is exactly the
// shape of the bundle's directory tree — so they become the `description` OKF
// recommends for index entries, sourced from authored text rather than
// synthesized from fact bodies.
type OntologyDoc struct {
	Name        string // e.g. "Source Code Knowledge"
	Description string // e.g. "Knowledge categories for AI agents working in a codebase."
	// Nodes maps an ontology path relative to kb/ ("invariants",
	// "invariants/architecture") to that node's description.
	Nodes map[string]string
}

// File is one rendered file in the bundle. Path is bundle-relative and
// forward-slashed (e.g. "decisions/okf/index.md").
type File struct {
	Path    string
	Content []byte
}

// Bundle is the complete in-memory OKF bundle. Files are unordered; callers
// that need determinism sort by Path.
type Bundle struct {
	Files []File
}
