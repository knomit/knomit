package store

import (
	"testing"
)

// FuzzGraphUpsert exercises the full graphSyncFact + graphDeleteFact path
// with adversarial strings in every field that gets interpolated into Cypher.
// It catches SQL/Cypher injection bugs like the apostrophe issue where
// a single quote in a title broke the outer SQL string wrapper.
func FuzzGraphUpsert(f *testing.F) {
	// Seed corpus: known-tricky characters.
	seeds := []string{
		"simple",
		"Dave's expertise",
		`say "hello"`,
		`back\slash`,
		"line\nbreak",
		"null\x00byte",
		`'; DROP TABLE facts; --`,
		`")); DELETE FROM node_labels; SELECT cypher('RETURN 1`,
		`%s %d %f %v`,
		"emoji 🎉 and unicode «»",
		"", // empty string
		"/slashes/in/path",
		"engineering/software/web",
		`nested "quotes 'mixed' here"`,
		`tab\there`,
		"控制字符",
	}
	for _, s := range seeds {
		f.Add(s, s, s, s)
	}

	f.Fuzz(func(t *testing.T, path, title, domain, entity string) {
		if path == "" {
			return
		}

		idx, err := New(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		defer idx.Close()

		rec := FactRecord{
			Path:       "know/" + path + ".md",
			Title:      title,
			Domain:     []string{domain},
			Entities:   []string{entity},
			Confidence: 0.8,
			Sources:    1,
		}
		err = idx.graphSyncFact(rec)
		if err != nil {
			t.Fatalf("graphSyncFact failed: %v\npath=%q title=%q domain=%q entity=%q",
				err, path, title, domain, entity)
		}

		err = idx.graphDeleteFact(rec.Path)
		if err != nil {
			t.Fatalf("graphDeleteFact failed: %v\npath=%q", err, path)
		}
	})
}

// FuzzEscapeCypherKey fuzzes the key escaping function used in MATCH/MERGE
// property patterns: {path: "escaped_value"}.
func FuzzEscapeCypherKey(f *testing.F) {
	f.Add("simple")
	f.Add("it's")
	f.Add(`he said "hi"`)
	f.Add(`\backslash`)
	f.Add("null\x00byte")
	f.Add(`'; DROP TABLE x; --`)
	f.Add("")

	f.Fuzz(func(t *testing.T, input string) {
		idx, err := New(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		defer idx.Close()

		escaped := escapeCypherKey(input)

		// Test in MERGE property pattern (strictest parser)
		q := `SELECT cypher('MERGE (:Fact {path: "` + escaped + `"})')`
		if _, err := idx.db.Exec(q); err != nil {
			t.Fatalf("escapeCypherKey broke MERGE pattern: %v\ninput=%q escaped=%q", err, input, escaped)
		}

		// Test in MATCH property pattern
		q2 := `SELECT cypher('MATCH (f:Fact {path: "` + escaped + `"}) RETURN f.path')`
		if _, err := idx.db.Exec(q2); err != nil {
			t.Fatalf("escapeCypherKey broke MATCH pattern: %v\ninput=%q escaped=%q", err, input, escaped)
		}
	})
}

// FuzzEscapeCypherVal fuzzes the value escaping function used in SET clauses:
// SET f.title = "escaped_value".
func FuzzEscapeCypherVal(f *testing.F) {
	f.Add("simple")
	f.Add("it's")
	f.Add(`he said "hi"`)
	f.Add(`\backslash`)
	f.Add("null\x00byte")
	f.Add(`'; DROP TABLE x; --`)
	f.Add("")

	f.Fuzz(func(t *testing.T, input string) {
		idx, err := New(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		defer idx.Close()

		escaped := escapeCypherVal(input)

		// Baseline: create a node to SET on
		if _, err := idx.db.Exec(`SELECT cypher('MERGE (:Fact {path: "fuzz/test.md"})')`); err != nil {
			t.Fatal(err)
		}

		// Test in SET value
		q := `SELECT cypher('MATCH (f:Fact {path: "fuzz/test.md"}) SET f.title = "` + escaped + `"')`
		if _, err := idx.db.Exec(q); err != nil {
			t.Fatalf("escapeCypherVal broke SET: %v\ninput=%q escaped=%q", err, input, escaped)
		}
	})
}
