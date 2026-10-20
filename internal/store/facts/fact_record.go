package facts

import (
	"knomit/internal/fact"
)

// FactRecord is the stored record — no body, just a blob_hash pointer.
type FactRecord struct {
	Path           string   `json:"path"`
	Title          string   `json:"title"`
	BlobHash       string   `json:"blob_hash"`
	Kind           string   `json:"kind"`
	Type           string   `json:"type"`
	Domain         []string `json:"domain"`
	Entities       []string `json:"entities"`
	Motifs         []string `json:"motifs,omitempty"`
	Confidence     float64  `json:"confidence"`
	Sources        int      `json:"sources"`
	Refs           []string `json:"refs"`
	EvidenceWeight float64  `json:"evidence_weight,omitempty"`
	Origin         string   `json:"origin"`
	// Expires is the frontmatter value as written, "" when absent. Knomit
	// never acts on it; see fact/expires.go.
	Expires      string `json:"expires,omitempty"`
	SourceCommit string `json:"source_commit,omitempty"` // commit at which this version was written
	// Context is the fact's F22 context map. On a write it fills the
	// fact_context side table; on a read it is taken from the BLOB (typed:
	// the table's text cannot tell the string "true" from the bool true), on
	// every read path that already holds the blob.
	Context map[string]any `json:"context,omitempty"`
}

// NewFactRecord constructs a FactRecord from a parsed fact and git metadata.
// blobHash is the blob SHA returned by WriteFile.
//
// Kind is normalized at this boundary: a zero-value fact.Kind (e.g. from a
// pre-Kind-aware caller) is canonicalized to fact.DefaultKind so the
// in-memory FactRecord always carries the same value the SQL row will
// carry under the column's DEFAULT.
func NewFactRecord(f fact.Fact, blobHash string) FactRecord {
	kind := f.Kind
	if kind == "" {
		kind = fact.DefaultKind
	}
	origin := f.Origin
	if origin == "" {
		origin = fact.DefaultOrigin
	}
	return FactRecord{
		Path:           f.Path(),
		Title:          f.Title,
		BlobHash:       blobHash,
		Kind:           string(kind),
		Type:           string(f.Type),
		Domain:         f.Domain,
		Entities:       f.Entities,
		Motifs:         f.Motifs,
		Confidence:     f.Confidence,
		Sources:        f.Sources,
		Refs:           f.Refs,
		EvidenceWeight: f.EvidenceWeight,
		Origin:         string(origin),
		Expires:        f.Expires,
		Context:        f.Context,
	}
}

// FactWithBody is returned by read operations that hydrate the body from git objects.
type FactWithBody struct {
	FactRecord
	Body        string `json:"body"`
	CommitHash  string `json:"commit_hash,omitempty"`
	CommittedAt int64  `json:"committed_at,omitempty"`
}
