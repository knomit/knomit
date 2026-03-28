package store

import (
	"encoding/json"
	"fmt"
)

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
// Self-loops are filtered out: GraphQLite creates (n)-[:DERIVED_FROM]->(n) when
// the target node is absent at edge-creation time (upstream bug).
func (idx *Index) ExplainFact(path string) (ExplainResult, error) {
	params, _ := json.Marshal(map[string]string{"path": path})
	pj := string(params)

	incoming, err := idx.queryRefSummaries(
		fmt.Sprintf(`MATCH (f:%s)-[:%s]->(t:%s {path: $path}) WHERE NOT f.deleted = true RETURN f.path AS path, f.title AS title, false AS deleted`,
			NodeFact, EdgeDerivedFrom, NodeFact),
		pj,
	)
	if err != nil {
		return ExplainResult{}, fmt.Errorf("explain incoming: %w", err)
	}

	outgoing, err := idx.queryRefSummaries(
		fmt.Sprintf(`MATCH (f:%s {path: $path})-[:%s]->(t:%s) RETURN t.path AS path, t.title AS title, t.deleted AS deleted`,
			NodeFact, EdgeDerivedFrom, NodeFact),
		pj,
	)
	if err != nil {
		return ExplainResult{}, fmt.Errorf("explain outgoing: %w", err)
	}

	return ExplainResult{
		Incoming: filterSelf(incoming, path),
		Outgoing: filterSelf(outgoing, path),
	}, nil
}

// filterSelf removes any RefSummary whose path equals selfPath (self-loops).
func filterSelf(refs []RefSummary, selfPath string) []RefSummary {
	out := refs[:0]
	for _, r := range refs {
		if r.Path != selfPath {
			out = append(out, r)
		}
	}
	return out
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
// cypherQuery must contain only $param placeholders (no embedded values).
// paramsJSON is the JSON-encoded parameter object passed as cypher()'s second arg.
func (idx *Index) queryRefSummaries(cypherQuery, paramsJSON string) ([]RefSummary, error) {
	q := `SELECT json_extract(value, '$.path'), json_extract(value, '$.title'), json_extract(value, '$.deleted') FROM json_each(cypher('` + cypherQuery + `', ?))`
	rows, err := idx.db.Query(q, paramsJSON)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []RefSummary{}
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
