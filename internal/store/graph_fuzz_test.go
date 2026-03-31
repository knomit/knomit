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
			Path:       "kb/" + path + ".md",
			BlobHash:   "fuzz_bh",
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

		err = idx.graphDeleteFact(rec.Path, rec.BlobHash)
		if err != nil {
			t.Fatalf("graphDeleteFact failed: %v\npath=%q", err, path)
		}
	})
}

