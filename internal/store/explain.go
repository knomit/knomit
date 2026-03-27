package store

import "fmt"

// RefSummary is a lightweight fact reference returned by ExplainFact.
type RefSummary struct {
	Path    string `json:"path"`
	Title   string `json:"title"`
	Deleted bool   `json:"deleted,omitempty"`
}

// ExplainResult holds the incoming and outgoing reference summary for a fact.
type ExplainResult struct {
	Incoming []RefSummary `json:"incoming"`
	Outgoing []RefSummary `json:"outgoing"`
}

// ExplainFact returns the incoming and outgoing [:DERIVED_FROM] neighbours for
// the given fact path. Incoming excludes deleted referrers. Outgoing includes
// deleted targets (marked with Deleted: true) so the UI can show them distinctly.
func (idx *Index) ExplainFact(path string) (ExplainResult, error) {
	p := escapeCypherKey(path)

	incoming, err := idx.queryRefSummaries(fmt.Sprintf(
		`MATCH (f:Fact)-[:DERIVED_FROM]->(t:Fact {path: "%s"}) WHERE NOT f.deleted = true RETURN f.path AS path, f.title AS title, false AS deleted`,
		p,
	))
	if err != nil {
		return ExplainResult{}, fmt.Errorf("explain incoming: %w", err)
	}

	outgoing, err := idx.queryRefSummaries(fmt.Sprintf(
		`MATCH (f:Fact {path: "%s"})-[:DERIVED_FROM]->(t:Fact) RETURN t.path AS path, t.title AS title, t.deleted AS deleted`,
		p,
	))
	if err != nil {
		return ExplainResult{}, fmt.Errorf("explain outgoing: %w", err)
	}

	return ExplainResult{
		Incoming: incoming,
		Outgoing: outgoing,
	}, nil
}

// isDeletedVal interprets the raw value returned by json_extract for a boolean
// property. SQLite maps JSON true→int64(1), JSON false→int64(0), and Cypher
// literal false→string("0"). Returns true only for int64(1).
func isDeletedVal(v interface{}) bool {
	switch val := v.(type) {
	case bool:
		return val
	case int64:
		return val == 1
	case []byte:
		return string(val) == "1"
	}
	return false
}

// queryRefSummaries runs a Cypher query that returns (path, title, deleted) rows.
// The cypher argument must already have all values escaped via escapeCypherKey.
func (idx *Index) queryRefSummaries(cypher string) ([]RefSummary, error) {
	q := `SELECT json_extract(value, '$.path'), json_extract(value, '$.title'), json_extract(value, '$.deleted') FROM json_each(cypher('` + cypher + `'))`
	rows, err := idx.db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []RefSummary
	for rows.Next() {
		var path, title string
		// json_extract on a JSON boolean returns an integer in SQLite (1=true, 0=false).
		// However, Cypher literal `false AS col` may return the string "0".
		// Scan into interface{} to handle both cases uniformly.
		var deletedRaw interface{}
		if err := rows.Scan(&path, &title, &deletedRaw); err != nil {
			return nil, fmt.Errorf("scan ref summary: %w", err)
		}
		if path == "" {
			continue
		}
		deleted := isDeletedVal(deletedRaw)
		result = append(result, RefSummary{Path: path, Title: title, Deleted: deleted})
	}
	return result, rows.Err()
}
