// Custom SQLite functions registered via ConnectHook. These run inside the
// database engine and are used by bulk SQL statements (e.g. fast rebuild).
package store

import (
	"encoding/json"

	"knomit/internal/fact"

	sqlite3 "github.com/mattn/go-sqlite3"
)

// registerSQLFuncs registers all custom SQL functions on a new connection.
// Called via ConnectHook so every pooled connection gets the same functions.
func registerSQLFuncs(conn *sqlite3.SQLiteConn) error {
	if err := conn.RegisterFunc("knomit_parse_fact", sqlParseFact, true); err != nil {
		return err
	}
	if err := conn.RegisterFunc("knomit_cosine_sim", sqlCosineSim, true); err != nil {
		return err
	}
	// knomit_canon_domain canonicalises a domain tag (NFC + fold + de-hyphenize)
	// so the bulk rebuild SQL can store canonical fact_domains values.
	return conn.RegisterFunc("knomit_canon_domain", canonicalizeDomain, true)
}

// parsedFact is the JSON structure returned by knomit_parse_fact.
type parsedFact struct {
	Title          string   `json:"title"`
	Kind           string   `json:"kind"`
	Type           string   `json:"type"`
	Domain         []string `json:"domain"`
	Entities       []string `json:"entities"`
	Confidence     float64  `json:"confidence"`
	Sources        int      `json:"sources"`
	Refs           []string `json:"refs"`
	EvidenceWeight float64  `json:"evidence_weight,omitempty"`
	Origin         string   `json:"origin"`
}

// sqlParseFact parses a knomit fact markdown blob (YAML frontmatter + body)
// and returns a JSON string, or nil (SQL NULL) for non-fact files.
func sqlParseFact(data []byte) interface{} {
	f, err := fact.ParseFact("", string(data))
	if err != nil {
		return nil
	}
	kind := f.Kind
	if kind == "" {
		kind = fact.DefaultKind
	}
	origin := f.Origin
	if origin == "" {
		origin = fact.DefaultOrigin
	}
	pf := &parsedFact{
		Title:          f.Title,
		Kind:           string(kind),
		Type:           string(f.Type),
		Domain:         f.Domain,
		Entities:       f.Entities,
		Confidence:     f.Confidence,
		Sources:        f.Sources,
		Refs:           f.Refs,
		EvidenceWeight: f.EvidenceWeight,
		Origin:         string(origin),
	}
	b, err := json.Marshal(pf)
	if err != nil {
		return nil
	}
	return string(b)
}

// sqlCosineSim computes cosine similarity between two embedding BLOBs.
// Both inputs must be little-endian float32 arrays of equal length.
// Returns a REAL in [-1, 1] or NULL on invalid input.
func sqlCosineSim(a, b []byte) interface{} {
	va, err := bytesToFloat32Slice(a)
	if err != nil || len(va) == 0 {
		return nil
	}
	vb, err := bytesToFloat32Slice(b)
	if err != nil || len(vb) != len(va) {
		return nil
	}
	sim := CosineSim(va, vb)
	if sim == 0 {
		// CosineSim folds "degenerate" into 0; SQL callers want NULL for a
		// vector that has no meaningful similarity, and a genuine 0 is
		// indistinguishable from it at this boundary. Orthogonality is
		// vanishingly rare in practice and NULL is the safer answer.
		return nil
	}
	return sim
}
