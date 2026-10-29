package index

import (
	"testing"

	"knomit/internal/engine/store/facts"
)

// Vector hits rank by cosine, highest first, and equal cosines by path: a
// tie has one order whatever order the metadata query returned the rows in,
// so the UI's results list and a book materializing the same search
// (materialize.runBookSearch) agree on it (review of PR #692, N2).
//
// Sabotage: drop the path tie-break, and the tied rows keep their input
// order.
func TestRankCandidates_TiesByPath(t *testing.T) {
	hit := func(path string, score float64) searchCandidate {
		return searchCandidate{rec: facts.FactWithBody{FactRecord: facts.FactRecord{Path: path}}, score: score}
	}
	c := []searchCandidate{
		hit("kb/t/c.md", 0.9), hit("kb/t/a.md", 0.5), hit("kb/t/b.md", 0.9),
		hit("kb/t/e.md", 0.95), hit("kb/t/a2.md", 0.9), hit("kb/t/d.md", 0.5),
	}
	rankCandidates(c)
	var got []string
	for _, x := range c {
		got = append(got, x.rec.Path)
	}
	want := []string{"kb/t/e.md", "kb/t/a2.md", "kb/t/b.md", "kb/t/c.md", "kb/t/a.md", "kb/t/d.md"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rank order\n got %v\nwant %v", got, want)
		}
	}
}
