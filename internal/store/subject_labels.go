package store

import (
	"context"
	"fmt"

	"knomit/internal/fact"
)

// SubjectLabelDF is the document frequency of every SUBJECT LABEL live on a
// branch, together with the population those frequencies are drawn from.
//
// A "subject label" is a stemmed token of a fact's entities, domain tags or
// path — the same token set fact.SubjectTokens produces for the write-time
// subject strip, computed by that same function so the two cannot drift apart.
//
// It exists for the Phase-3 subject-disjointness gate, which needs to know how
// SPECIFIC a shared label is before letting it block a bridge. That question
// is answered against this corpus's own distribution rather than against any
// fixed number (roadmap MN13), so the distribution has to be readable.
//
// Derived state in the plainest sense: recomputed from live fact frontmatter
// on demand and stored nowhere, so a corpus hydrated from a remote is correct
// immediately and nothing here can go stale.
type SubjectLabelDF struct {
	// DF maps a stemmed subject token to how many live facts carry it.
	DF map[string]int
	// LiveFacts is the denominator — the count of live facts on the branch.
	LiveFacts int
}

// SubjectLabelDF computes the whole distribution in one pass.
//
// Liveness and branch scoping come from branch_facts (UNIQUE(branch_id, path)
// => one row per live path per branch), exactly as TokenDF and BlastRadius do.
//
// Tokenisation happens in Go rather than in SQL because the definition of a
// subject token lives in internal/fact and must stay single: a SQL
// reimplementation would be a second definition of the same rule, free to
// drift from the one the write path enforces.
func (gs *graphStore) SubjectLabelDF(ctx context.Context, branch string) (SubjectLabelDF, error) {
	branchID, err := gs.rh.branchID(ctx, branch)
	if err != nil {
		return SubjectLabelDF{}, fmt.Errorf("SubjectLabelDF: branchID: %w", err)
	}

	paths, err := gs.livePathsByFactID(ctx, branchID)
	if err != nil {
		return SubjectLabelDF{}, err
	}
	domains, err := gs.labelsByFact(ctx, branchID, "fact_domains", "domain")
	if err != nil {
		return SubjectLabelDF{}, err
	}
	entities, err := gs.labelsByFact(ctx, branchID, "fact_entities", "entity")
	if err != nil {
		return SubjectLabelDF{}, err
	}

	out := SubjectLabelDF{DF: make(map[string]int), LiveFacts: len(paths)}
	for id, path := range paths {
		// One SET per fact, so a token carried on both the domain and the path
		// counts once. Counting occurrences instead would let a corpus inflate
		// its own umbrella cut through how its facts happen to be filed.
		for tok := range fact.SubjectTokens(entities[id], domains[id], path) {
			out.DF[tok]++
		}
	}
	return out, nil
}

// livePathsByFactID returns fact_id → path for every fact live on the branch.
func (gs *graphStore) livePathsByFactID(ctx context.Context, branchID int64) (map[int64]string, error) {
	rows, err := conn(ctx, gs.rh.db).QueryContext(ctx,
		`SELECT bf.fact_id, bf.path FROM branch_facts bf WHERE bf.branch_id = ?`, branchID)
	if err != nil {
		return nil, fmt.Errorf("SubjectLabelDF: paths: %w", err)
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var path string
		if err := rows.Scan(&id, &path); err != nil {
			return nil, fmt.Errorf("SubjectLabelDF: scan path: %w", err)
		}
		out[id] = path
	}
	return out, rows.Err()
}

// labelsByFact reads one label junction table into fact_id → labels, restricted
// to facts live on the branch.
func (gs *graphStore) labelsByFact(ctx context.Context, branchID int64, table, col string) (map[int64][]string, error) {
	q := fmt.Sprintf(
		`SELECT bf.fact_id, j.%s
		   FROM branch_facts bf
		   JOIN %s j ON j.fact_id = bf.fact_id
		  WHERE bf.branch_id = ?`, col, table)
	rows, err := conn(ctx, gs.rh.db).QueryContext(ctx, q, branchID)
	if err != nil {
		return nil, fmt.Errorf("SubjectLabelDF: %s: %w", table, err)
	}
	defer rows.Close()
	out := map[int64][]string{}
	for rows.Next() {
		var id int64
		var label string
		if err := rows.Scan(&id, &label); err != nil {
			return nil, fmt.Errorf("SubjectLabelDF: scan %s: %w", table, err)
		}
		out[id] = append(out[id], label)
	}
	return out, rows.Err()
}
