package store

import (
	"fmt"
	"testing"
)

// seedBenchIndex inserts n facts with realistic entity/domain/ref structure and
// syncs the graph. Returns the Index; caller must call idx.Close().
//
// Structure:
//   - 10 entities (e0..e9), each assigned to n/10 facts
//   - 5 top-level domains (d0..d4), each with 2 sub-domains
//   - 20% of facts have a DERIVED_FROM ref to another fact
func seedBenchIndex(b *testing.B, n int) *Index {
	b.Helper()
	idx, err := New(":memory:")
	if err != nil {
		b.Fatal(err)
	}

	const nEntities = 10
	const nDomains = 5

	for i := 0; i < n; i++ {
		path := fmt.Sprintf("kb/bench/fact%04d.md", i)
		entity := fmt.Sprintf("Entity%d", i%nEntities)
		topDomain := fmt.Sprintf("domain%d", i%nDomains)
		subDomain := fmt.Sprintf("domain%d/sub%d", i%nDomains, i%2)

		var refs []string
		if i > 0 && i%5 == 0 {
			refs = []string{fmt.Sprintf("kb/bench/fact%04d.md", i-1)}
		}

		rec := FactRecord{
			Path:       path,
			Title:      fmt.Sprintf("Bench fact %d", i),
			BlobHash:   fmt.Sprintf("deadbeef%08d", i),
			Type:       "observation",
			Domain:     []string{topDomain, subDomain},
			Entities:   []string{entity},
			Confidence: 0.8,
			Sources:    1,
			Refs:       refs,
		}

		// Write blob so Upsert can find it.
		if _, err := idx.db.Exec(
			`INSERT OR IGNORE INTO objects(hash, type, size, data) VALUES (?, ?, ?, ?)`,
			rec.BlobHash, BlobObjectType, 10, []byte("bench body"),
		); err != nil {
			b.Fatalf("insert blob: %v", err)
		}

		if err := idx.Upsert(testBranch, "abc", rec); err != nil {
			b.Fatalf("upsert fact %d: %v", i, err)
		}
	}
	return idx
}

// ── Entity lookup ─────────────────────────────────────────────────────────────

// BenchmarkEntityFilter_SQL measures lookup via fact_entities junction table.
func BenchmarkEntityFilter_SQL(b *testing.B) {
	for _, n := range []int{100, 1_000, 5_000} {
		n := n
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			idx := seedBenchIndex(b, n)
			defer idx.Close()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				rows, err := idx.db.Query(
					`SELECT fact_id FROM fact_entities WHERE entity = ?`,
					"Entity3",
				)
				if err != nil {
					b.Fatal(err)
				}
				var paths []string
				for rows.Next() {
					var p string
					rows.Scan(&p)
					paths = append(paths, p)
				}
				rows.Close()
				if len(paths) == 0 {
					b.Fatal("no results")
				}
			}
		})
	}
}

// BenchmarkEntityFilter_Graph measures lookup via TAGGED graph edges.
func BenchmarkEntityFilter_Graph(b *testing.B) {
	for _, n := range []int{100, 1_000, 5_000} {
		n := n
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			idx := seedBenchIndex(b, n)
			defer idx.Close()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				pj := jsonParams("entity", "Entity3")
				rows, err := idx.db.Query(
					`SELECT json_extract(value, '$.path') FROM json_each(cypher('MATCH (f:Fact)-[:TAGGED]->(e:Entity {name: $entity}) RETURN f.path AS path', ?))`,
					pj,
				)
				if err != nil {
					b.Fatal(err)
				}
				var paths []string
				for rows.Next() {
					var p string
					rows.Scan(&p)
					paths = append(paths, p)
				}
				rows.Close()
				if len(paths) == 0 {
					b.Fatal("no results")
				}
			}
		})
	}
}

// ── Domain lookup ─────────────────────────────────────────────────────────────

// BenchmarkDomainFilter_SQL measures lookup via fact_domains junction table,
// including hierarchical prefix matching (domain = ? OR domain LIKE ?/%).
func BenchmarkDomainFilter_SQL(b *testing.B) {
	for _, n := range []int{100, 1_000, 5_000} {
		n := n
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			idx := seedBenchIndex(b, n)
			defer idx.Close()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				rows, err := idx.db.Query(
					`SELECT fact_id FROM fact_domains WHERE domain = ? OR domain LIKE ?`,
					"domain2", "domain2/%",
				)
				if err != nil {
					b.Fatal(err)
				}
				var paths []string
				for rows.Next() {
					var p string
					rows.Scan(&p)
					paths = append(paths, p)
				}
				rows.Close()
				if len(paths) == 0 {
					b.Fatal("no results")
				}
			}
		})
	}
}

// BenchmarkDomainFilter_Graph measures lookup via IN_DOMAIN graph edges,
// matching the top-level domain and all sub-domains via DOMAIN_CHILD_OF.
func BenchmarkDomainFilter_Graph(b *testing.B) {
	for _, n := range []int{100, 1_000, 5_000} {
		n := n
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			idx := seedBenchIndex(b, n)
			defer idx.Close()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				pj := jsonParams("domain", "domain2")
				rows, err := idx.db.Query(
					`SELECT json_extract(value, '$.path') FROM json_each(cypher('MATCH (f:Fact)-[:IN_DOMAIN]->(d:Domain) WHERE d.path = $domain OR (d)-[:DOMAIN_CHILD_OF*]->(:Domain {path: $domain}) RETURN DISTINCT f.path AS path', ?))`,
					pj,
				)
				if err != nil {
					b.Fatal(err)
				}
				var paths []string
				for rows.Next() {
					var p string
					rows.Scan(&p)
					paths = append(paths, p)
				}
				rows.Close()
				if len(paths) == 0 {
					b.Fatal("no results")
				}
			}
		})
	}
}

// ── Refs in (incoming DERIVED_FROM) ──────────────────────────────────────────

// BenchmarkRefsIn_Graph measures lookup of incoming DERIVED_FROM edges for a
// single fact. There is no SQL equivalent (refs are a JSON blob on facts).
func BenchmarkRefsIn_Graph(b *testing.B) {
	for _, n := range []int{100, 1_000, 5_000} {
		n := n
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			idx := seedBenchIndex(b, n)
			defer idx.Close()
			target := "kb/bench/fact0004.md" // has multiple referrers from seeding
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				pj := jsonParams("path", target)
				rows, err := idx.db.Query(
					`SELECT json_extract(value, '$.path') FROM json_each(cypher('MATCH (f:Fact)-[:DERIVED_FROM]->(t:Fact {path: $path}) RETURN f.path AS path', ?))`,
					pj,
				)
				if err != nil {
					b.Fatal(err)
				}
				var paths []string
				for rows.Next() {
					var p string
					rows.Scan(&p)
					paths = append(paths, p)
				}
				rows.Close()
			}
		})
	}
}
