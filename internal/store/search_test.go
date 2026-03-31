package store_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"knomit/internal/store"
	"go.uber.org/mock/gomock"
)

func testVec(values ...float32) []float32 {
	v := make([]float32, 768)
	copy(v, values)
	return v
}

func TestSearchIncludeTypes(t *testing.T) {
	ctx := context.Background()
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
		insertTestBlob(t, idx.TestDB(), f.blobHash, f.body)
		if err := idx.Upsert(ctx, testBranch, "abc", store.FactRecord{
			Path:       f.path,
			Title:      f.typ + " fact",
			BlobHash:   f.blobHash,
			Type:       f.typ,
			Domain:     []string{"test"},
			Entities:   []string{},
			Confidence: 0.9,
			Sources:    1,
		}); err != nil {
			t.Fatal(err)
		}
	}

	results, err := idx.Search(ctx, testBranch, store.SearchQuery{
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
	ctx := context.Background()
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
		insertTestBlob(t, idx.TestDB(), f.blobHash, f.body)
		if err := idx.Upsert(ctx, testBranch, "abc", store.FactRecord{
			Path:       f.path,
			Title:      f.typ + " fact",
			BlobHash:   f.blobHash,
			Type:       f.typ,
			Domain:     []string{"test"},
			Entities:   []string{},
			Confidence: 0.9,
			Sources:    1,
		}); err != nil {
			t.Fatal(err)
		}
	}

	results, err := idx.Search(ctx, testBranch, store.SearchQuery{
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
	ctx := context.Background()
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
		insertTestBlob(t, idx.TestDB(), f.blobHash, f.body)
		if err := idx.Upsert(ctx, testBranch, "abc", store.FactRecord{
			Path:       f.path,
			Title:      f.typ + " fact",
			BlobHash:   f.blobHash,
			Type:       f.typ,
			Domain:     []string{"test"},
			Entities:   []string{},
			Confidence: 0.9,
			Sources:    1,
		}); err != nil {
			t.Fatal(err)
		}
	}

	results, err := idx.Search(ctx, testBranch, store.SearchQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
}

func TestSearchIncludeMultipleTypes(t *testing.T) {
	ctx := context.Background()
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
		insertTestBlob(t, idx.TestDB(), f.blobHash, f.body)
		if err := idx.Upsert(ctx, testBranch, "abc", store.FactRecord{
			Path:       f.path,
			Title:      f.typ + " fact",
			BlobHash:   f.blobHash,
			Type:       f.typ,
			Domain:     []string{"test"},
			Entities:   []string{},
			Confidence: 0.9,
			Sources:    1,
		}); err != nil {
			t.Fatal(err)
		}
	}

	results, err := idx.Search(ctx, testBranch, store.SearchQuery{
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
	ctx := context.Background()
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
		insertTestBlob(t, idx.TestDB(), f.blobHash, "content for "+f.path)
		if err := idx.Upsert(ctx, testBranch, "abc", store.FactRecord{
			Path: f.path, Title: f.path, BlobHash: f.blobHash,
			Type: "observation", Domain: f.domain, Entities: f.entities,
			Confidence: 0.9, Sources: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Entity filter with a hyphenated name.
	results, err := idx.Search(ctx, testBranch, store.SearchQuery{Entities: []string{"developer-platform"}})
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
	results, err = idx.Search(ctx, testBranch, store.SearchQuery{Domain: []string{"tools"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results for domain tools, got %d", len(results))
	}

	// Combined entity + domain filter.
	results, err = idx.Search(ctx, testBranch, store.SearchQuery{
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
	results, err = idx.Search(ctx, testBranch, store.SearchQuery{Entities: []string{"POSTGRESQL"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Path != "kb/a.md" {
		t.Fatalf("expected 1 result for case-insensitive entity, got %d", len(results))
	}
}

func TestSearchEntityWithSpaces(t *testing.T) {
	ctx := context.Background()
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
		insertTestBlob(t, idx.TestDB(), f.blobHash, "content for "+f.path)
		if err := idx.Upsert(ctx, testBranch, "abc", store.FactRecord{
			Path: f.path, Title: f.path, BlobHash: f.blobHash,
			Type: "observation", Domain: []string{"test"}, Entities: f.entities,
			Confidence: 0.9, Sources: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}

	results, err := idx.Search(ctx, testBranch, store.SearchQuery{Entities: []string{"Composer 2"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Path != "kb/a.md" {
		t.Fatalf("expected kb/a.md for entity 'Composer 2', got %d results", len(results))
	}

	results, err = idx.Search(ctx, testBranch, store.SearchQuery{Entities: []string{"supply chain security"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Path != "kb/b.md" {
		t.Fatalf("expected kb/b.md for entity 'supply chain security', got %d results", len(results))
	}
}

func TestSearchEntityBeyondOldLimit(t *testing.T) {
	ctx := context.Background()
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
		insertTestBlob(t, idx.TestDB(), bh, fmt.Sprintf("filler content %d", i))
		if err := idx.Upsert(ctx, testBranch, "abc", store.FactRecord{
			Path: path, Title: path, BlobHash: bh,
			Type: "observation", Domain: []string{"test"}, Entities: []string{"filler"},
			Confidence: 0.9, Sources: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Insert the needle — a hypothesis with a unique entity.
	insertTestBlob(t, idx.TestDB(), "blob_needle", "the needle fact")
	if err := idx.Upsert(ctx, testBranch, "abc", store.FactRecord{
		Path: "kb/needle.md", Title: "Needle", BlobHash: "blob_needle",
		Type: "hypothesis", Domain: []string{"special"}, Entities: []string{"rare-entity"},
		Confidence: 0.6, Sources: 1,
	}); err != nil {
		t.Fatal(err)
	}

	// Search by the unique entity — must find the needle even though
	// it's beyond the first 150 rows.
	results, err := idx.Search(ctx, testBranch, store.SearchQuery{Entities: []string{"rare-entity"}})
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
	results, err = idx.Search(ctx, testBranch, store.SearchQuery{Domain: []string{"special"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Path != "kb/needle.md" {
		t.Fatalf("expected 1 result for domain special, got %d", len(results))
	}
}

func TestSearchVecPathEntityFilter(t *testing.T) {
	ctx := context.Background()
	// Regression: the vector search path (text + entity filter) must apply
	// entity/domain filters in SQL, not in Go post-filtering.
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	vecA := testVec(1, 0, 0, 0)
	vecB := testVec(0.9, 0.4, 0, 0)
	vecQ := testVec(1, 0, 0, 0)

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
		return make([]float32, 768), nil
	}).AnyTimes()
	idx.SetEmbedder(emb)

	insertTestBlob(t, idx.TestDB(), "blob_va", "caching content")
	insertTestBlob(t, idx.TestDB(), "blob_vb", "caching content")

	if err := idx.Upsert(ctx, testBranch, "abc", store.FactRecord{
		Path: "kb/va.md", Title: "Alpha", BlobHash: "blob_va",
		Type: "observation", Domain: []string{"infra"}, Entities: []string{"Redis", "caching"},
		Confidence: 0.9, Sources: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Upsert(ctx, testBranch, "abc", store.FactRecord{
		Path: "kb/vb.md", Title: "Beta", BlobHash: "blob_vb",
		Type: "hypothesis", Domain: []string{"infra"}, Entities: []string{"Memcached", "caching"},
		Confidence: 0.7, Sources: 1,
	}); err != nil {
		t.Fatal(err)
	}

	// Text search without entity filter — both should match.
	results, err := idx.Search(ctx, testBranch, store.SearchQuery{Text: "caching", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results for text-only search, got %d", len(results))
	}

	// Text search + entity filter — only Redis fact should match.
	results, err = idx.Search(ctx, testBranch, store.SearchQuery{Text: "caching", Entities: []string{"Redis"}, Limit: 10})
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
	results, err = idx.Search(ctx, testBranch, store.SearchQuery{Text: "caching", Domain: []string{"infra"}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results for text+domain search, got %d", len(results))
	}

	// Text search + entity filter for hypothesis type — should still find it.
	results, err = idx.Search(ctx, testBranch, store.SearchQuery{Text: "caching", Entities: []string{"Memcached"}, Limit: 10})
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

func TestSearchVecLimitReturnsTopNByScoreWithBodies(t *testing.T) {
	ctx := context.Background()
	// Regression: the vector search path must return exactly Limit results,
	// ranked by score descending, with body content correctly loaded.
	// Previously, ALL candidate bodies were fetched before trimming to Limit,
	// wasting memory. After the fix, bodies are only fetched for the top-Limit
	// candidates. This test verifies the contract: correct results, correct order,
	// correct body content.
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	// Unit vectors with known dot-product similarity to query [1,0,0,0].
	facts := []struct {
		path  string
		vec   []float32
		body  string
		score float32 // expected cosine sim with query
	}{
		{"kb/rank1.md", testVec(1, 0, 0, 0), "body of rank one", 1.0},
		{"kb/rank2.md", testVec(0.9, 0.44, 0, 0), "body of rank two", 0.9},
		{"kb/rank3.md", testVec(0.8, 0.6, 0, 0), "body of rank three", 0.8},
		{"kb/rank4.md", testVec(0.7, 0.71, 0, 0), "body of rank four", 0.7},
		{"kb/rank5.md", testVec(0.6, 0.8, 0, 0), "body of rank five", 0.6},
	}

	queryVec := testVec(1, 0, 0, 0)

	// Upsert embeds using rec.Title + " " + extractBody(blob).
	// insertTestBlob wraps body with frontmatter; extractBody strips it back to f.body.
	// The search query embeds using q.Text directly.
	vecMap := map[string][]float32{}
	for _, f := range facts {
		vecMap[f.path+" "+f.body] = f.vec
	}
	vecMap["query"] = queryVec

	ctrl := gomock.NewController(t)
	emb := NewMockEmbedder(ctrl)
	emb.EXPECT().Embed(gomock.Any()).DoAndReturn(func(text string) ([]float32, error) {
		if v, ok := vecMap[text]; ok {
			return v, nil
		}
		return queryVec, nil
	}).AnyTimes()
	// SetEmbedder must be called before Upsert so embeddings are stored.
	idx.SetEmbedder(emb)

	for _, f := range facts {
		bh := "blob_" + f.path
		insertTestBlob(t, idx.TestDB(), bh, f.body)
		if err := idx.Upsert(ctx, testBranch, "abc", store.FactRecord{
			Path: f.path, Title: f.path, BlobHash: bh,
			Type: "observation", Domain: []string{"test"}, Entities: []string{},
			Confidence: 0.9, Sources: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}

	results, err := idx.Search(ctx, testBranch, store.SearchQuery{
		Text:  "query",
		Limit: 3,
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(results) != 3 {
		t.Fatalf("expected 3 results (limit), got %d", len(results))
	}

	// Results must be in descending score order.
	for i := 1; i < len(results); i++ {
		if results[i].Score > results[i-1].Score {
			t.Fatalf("results not sorted by score: index %d (%.2f) > index %d (%.2f)",
				i, results[i].Score, i-1, results[i-1].Score)
		}
	}

	// Top result must be rank1 with correct body.
	if results[0].Path != "kb/rank1.md" {
		t.Fatalf("expected rank1 as top result, got %s", results[0].Path)
	}
	if !strings.Contains(results[0].Body, "body of rank one") {
		t.Fatalf("expected rank1 body content, got %q", results[0].Body)
	}

	// rank4 and rank5 must NOT appear (they are beyond limit=3).
	for _, r := range results {
		if r.Path == "kb/rank4.md" || r.Path == "kb/rank5.md" {
			t.Fatalf("result beyond limit appeared: %s", r.Path)
		}
	}
}

func TestMatchesFiltersWithTypes(t *testing.T) {
	ctx := context.Background()
	// Test matchesFilters via Search with combined filters.
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	insertTestBlob(t, idx.TestDB(), "blob_a", "content a")
	insertTestBlob(t, idx.TestDB(), "blob_b", "content b")

	if err := idx.Upsert(ctx, testBranch, "abc", store.FactRecord{
		Path: "kb/a.md", Title: "A", BlobHash: "blob_a", Type: "observation",
		Domain: []string{"test"}, Entities: []string{}, Confidence: 0.9, Sources: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Upsert(ctx, testBranch, "abc", store.FactRecord{
		Path: "kb/b.md", Title: "B", BlobHash: "blob_b", Type: "hypothesis",
		Domain: []string{"test"}, Entities: []string{}, Confidence: 0.5, Sources: 1,
	}); err != nil {
		t.Fatal(err)
	}

	// Exclude hypothesis + min confidence filter
	results, err := idx.Search(ctx, testBranch, store.SearchQuery{
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

// upsertFact is a test helper that inserts a blob and upserts a FactRecord.
func upsertFact(t *testing.T, idx *store.Index, path, blobHash, typ string, domain, entities []string, confidence float64) {
	ctx := context.Background()
	t.Helper()
	insertTestBlob(t, idx.TestDB(), blobHash, "content of "+path)
	if err := idx.Upsert(ctx, testBranch, "abc", store.FactRecord{
		Path: path, Title: path, BlobHash: blobHash,
		Type: typ, Domain: domain, Entities: entities,
		Confidence: confidence, Sources: 1,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSearchPathFilter(t *testing.T) {
	ctx := context.Background()
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	upsertFact(t, idx, "kb/alpha/one.md", "bh1", "observation", []string{"test"}, nil, 0.9)
	upsertFact(t, idx, "kb/alpha/two.md", "bh2", "observation", []string{"test"}, nil, 0.9)
	upsertFact(t, idx, "kb/beta/three.md", "bh3", "observation", []string{"test"}, nil, 0.9)

	results, err := idx.Search(ctx, testBranch, store.SearchQuery{Path: "kb/alpha/"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results under kb/alpha/, got %d", len(results))
	}
	for _, r := range results {
		if !strings.HasPrefix(r.Path, "kb/alpha/") {
			t.Errorf("result %q is outside path prefix kb/alpha/", r.Path)
		}
	}

	// Exact prefix — should match nothing beyond that subtree.
	results, err = idx.Search(ctx, testBranch, store.SearchQuery{Path: "kb/beta/"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Path != "kb/beta/three.md" {
		t.Fatalf("expected only kb/beta/three.md, got %v", results)
	}
}

func TestSearchLimitTextlessPath(t *testing.T) {
	ctx := context.Background()
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	for i := 0; i < 10; i++ {
		upsertFact(t, idx, fmt.Sprintf("kb/f%02d.md", i), fmt.Sprintf("bh%d", i), "observation", []string{"test"}, nil, 0.9)
	}

	results, err := idx.Search(ctx, testBranch, store.SearchQuery{Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results (Limit=3), got %d", len(results))
	}
}

func TestSearchVecMinConfidence(t *testing.T) {
	ctx := context.Background()
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	ctrl := gomock.NewController(t)
	emb := NewMockEmbedder(ctrl)
	emb.EXPECT().Embed(gomock.Any()).Return(testVec(1, 0, 0, 0), nil).AnyTimes()
	idx.SetEmbedder(emb)

	upsertFact(t, idx, "kb/high.md", "bh_high", "observation", []string{"test"}, nil, 0.9)
	upsertFact(t, idx, "kb/low.md", "bh_low", "observation", []string{"test"}, nil, 0.3)

	results, err := idx.Search(ctx, testBranch, store.SearchQuery{Text: "q", MinConfidence: 0.8, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Path != "kb/high.md" {
		t.Fatalf("expected only kb/high.md (confidence>=0.8), got %v", results)
	}
}

func TestSearchVecIncludeTypes(t *testing.T) {
	ctx := context.Background()
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	ctrl := gomock.NewController(t)
	emb := NewMockEmbedder(ctrl)
	emb.EXPECT().Embed(gomock.Any()).Return(testVec(1, 0, 0, 0), nil).AnyTimes()
	idx.SetEmbedder(emb)

	upsertFact(t, idx, "kb/obs.md", "bh_obs", "observation", []string{"test"}, nil, 0.9)
	upsertFact(t, idx, "kb/hyp.md", "bh_hyp", "hypothesis", []string{"test"}, nil, 0.9)

	results, err := idx.Search(ctx, testBranch, store.SearchQuery{Text: "q", IncludeTypes: []string{"observation"}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Path != "kb/obs.md" {
		t.Fatalf("expected only kb/obs.md, got %v", results)
	}
}

func TestSearchVecExcludeTypes(t *testing.T) {
	ctx := context.Background()
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	ctrl := gomock.NewController(t)
	emb := NewMockEmbedder(ctrl)
	emb.EXPECT().Embed(gomock.Any()).Return(testVec(1, 0, 0, 0), nil).AnyTimes()
	idx.SetEmbedder(emb)

	upsertFact(t, idx, "kb/obs.md", "bh_obs", "observation", []string{"test"}, nil, 0.9)
	upsertFact(t, idx, "kb/hyp.md", "bh_hyp", "hypothesis", []string{"test"}, nil, 0.9)

	results, err := idx.Search(ctx, testBranch, store.SearchQuery{Text: "q", ExcludeTypes: []string{"hypothesis"}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Path != "kb/obs.md" {
		t.Fatalf("expected only kb/obs.md, got %v", results)
	}
}

func TestSearchVecPathFilter(t *testing.T) {
	ctx := context.Background()
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	ctrl := gomock.NewController(t)
	emb := NewMockEmbedder(ctrl)
	emb.EXPECT().Embed(gomock.Any()).Return(testVec(1, 0, 0, 0), nil).AnyTimes()
	idx.SetEmbedder(emb)

	upsertFact(t, idx, "kb/alpha/one.md", "bh_a1", "observation", []string{"test"}, nil, 0.9)
	upsertFact(t, idx, "kb/alpha/two.md", "bh_a2", "observation", []string{"test"}, nil, 0.9)
	upsertFact(t, idx, "kb/beta/three.md", "bh_b3", "observation", []string{"test"}, nil, 0.9)

	results, err := idx.Search(ctx, testBranch, store.SearchQuery{Text: "q", Path: "kb/alpha/", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results under kb/alpha/, got %d", len(results))
	}
	for _, r := range results {
		if !strings.HasPrefix(r.Path, "kb/alpha/") {
			t.Errorf("result %q is outside path prefix kb/alpha/", r.Path)
		}
	}
}

func TestSearchMinSimilarity(t *testing.T) {
	ctx := context.Background()
	// Two facts: one with high similarity, one below threshold.
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	// Upsert embeds title+" "+body. Blobs contain "distant" or "close" so we
	// can distinguish them without knowing the exact frontmatter format.
	ctrl := gomock.NewController(t)
	emb := NewMockEmbedder(ctrl)
	emb.EXPECT().Embed(gomock.Any()).DoAndReturn(func(text string) ([]float32, error) {
		if strings.Contains(text, "distant") {
			return testVec(0.5, 0.87, 0, 0), nil // cosine ~0.5 with [1,0,0,0]
		}
		return testVec(1, 0, 0, 0), nil
	}).AnyTimes()
	idx.SetEmbedder(emb)

	insertTestBlob(t, idx.TestDB(), "bh_high", "close match")
	insertTestBlob(t, idx.TestDB(), "bh_low", "distant match")
	if err := idx.Upsert(ctx, testBranch, "abc", store.FactRecord{
		Path: "kb/high.md", Title: "close", BlobHash: "bh_high",
		Type: "observation", Domain: []string{"test"}, Entities: []string{},
		Confidence: 0.9, Sources: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Upsert(ctx, testBranch, "abc", store.FactRecord{
		Path: "kb/low.md", Title: "distant", BlobHash: "bh_low",
		Type: "observation", Domain: []string{"test"}, Entities: []string{},
		Confidence: 0.9, Sources: 1,
	}); err != nil {
		t.Fatal(err)
	}

	// Default threshold (0.40) — both should match.
	results, err := idx.Search(ctx, testBranch, store.SearchQuery{Text: "q", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results with default threshold, got %d", len(results))
	}

	// High threshold (0.9) — only the high-similarity fact should match.
	results, err = idx.Search(ctx, testBranch, store.SearchQuery{Text: "q", MinSimilarity: 0.9, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Path != "kb/high.md" {
		t.Fatalf("expected only kb/high.md with MinSimilarity=0.9, got %v", results)
	}
}

func TestSearchAdaptiveKBranchMid(t *testing.T) {
	ctx := context.Background()
	// Behavioral test for the adaptive-k middle branch (MinSimilarity > 0.5 → kLimit = limit*3).
	// The "close" fact has cosine ~1.0 with the query; the "distant" fact has cosine ~0.5.
	// With MinSimilarity=0.6 the distant fact falls below the threshold, so only
	// the close fact should be returned — confirming Search works correctly with
	// the limit*3 candidate window.
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	ctrl := gomock.NewController(t)
	emb := NewMockEmbedder(ctrl)
	emb.EXPECT().Embed(gomock.Any()).DoAndReturn(func(text string) ([]float32, error) {
		if strings.Contains(text, "distant") {
			return testVec(0.5, 0.87, 0, 0), nil // cosine ~0.5 with [1,0,0,0]
		}
		return testVec(1, 0, 0, 0), nil
	}).AnyTimes()
	idx.SetEmbedder(emb)

	insertTestBlob(t, idx.TestDB(), "bh_high2", "close match")
	insertTestBlob(t, idx.TestDB(), "bh_low2", "distant match")
	if err := idx.Upsert(ctx, testBranch, "abc", store.FactRecord{
		Path: "kb/high2.md", Title: "close", BlobHash: "bh_high2",
		Type: "observation", Domain: []string{"test"}, Entities: []string{},
		Confidence: 0.9, Sources: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Upsert(ctx, testBranch, "abc", store.FactRecord{
		Path: "kb/low2.md", Title: "distant", BlobHash: "bh_low2",
		Type: "observation", Domain: []string{"test"}, Entities: []string{},
		Confidence: 0.9, Sources: 1,
	}); err != nil {
		t.Fatal(err)
	}

	// MinSimilarity=0.6 exercises the >0.5 branch (kLimit = limit*3).
	results, err := idx.Search(ctx, testBranch, store.SearchQuery{Text: "q", MinSimilarity: 0.6, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Path != "kb/high2.md" {
		t.Fatalf("expected only kb/high2.md with MinSimilarity=0.6, got %v", results)
	}
}

func TestSearchAdaptiveKBranchDefault(t *testing.T) {
	ctx := context.Background()
	// Behavioral test for the adaptive-k default branch (MinSimilarity=0 → kLimit = limit*5).
	// Both facts have cosine above the default 0.40 floor, so both should be returned —
	// confirming Search works correctly with the limit*5 candidate window.
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	ctrl := gomock.NewController(t)
	emb := NewMockEmbedder(ctrl)
	emb.EXPECT().Embed(gomock.Any()).DoAndReturn(func(text string) ([]float32, error) {
		if strings.Contains(text, "distant") {
			return testVec(0.5, 0.87, 0, 0), nil // cosine ~0.5 with [1,0,0,0]
		}
		return testVec(1, 0, 0, 0), nil
	}).AnyTimes()
	idx.SetEmbedder(emb)

	insertTestBlob(t, idx.TestDB(), "bh_high3", "close match")
	insertTestBlob(t, idx.TestDB(), "bh_low3", "distant match")
	if err := idx.Upsert(ctx, testBranch, "abc", store.FactRecord{
		Path: "kb/high3.md", Title: "close", BlobHash: "bh_high3",
		Type: "observation", Domain: []string{"test"}, Entities: []string{},
		Confidence: 0.9, Sources: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Upsert(ctx, testBranch, "abc", store.FactRecord{
		Path: "kb/low3.md", Title: "distant", BlobHash: "bh_low3",
		Type: "observation", Domain: []string{"test"}, Entities: []string{},
		Confidence: 0.9, Sources: 1,
	}); err != nil {
		t.Fatal(err)
	}

	// MinSimilarity=0 exercises the default branch (kLimit = limit*5).
	// Both facts clear the 0.40 default floor (cosine ~1.0 and ~0.5).
	results, err := idx.Search(ctx, testBranch, store.SearchQuery{Text: "q", MinSimilarity: 0, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results with MinSimilarity=0 (default branch), got %d", len(results))
	}
}

func TestSearchDomainPrefixFilter(t *testing.T) {
	ctx := context.Background()
	// Domain prefix filtering: querying for "technology" should match facts
	// with domain "technology" or "technology/go" but not "science".
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	facts := []struct {
		path     string
		blobHash string
		domain   []string
	}{
		{"kb/a.md", "blob_a", []string{"technology/go"}},
		{"kb/b.md", "blob_b", []string{"technology"}},
		{"kb/c.md", "blob_c", []string{"science"}},
		{"kb/d.md", "blob_d", []string{"technology/rust", "science/physics"}},
	}
	for _, f := range facts {
		insertTestBlob(t, idx.TestDB(), f.blobHash, "content for "+f.path)
		if err := idx.Upsert(ctx, testBranch, "abc", store.FactRecord{
			Path: f.path, Title: f.path, BlobHash: f.blobHash,
			Type: "observation", Domain: f.domain, Entities: []string{},
			Confidence: 0.9, Sources: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// "technology" should match a (technology/go), b (technology), d (technology/rust).
	results, err := idx.Search(ctx, testBranch, store.SearchQuery{Domain: []string{"technology"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results for domain prefix 'technology', got %d", len(results))
	}
	paths := map[string]bool{}
	for _, r := range results {
		paths[r.Path] = true
	}
	if !paths["kb/a.md"] || !paths["kb/b.md"] || !paths["kb/d.md"] {
		t.Fatalf("expected a, b, d; got %v", paths)
	}

	// Combined domain prefix: "technology" AND "science" should match only d.
	results, err = idx.Search(ctx, testBranch, store.SearchQuery{Domain: []string{"technology", "science"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Path != "kb/d.md" {
		t.Fatalf("expected only kb/d.md for combined domain filter, got %d results", len(results))
	}
}
