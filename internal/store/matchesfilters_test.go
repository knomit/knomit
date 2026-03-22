package store

import "testing"

func TestMatchesFiltersIncludeTypes(t *testing.T) {
	rec := FactRecord{Type: "hypothesis", Path: "kb/test/a.md", Confidence: 0.5}
	q := SearchQuery{IncludeTypes: []string{"observation"}}
	if matchesFilters(rec, q) {
		t.Error("hypothesis should not match IncludeTypes=[observation]")
	}
	q2 := SearchQuery{IncludeTypes: []string{"hypothesis"}}
	if !matchesFilters(rec, q2) {
		t.Error("hypothesis should match IncludeTypes=[hypothesis]")
	}
}

func TestMatchesFiltersExcludeTypes(t *testing.T) {
	rec := FactRecord{Type: "hypothesis", Path: "kb/test/a.md", Confidence: 0.5}
	q := SearchQuery{ExcludeTypes: []string{"hypothesis"}}
	if matchesFilters(rec, q) {
		t.Error("hypothesis should be excluded by ExcludeTypes=[hypothesis]")
	}
	q2 := SearchQuery{ExcludeTypes: []string{"observation"}}
	if !matchesFilters(rec, q2) {
		t.Error("hypothesis should not be excluded by ExcludeTypes=[observation]")
	}
}

func TestMatchesFiltersEmpty(t *testing.T) {
	rec := FactRecord{Type: "observation", Path: "kb/test/a.md", Confidence: 0.5}
	q := SearchQuery{}
	if !matchesFilters(rec, q) {
		t.Error("empty filters should match all")
	}
}

func TestMatchesFiltersEntities(t *testing.T) {
	rec := FactRecord{Type: "observation", Path: "kb/test/a.md", Entities: []string{"Go", "Rust"}}
	q := SearchQuery{Entities: []string{"Go"}}
	if !matchesFilters(rec, q) {
		t.Error("should match when entity present")
	}
	q2 := SearchQuery{Entities: []string{"Python"}}
	if matchesFilters(rec, q2) {
		t.Error("should not match when entity missing")
	}
}

func TestMatchesFiltersDomain(t *testing.T) {
	rec := FactRecord{Type: "observation", Path: "kb/test/a.md", Domain: []string{"tech/go"}}
	q := SearchQuery{Domain: []string{"tech"}}
	if !matchesFilters(rec, q) {
		t.Error("should match domain prefix")
	}
	q2 := SearchQuery{Domain: []string{"science"}}
	if matchesFilters(rec, q2) {
		t.Error("should not match unrelated domain")
	}
}

func TestMatchesFiltersPathPrefix(t *testing.T) {
	rec := FactRecord{Type: "observation", Path: "kb/tech/go/a.md"}
	q := SearchQuery{Path: "kb/tech"}
	if !matchesFilters(rec, q) {
		t.Error("should match path prefix")
	}
	q2 := SearchQuery{Path: "kb/science"}
	if matchesFilters(rec, q2) {
		t.Error("should not match unrelated path prefix")
	}
}

func TestMatchesFiltersMinConfidence(t *testing.T) {
	rec := FactRecord{Type: "observation", Path: "kb/a.md", Confidence: 0.5}
	q := SearchQuery{MinConfidence: 0.8}
	if matchesFilters(rec, q) {
		t.Error("should not match when confidence below threshold")
	}
	q2 := SearchQuery{MinConfidence: 0.3}
	if !matchesFilters(rec, q2) {
		t.Error("should match when confidence above threshold")
	}
}

func TestMatchesFiltersCombined(t *testing.T) {
	rec := FactRecord{
		Type:       "hypothesis",
		Path:       "kb/tech/go/a.md",
		Confidence: 0.9,
		Entities:   []string{"goroutine"},
		Domain:     []string{"tech/go"},
	}
	q := SearchQuery{
		IncludeTypes:  []string{"hypothesis", "synthesis"},
		Path:          "kb/tech",
		MinConfidence: 0.5,
		Entities:      []string{"goroutine"},
		Domain:        []string{"tech"},
	}
	if !matchesFilters(rec, q) {
		t.Error("should match combined filters")
	}

	// Fail on one dimension
	q.MinConfidence = 0.95
	if matchesFilters(rec, q) {
		t.Error("should not match when confidence is below threshold")
	}
}
