// Custom SQLite functions registered via ConnectHook. These run inside the
// database engine and are used by bulk SQL statements (e.g. fast rebuild).
package store

import (
	"encoding/json"
	"sort"
	"strings"

	"knomit/internal/fact"
	"knomit/internal/fact/textnorm"

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
	if err := conn.RegisterFunc("knomit_canon_domain", canonicalizeDomain, true); err != nil {
		return err
	}
	// knomit_motif_key computes a motif's mechanical cluster key, so SQL that
	// reads an UNRESOLVED motif can key it exactly as a rebuild would.
	//
	// It exists so every motif API degrades the same way on a corpus whose
	// alias table has not been built yet: each spelling is its own singleton
	// cluster. Without it, the aggregate readers returned nothing while the
	// point readers returned singletons, so one corpus reported two different
	// vocabularies depending on which API you asked.
	return conn.RegisterFunc("knomit_motif_key", groupingKey, true)
}

// parsedFact is the JSON structure returned by knomit_parse_fact.
type parsedFact struct {
	Title          string   `json:"title"`
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
	// Expires/ExpiresAt fill the fact_expires side table on rebuild;
	// omitted (JSON null on extract) when the fact has none.
	Expires   string `json:"expires,omitempty"`
	ExpiresAt *int64 `json:"expires_at,omitempty"`
	// Context (canonical text) and ContextNum (numbers only) fill the
	// fact_context side table on rebuild; absent when the fact has none or
	// ParseFact dropped a malformed map.
	Context    map[string]string  `json:"context,omitempty"`
	ContextNum map[string]float64 `json:"context_num,omitempty"`
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
		Motifs:         f.Motifs,
		Confidence:     f.Confidence,
		Sources:        f.Sources,
		Refs:           f.Refs,
		EvidenceWeight: f.EvidenceWeight,
		Origin:         string(origin),
		Expires:        f.Expires,
		ExpiresAt:      fact.ExpiresUnix(f.Expires),
	}
	for k, v := range f.Context {
		if pf.Context == nil {
			pf.Context = map[string]string{}
		}
		pf.Context[k] = fact.ContextText(v)
		if x, ok := fact.ContextNum(v); ok {
			if pf.ContextNum == nil {
				pf.ContextNum = map[string]float64{}
			}
			pf.ContextNum[k] = x
		}
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
	sim, ok := cosineSim(va, vb)
	if !ok {
		// NULL only for a vector with no defined similarity. Orthogonal vectors
		// are not that case: their similarity is 0, and reporting NULL for it
		// would change what this function has always answered.
		return nil
	}
	return sim
}

// groupingKey is the mechanical cluster key for a motif spelling: its
// canonicalized tokens, stemmed, SORTED and rejoined.
//
// Sorted, so the key is a token MULTISET and word order cannot split a
// cluster — "atomic-write" and "write-atomic" name the same mechanism.
//
// Stemming comes from textnorm.Tokens, which already returns STEMMED tokens
// (see its doc) — so "silent-fallbacks" and "silent-fallback" group without a
// second Stem call here. An earlier draft called textnorm.Stem again on each
// token; it was invisible because stemming is idempotent, and it was caught by
// sabotaging it and watching the test still pass. Reaching for the shared
// matcher rather than re-deriving is also what invariant 7a6af15e requires.
//
// This key is internal. It is deliberately NOT the canonical id: sorting reads
// badly ("fallback-silent"), and the id is shown to readers in the §6 explain
// surface and to an LLM in the backfill prompt. The id is a real member
// spelling; see RebuildAliases.
func groupingKey(motif string) string {
	toks := textnorm.Tokens(textnorm.Canonicalize(motif))
	stemmed := make([]string, 0, len(toks))
	for _, t := range toks {
		if t == "" {
			continue
		}
		stemmed = append(stemmed, t)
	}
	sort.Strings(stemmed)
	return strings.Join(stemmed, "-")
}

// motifClusterKeyExpr renders the ONE SQL expression that keys a motif
// spelling to its cluster, for the readers that must agree with each other.
//
// Three steps, and each is a posture decision rather than defensiveness:
//   - the STORED key, when the vocabulary has been resolved;
//   - else the key a rebuild WOULD compute (knomit_motif_key is groupingKey),
//     so a corpus with facts written since the last rebuild reads the way it
//     will read a moment later rather than a different way in the meantime;
//   - else the spelling itself, for a motif whose every token normalizes away.
//     Those have no mechanical cluster to join, and letting them share the
//     empty key would fuse unrelated motifs into one; RebuildAliases makes the
//     same call by skipping them.
//
// Shared as TEXT because the alternative is three hand-written COALESCEs that
// agree until one of them is edited. The caller supplies the motif column
// (queries name it differently) and must alias motif_aliases as `a`.
func motifClusterKeyExpr(motifCol string) string {
	return `COALESCE(NULLIF(a.cluster_key, ''), NULLIF(knomit_motif_key(` + motifCol + `), ''), ` + motifCol + `)`
}
