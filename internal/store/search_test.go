package store_test

import (
	"fmt"
	"testing"

	"knomit/internal/store"
	"go.uber.org/mock/gomock"
)

func TestSearchIncludeTypes(t *testing.T) {
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	facts := []struct {
		path     string
		blobHash string
		typ      string
		body     string
	}{
		{"kb/obs.md", "blob_obs", "observation", "observing something"},
		{"kb/hyp.md", "blob_hyp", "hypothesis", "a hypothesis about X"},
		{"kb/syn.md", "blob_syn", "synthesis", "synthesized knowledge"},
	}

	for _, f := range facts {
		insertTestBlob(t, idx.DB(), f.blobHash, f.body)
		if err := idx.Upsert(store.FactRecord{
			Path:       f.path,
			Title:      f.typ + " fact",
			BlobHash:   f.blobHash,
			Type:       f.typ,
			Domain:     []string{"test"},
			Entities:   []string{},
			Confidence: 0.9,
			Sources:    1,
			CommitHash: "abc",
		}); err != nil {
			t.Fatal(err)
		}
	}

	results, err := idx.Search(store.SearchQuery{
		IncludeTypes: []string{"observation"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Type != "observation" {
		t.Fatalf("expected type observation, got %q", results[0].Type)
	}
}

func TestSearchExcludeTypes(t *testing.T) {
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	facts := []struct {
		path     string
		blobHash string
		typ      string
		body     string
	}{
		{"kb/obs.md", "blob_obs", "observation", "observing something"},
		{"kb/hyp.md", "blob_hyp", "hypothesis", "a hypothesis about X"},
		{"kb/syn.md", "blob_syn", "synthesis", "synthesized knowledge"},
	}

	for _, f := range facts {
		insertTestBlob(t, idx.DB(), f.blobHash, f.body)
		if err := idx.Upsert(store.FactRecord{
			Path:       f.path,
			Title:      f.typ + " fact",
			BlobHash:   f.blobHash,
			Type:       f.typ,
			Domain:     []string{"test"},
			Entities:   []string{},
			Confidence: 0.9,
			Sources:    1,
			CommitHash: "abc",
		}); err != nil {
			t.Fatal(err)
		}
	}

	results, err := idx.Search(store.SearchQuery{
		ExcludeTypes: []string{"hypothesis"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	for _, r := range results {
		if r.Type == "hypothesis" {
			t.Fatal("hypothesis should have been excluded")
		}
	}
}

func TestSearchTypeFilterEmptyPassesAll(t *testing.T) {
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	facts := []struct {
		path     string
		blobHash string
		typ      string
		body     string
	}{
		{"kb/obs.md", "blob_obs", "observation", "observing something"},
		{"kb/hyp.md", "blob_hyp", "hypothesis", "a hypothesis about X"},
		{"kb/syn.md", "blob_syn", "synthesis", "synthesized knowledge"},
	}

	for _, f := range facts {
		insertTestBlob(t, idx.DB(), f.blobHash, f.body)
		if err := idx.Upsert(store.FactRecord{
			Path:       f.path,
			Title:      f.typ + " fact",
			BlobHash:   f.blobHash,
			Type:       f.typ,
			Domain:     []string{"test"},
			Entities:   []string{},
			Confidence: 0.9,
			Sources:    1,
			CommitHash: "abc",
		}); err != nil {
			t.Fatal(err)
		}
	}

	results, err := idx.Search(store.SearchQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
}

func TestSearchIncludeMultipleTypes(t *testing.T) {
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	facts := []struct {
		path     string
		blobHash string
		typ      string
		body     string
	}{
		{"kb/obs.md", "blob_obs", "observation", "observing something"},
		{"kb/hyp.md", "blob_hyp", "hypothesis", "a hypothesis about X"},
		{"kb/syn.md", "blob_syn", "synthesis", "synthesized knowledge"},
		{"kb/dec.md", "blob_dec", "decision", "a decision was made"},
	}

	for _, f := range facts {
		insertTestBlob(t, idx.DB(), f.blobHash, f.body)
		if err := idx.Upsert(store.FactRecord{
			Path:       f.path,
			Title:      f.typ + " fact",
			BlobHash:   f.blobHash,
			Type:       f.typ,
			Domain:     []string{"test"},
			Entities:   []string{},
			Confidence: 0.9,
			Sources:    1,
			CommitHash: "abc",
		}); err != nil {
			t.Fatal(err)
		}
	}

	results, err := idx.Search(store.SearchQuery{
		IncludeTypes: []string{"observation", "hypothesis"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	types := map[string]bool{}
	for _, r := range results {
		types[r.Type] = true
	}
	if !types["observation"] || !types["hypothesis"] {
		t.Fatalf("expected observation and hypothesis, got %v", types)
	}
}

func TestSearchEntityFilterSQL(t *testing.T) {
	// Regression test: entity and domain filters must be applied in SQL
	// (via json_each), not Go-side post-filtering. Before the fix, the
	// text-less path used a LIMIT with Go-side filtering, which could miss
	// facts beyond the over-fetch window.
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	// Create facts with different entities.
	facts := []struct {
		path     string
		blobHash string
		entities []string
		domain   []string
	}{
		{"kb/a.md", "blob_a", []string{"PostgreSQL"}, []string{"databases"}},
		{"kb/b.md", "blob_b", []string{"developer-platform"}, []string{"tools"}},
		{"kb/c.md", "blob_c", []string{"Redis", "caching"}, []string{"databases", "infra"}},
		{"kb/d.md", "blob_d", []string{"developer-platform", "CI/CD"}, []string{"tools", "devops"}},
	}
	for _, f := range facts {
		insertTestBlob(t, idx.DB(), f.blobHash, "content for "+f.path)
		if err := idx.Upsert(store.FactRecord{
			Path: f.path, Title: f.path, BlobHash: f.blobHash,
			Type: "observation", Domain: f.domain, Entities: f.entities,
			Confidence: 0.9, Sources: 1, CommitHash: "abc",
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Entity filter with a hyphenated name.
	results, err := idx.Search(store.SearchQuery{Entities: []string{"developer-platform"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results for entity developer-platform, got %d", len(results))
	}
	paths := map[string]bool{}
	for _, r := range results {
		paths[r.Path] = true
	}
	if !paths["kb/b.md"] || !paths["kb/d.md"] {
		t.Fatalf("expected kb/b.md and kb/d.md, got %v", paths)
	}

	// Domain prefix filter.
	results, err = idx.Search(store.SearchQuery{Domain: []string{"tools"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results for domain tools, got %d", len(results))
	}

	// Combined entity + domain filter.
	results, err = idx.Search(store.SearchQuery{
		Entities: []string{"developer-platform"},
		Domain:   []string{"devops"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for entity+domain combo, got %d", len(results))
	}
	if results[0].Path != "kb/d.md" {
		t.Fatalf("expected kb/d.md, got %s", results[0].Path)
	}

	// Entity filter is case-insensitive.
	results, err = idx.Search(store.SearchQuery{Entities: []string{"POSTGRESQL"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Path != "kb/a.md" {
		t.Fatalf("expected 1 result for case-insensitive entity, got %d", len(results))
	}
}

func TestSearchEntityWithSpaces(t *testing.T) {
	// Regression: entities with spaces (e.g. "Composer 2", "supply chain security")
	// must be findable via the text-less search path.
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	facts := []struct {
		path     string
		blobHash string
		entities []string
	}{
		{"kb/a.md", "blob_a", []string{"Composer 2", "PHP"}},
		{"kb/b.md", "blob_b", []string{"supply chain security"}},
		{"kb/c.md", "blob_c", []string{"Redis"}},
	}
	for _, f := range facts {
		insertTestBlob(t, idx.DB(), f.blobHash, "content for "+f.path)
		if err := idx.Upsert(store.FactRecord{
			Path: f.path, Title: f.path, BlobHash: f.blobHash,
			Type: "observation", Domain: []string{"test"}, Entities: f.entities,
			Confidence: 0.9, Sources: 1, CommitHash: "abc",
		}); err != nil {
			t.Fatal(err)
		}
	}

	results, err := idx.Search(store.SearchQuery{Entities: []string{"Composer 2"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Path != "kb/a.md" {
		t.Fatalf("expected kb/a.md for entity 'Composer 2', got %d results", len(results))
	}

	results, err = idx.Search(store.SearchQuery{Entities: []string{"supply chain security"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Path != "kb/b.md" {
		t.Fatalf("expected kb/b.md for entity 'supply chain security', got %d results", len(results))
	}
}

func TestSearchEntityBeyondOldLimit(t *testing.T) {
	// Regression: before pushing entity filters to SQL, the text-less path
	// fetched LIMIT*3 rows and filtered in Go. If the matching fact was
	// beyond that window it was silently missed. This test creates enough
	// facts to exceed the old 150-row window.
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	// Insert 200 filler facts with entity "filler".
	for i := 0; i < 200; i++ {
		bh := fmt.Sprintf("blob_filler_%d", i)
		path := fmt.Sprintf("kb/filler/%d.md", i)
		insertTestBlob(t, idx.DB(), bh, fmt.Sprintf("filler content %d", i))
		if err := idx.Upsert(store.FactRecord{
			Path: path, Title: path, BlobHash: bh,
			Type: "observation", Domain: []string{"test"}, Entities: []string{"filler"},
			Confidence: 0.9, Sources: 1, CommitHash: "abc",
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Insert the needle — a hypothesis with a unique entity.
	insertTestBlob(t, idx.DB(), "blob_needle", "the needle fact")
	if err := idx.Upsert(store.FactRecord{
		Path: "kb/needle.md", Title: "Needle", BlobHash: "blob_needle",
		Type: "hypothesis", Domain: []string{"special"}, Entities: []string{"rare-entity"},
		Confidence: 0.6, Sources: 1, CommitHash: "abc",
	}); err != nil {
		t.Fatal(err)
	}

	// Search by the unique entity — must find the needle even though
	// it's beyond the first 150 rows.
	results, err := idx.Search(store.SearchQuery{Entities: []string{"rare-entity"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for rare-entity, got %d", len(results))
	}
	if results[0].Path != "kb/needle.md" {
		t.Fatalf("expected kb/needle.md, got %s", results[0].Path)
	}

	// Also verify domain filter finds it.
	results, err = idx.Search(store.SearchQuery{Domain: []string{"special"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Path != "kb/needle.md" {
		t.Fatalf("expected 1 result for domain special, got %d", len(results))
	}
}

func TestSearchVecPathEntityFilter(t *testing.T) {
	// Regression: the vector search path (text + entity filter) must apply
	// entity/domain filters in SQL, not in Go post-filtering.
	idx, err := store.New(":memory:", store.WithVecDimension(4))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	vecA := []float32{1, 0, 0, 0}
	vecB := []float32{0.9, 0.4, 0, 0}
	vecQ := []float32{1, 0, 0, 0}

	m := map[string][]float32{
		"Alpha caching content":   vecA,
		"Beta caching content":    vecB,
		"caching":                 vecQ,
	}
	ctrl := gomock.NewController(t)
	emb := NewMockEmbedder(ctrl)
	emb.EXPECT().Embed(gomock.Any()).DoAndReturn(func(text string) ([]float32, error) {
		if v, ok := m[text]; ok {
			return v, nil
		}
		return make([]float32, 4), nil
	}).AnyTimes()
	idx.SetEmbedder(emb)

	insertTestBlob(t, idx.DB(), "blob_va", "caching content")
	insertTestBlob(t, idx.DB(), "blob_vb", "caching content")

	if err := idx.Upsert(store.FactRecord{
		Path: "kb/va.md", Title: "Alpha", BlobHash: "blob_va",
		Type: "observation", Domain: []string{"infra"}, Entities: []string{"Redis", "caching"},
		Confidence: 0.9, Sources: 1, CommitHash: "x",
	}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Upsert(store.FactRecord{
		Path: "kb/vb.md", Title: "Beta", BlobHash: "blob_vb",
		Type: "hypothesis", Domain: []string{"infra"}, Entities: []string{"Memcached", "caching"},
		Confidence: 0.7, Sources: 1, CommitHash: "x",
	}); err != nil {
		t.Fatal(err)
	}

	// Text search without entity filter — both should match.
	results, err := idx.Search(store.SearchQuery{Text: "caching", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results for text-only search, got %d", len(results))
	}

	// Text search + entity filter — only Redis fact should match.
	results, err = idx.Search(store.SearchQuery{Text: "caching", Entities: []string{"Redis"}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for text+entity search, got %d", len(results))
	}
	if results[0].Path != "kb/va.md" {
		t.Fatalf("expected kb/va.md, got %s", results[0].Path)
	}

	// Text search + domain filter.
	results, err = idx.Search(store.SearchQuery{Text: "caching", Domain: []string{"infra"}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results for text+domain search, got %d", len(results))
	}

	// Text search + entity filter for hypothesis type — should still find it.
	results, err = idx.Search(store.SearchQuery{Text: "caching", Entities: []string{"Memcached"}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for hypothesis entity, got %d", len(results))
	}
	if results[0].Type != "hypothesis" {
		t.Fatalf("expected hypothesis type, got %s", results[0].Type)
	}
}

func TestMatchesFiltersWithTypes(t *testing.T) {
	// Test matchesFilters via Search with combined filters.
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	insertTestBlob(t, idx.DB(), "blob_a", "content a")
	insertTestBlob(t, idx.DB(), "blob_b", "content b")

	if err := idx.Upsert(store.FactRecord{
		Path: "kb/a.md", Title: "A", BlobHash: "blob_a", Type: "observation",
		Domain: []string{"test"}, Entities: []string{}, Confidence: 0.9, Sources: 1, CommitHash: "abc",
	}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Upsert(store.FactRecord{
		Path: "kb/b.md", Title: "B", BlobHash: "blob_b", Type: "hypothesis",
		Domain: []string{"test"}, Entities: []string{}, Confidence: 0.5, Sources: 1, CommitHash: "abc",
	}); err != nil {
		t.Fatal(err)
	}

	// Exclude hypothesis + min confidence filter
	results, err := idx.Search(store.SearchQuery{
		ExcludeTypes:  []string{"hypothesis"},
		MinConfidence: 0.8,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Type != "observation" {
		t.Fatalf("expected observation, got %q", results[0].Type)
	}
}
