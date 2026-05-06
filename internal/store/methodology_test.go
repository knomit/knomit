package store

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// methFactBody builds a methodology fact body for tests.
func methFactBody(title, body string, domains, entities []string) string {
	f := fact.NewFact("placeholder.md")
	f.Title = title
	f.Body = body
	f.Type = "methodology"
	f.Confidence = 0.7
	f.Sources = 1
	f.Domain = domains
	f.Entities = entities
	return fact.SerializeFact(f)
}

// TestRelevantMethodology_FiltersByTypeAndBranch verifies the candidate set
// is exactly the methodology facts visible on the requested branch — not
// other types, not other branches.
func TestRelevantMethodology_FiltersByTypeAndBranch(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/a"))

	ctx := context.Background()
	branch := "agent/a"

	// Two methodology facts on agent/a, one regular observation, one
	// methodology on a different branch.
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/meta/reasoning/m1.md",
		methFactBody("M1", "first lesson", []string{"meta", "reasoning", "methodology", "security"}, []string{"Anthropic"}),
		"add m1", "")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/meta/reasoning/m2.md",
		methFactBody("M2", "second lesson", []string{"meta", "reasoning", "methodology"}, nil),
		"add m2", "")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/obs/x.md",
		testFactBody("regular obs", 0.9, nil), "add obs", "")
	require.NoError(t, err)

	// Methodology written on a different branch should be invisible.
	// CreateBranch first so WriteFact can resolve the git ref.
	require.NoError(t, svc.Branches().CreateBranch(ctx, "agent/b", branch))
	_, err = svc.Facts().WriteFact(ctx, "agent/b", "kb/meta/reasoning/m3.md",
		methFactBody("M3", "branch-b lesson", []string{"meta", "reasoning", "methodology"}, nil),
		"add m3 on b", "")
	require.NoError(t, err)

	got, err := svc.Search().RelevantMethodology(ctx, branch, "anything", nil, nil, 10)
	require.NoError(t, err)

	paths := make([]string, len(got))
	for i, m := range got {
		paths[i] = m.Path
	}
	require.ElementsMatch(t, []string{"kb/meta/reasoning/m1.md", "kb/meta/reasoning/m2.md"}, paths)

	// Verify body hydration: each result has the body we wrote.
	byPath := map[string]MethodologyMatch{}
	for _, m := range got {
		byPath[m.Path] = m
	}
	require.Equal(t, "first lesson", byPath["kb/meta/reasoning/m1.md"].Body)
	require.Equal(t, "second lesson", byPath["kb/meta/reasoning/m2.md"].Body)
}

// TestRelevantMethodology_EmptyCandidateSet verifies graceful empty return
// (not an error) when a branch has no methodology facts.
func TestRelevantMethodology_EmptyCandidateSet(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/a"))

	got, err := svc.Search().RelevantMethodology(context.Background(), "agent/a", "anything", nil, nil, 10)
	require.NoError(t, err)
	require.Empty(t, got)
}

// TestFormatMethodologySection_NonEmpty verifies the prompt-section
// formatter renders header + per-match title/path/score lines with the
// body intentionally omitted (the model fetches via knomit_query on
// demand). Pure function (no DB) — used by mcp and synthesize callers.
func TestFormatMethodologySection_NonEmpty(t *testing.T) {
	matches := []MethodologyMatch{
		{Path: "kb/meta/reasoning/m1.md", Title: "M1 title", Body: "M1 body about evidence weighting.", Score: 0.87},
		{Path: "kb/meta/reasoning/m2.md", Title: "M2 title", Body: "M2 body about pitfall detection.", Score: 0.62},
	}
	got := FormatMethodologySection(matches, 0.0)
	require.Contains(t, got, "Applicable methodology (ranked candidates")
	require.Contains(t, got, "fetch via knomit_query")
	require.Contains(t, got, "M1 title")
	require.Contains(t, got, "kb/meta/reasoning/m1.md")
	require.Contains(t, got, "score=0.87")
	require.Contains(t, got, "score=0.62")
	require.NotContains(t, got, "M1 body about evidence weighting.")
}

// TestFormatMethodologySection_Empty returns empty string for empty input
// so callers can omit the entire section.
func TestFormatMethodologySection_Empty(t *testing.T) {
	require.Equal(t, "", FormatMethodologySection(nil, 0.0))
	require.Equal(t, "", FormatMethodologySection([]MethodologyMatch{}, 0.0))
}

// TestFormatMethodologySection_BelowThresholdDropped drops matches whose
// composite score falls below the configured floor, retaining only
// candidates at or above the threshold.
func TestFormatMethodologySection_BelowThresholdDropped(t *testing.T) {
	matches := []MethodologyMatch{
		{Path: "kb/meta/reasoning/keep.md", Title: "Keep", Score: 0.20},
		{Path: "kb/meta/reasoning/drop1.md", Title: "Drop1", Score: 0.10},
		{Path: "kb/meta/reasoning/drop2.md", Title: "Drop2", Score: 0.05},
	}
	got := FormatMethodologySection(matches, 0.15)
	require.Contains(t, got, "Keep")
	require.Contains(t, got, "kb/meta/reasoning/keep.md")
	require.NotContains(t, got, "Drop1")
	require.NotContains(t, got, "Drop2")
	// Only one bullet rendered.
	require.Equal(t, 1, strings.Count(got, "\n•"))
}

// TestFormatMethodologySection_AllBelowReturnsEmpty returns "" when every
// candidate is below the threshold, so callers can omit the section.
func TestFormatMethodologySection_AllBelowReturnsEmpty(t *testing.T) {
	matches := []MethodologyMatch{
		{Path: "kb/meta/reasoning/a.md", Title: "A", Score: 0.05},
		{Path: "kb/meta/reasoning/b.md", Title: "B", Score: 0.10},
	}
	require.Equal(t, "", FormatMethodologySection(matches, 0.15))
}

// TestRelevantMethodology_TagOverlap_RanksByMatch verifies the tag-overlap
// scoring: a methodology with both source domain AND source entity matching
// outranks one with only one matching, which outranks one with no matching.
// Verifies the universal-marker exclusion (meta/reasoning/methodology don't
// count toward overlap).
func TestRelevantMethodology_TagOverlap_RanksByMatch(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/a"))

	ctx := context.Background()
	branch := "agent/a"

	// Three methodology facts with progressively-stronger overlap to a
	// source tagged domain=[security], entities=[Anthropic].
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/meta/reasoning/full.md",
		methFactBody("Full", "full match",
			[]string{"meta", "reasoning", "methodology", "security"},
			[]string{"Anthropic"}),
		"full", "")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/meta/reasoning/halfdom.md",
		methFactBody("HalfDomain", "domain match only",
			[]string{"meta", "reasoning", "methodology", "security"},
			[]string{"OpenAI"}),
		"half-domain", "")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/meta/reasoning/markersonly.md",
		methFactBody("MarkersOnly", "only meta tags",
			[]string{"meta", "reasoning", "methodology"},
			nil),
		"markers-only", "")
	require.NoError(t, err)

	got, err := svc.Search().RelevantMethodology(ctx, branch, "any source body",
		[]string{"security"}, []string{"Anthropic"}, 10)
	require.NoError(t, err)
	require.Len(t, got, 3)

	// Ranking: full > halfdom > markersonly.
	require.Equal(t, "kb/meta/reasoning/full.md", got[0].Path)
	require.Equal(t, "kb/meta/reasoning/halfdom.md", got[1].Path)
	require.Equal(t, "kb/meta/reasoning/markersonly.md", got[2].Path)

	// Universal markers (meta/reasoning/methodology) are excluded from
	// overlap calc — markersonly has TagOverlap == 0.
	require.Equal(t, 0.0, got[2].TagOverlap)

	// Full match: domain_overlap=1.0, entity_overlap=1.0 → tag_overlap=1.0.
	require.InDelta(t, 1.0, got[0].TagOverlap, 0.001)

	// Half match: domain_overlap=1.0, entity_overlap=0.0 → tag_overlap=0.5.
	require.InDelta(t, 0.5, got[1].TagOverlap, 0.001)

	// MatchedDomains/Entities expose what actually matched.
	require.Equal(t, []string{"security"}, got[0].MatchedDomains)
	require.Equal(t, []string{"Anthropic"}, got[0].MatchedEntities)
	require.Empty(t, got[2].MatchedDomains)
	require.Empty(t, got[2].MatchedEntities)
}

// TestRelevantMethodology_TagOverlap_EmptySourceTags handles the edge case
// where the source has no tags. Tag-overlap collapses to 0 for everyone;
// candidates are still returned (Task 3 will rank them via vector).
func TestRelevantMethodology_TagOverlap_EmptySourceTags(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/a"))

	_, err = svc.Facts().WriteFact(context.Background(), "agent/a", "kb/meta/reasoning/m.md",
		methFactBody("M", "body",
			[]string{"meta", "reasoning", "methodology", "security"},
			[]string{"Anthropic"}),
		"add m", "")
	require.NoError(t, err)

	got, err := svc.Search().RelevantMethodology(context.Background(), "agent/a", "src",
		nil, nil, 10)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, 0.0, got[0].TagOverlap)
}

// TestRelevantMethodology_VectorOnly_Fallback verifies that when source
// has no tag overlap, ranking still works via vector similarity.
// Modeling the 28%/20% gap: methodology with empty domain/entities (only
// meta markers) is still retrievable when its body is semantically related.
func TestRelevantMethodology_VectorOnly_Fallback(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	svc.SetEmbedder(&stub768Embedder{})
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/a"))

	ctx := context.Background()
	branch := "agent/a"

	// Two methodology facts, neither has source-overlapping tags. Vector
	// ranks them by body similarity to source body.
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/meta/reasoning/close.md",
		methFactBody("Close", "When evaluating evidence weighting under uncertainty",
			[]string{"meta", "reasoning", "methodology"}, nil),
		"close", "")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/meta/reasoning/far.md",
		methFactBody("Far", "completely unrelated topic about cooking pasta",
			[]string{"meta", "reasoning", "methodology"}, nil),
		"far", "")
	require.NoError(t, err)

	// stub768Embedder hashes by len(text), so two distinct-length bodies
	// yield two distinct vectors. The exact ordering depends on the stub;
	// the test asserts only that BOTH are returned and have non-zero
	// VectorScore — confirming vector retrieval is wired.
	got, err := svc.Search().RelevantMethodology(ctx, branch,
		"evidence weighting under uncertainty",
		nil, nil, 10)
	require.NoError(t, err)
	require.Len(t, got, 2)
	for _, m := range got {
		require.Greater(t, m.VectorScore, 0.0,
			"vector score must be non-zero when embedder is configured: %+v", m)
	}
}

// TestRelevantMethodology_Composite_FormulaIsApplied verifies the score
// formula Score == 0.6·VectorScore + 0.4·TagOverlap holds for every
// returned match. Avoids asserting any particular ordering against the
// stub embedder (whose cosine similarities are deterministic but
// content-independent), instead asserting the per-match invariant.
func TestRelevantMethodology_Composite_FormulaIsApplied(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	svc.SetEmbedder(&stub768Embedder{})
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/a"))

	ctx := context.Background()
	branch := "agent/a"

	_, err = svc.Facts().WriteFact(ctx, branch, "kb/meta/reasoning/tagged.md",
		methFactBody("Tagged", "different content but tags match",
			[]string{"meta", "reasoning", "methodology", "security"},
			[]string{"Anthropic"}),
		"tagged", "")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/meta/reasoning/vectored.md",
		methFactBody("Vectored", "exact source body for high vector score",
			[]string{"meta", "reasoning", "methodology"}, nil),
		"vectored", "")
	require.NoError(t, err)

	got, err := svc.Search().RelevantMethodology(ctx, branch,
		"exact source body for high vector score",
		[]string{"security"}, []string{"Anthropic"}, 10)
	require.NoError(t, err)
	require.Len(t, got, 2)

	// Per-match invariant: Score == 0.6·VectorScore + 0.4·TagOverlap.
	for _, m := range got {
		expected := 0.6*m.VectorScore + 0.4*m.TagOverlap
		require.InDelta(t, expected, m.Score, 0.0001,
			"composite formula violated for %s: vec=%f, tag=%f, score=%f",
			m.Path, m.VectorScore, m.TagOverlap, m.Score)
	}

	// Tagged has full tag overlap; Vectored has none.
	byPath := map[string]MethodologyMatch{}
	for _, m := range got {
		byPath[m.Path] = m
	}
	require.InDelta(t, 1.0, byPath["kb/meta/reasoning/tagged.md"].TagOverlap, 0.0001)
	require.InDelta(t, 0.0, byPath["kb/meta/reasoning/vectored.md"].TagOverlap, 0.0001)
}

// TestRelevantMethodology_Composite_TagWeightWinsAtFullMatch verifies that
// with no embedder configured, tag-only ranking still works as a fallback
// (VectorScore stays 0 for all candidates; ranking falls back to
// 0.4·TagOverlap = TagOverlap, scaled).
func TestRelevantMethodology_Composite_TagWeightWinsAtFullMatch(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	// No embedder: VectorScore stays 0 for all candidates.
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/a"))

	ctx := context.Background()
	branch := "agent/a"

	_, err = svc.Facts().WriteFact(ctx, branch, "kb/meta/reasoning/tagged.md",
		methFactBody("Tagged", "body",
			[]string{"meta", "reasoning", "methodology", "security"},
			[]string{"Anthropic"}),
		"tagged", "")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/meta/reasoning/untagged.md",
		methFactBody("Untagged", "body",
			[]string{"meta", "reasoning", "methodology"}, nil),
		"untagged", "")
	require.NoError(t, err)

	got, err := svc.Search().RelevantMethodology(ctx, branch, "src",
		[]string{"security"}, []string{"Anthropic"}, 10)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, "kb/meta/reasoning/tagged.md", got[0].Path,
		"with no embedder, tag-overlap=1.0 must outrank tag-overlap=0")
}

// TestRelevantMethodology_TopK limits the result count.
func TestRelevantMethodology_TopK(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	svc.SetEmbedder(&stub768Embedder{})
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/a"))

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		path := fmt.Sprintf("kb/meta/reasoning/m%d.md", i)
		title := fmt.Sprintf("M%d", i)
		_, err = svc.Facts().WriteFact(ctx, "agent/a", path,
			methFactBody(title, "body", []string{"meta", "reasoning", "methodology"}, nil),
			"add", "")
		require.NoError(t, err)
	}

	got, err := svc.Search().RelevantMethodology(ctx, "agent/a", "src", nil, nil, 3)
	require.NoError(t, err)
	require.Len(t, got, 3)
}

// TestRelevantMethodology_VectorCoverage_WithNoiseInIndex regresses the bug
// where sqlite-vec's `MATCH ... AND k = N` applied N as a global top-k
// before the type='methodology' filter, causing methodology rows to fall
// outside the window when the index has many non-methodology facts.
//
// Seeds many non-methodology facts to push methodology rows into the long
// tail by distance, then asserts every methodology candidate still gets a
// non-zero VectorScore.
func TestRelevantMethodology_VectorCoverage_WithNoiseInIndex(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	svc.SetEmbedder(&stub768Embedder{})
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/a"))

	ctx := context.Background()
	branch := "agent/a"

	// Seed 30 non-methodology observation facts (the "noise" the KNN
	// global top-k window would otherwise consume).
	for i := 0; i < 30; i++ {
		path := fmt.Sprintf("kb/obs/n%d.md", i)
		body := fmt.Sprintf("noise body %d with varied content", i)
		_, err = svc.Facts().WriteFact(ctx, branch, path, testFactBody(body, 0.5, nil), "noise", "")
		require.NoError(t, err)
	}

	// Seed 3 methodology facts. With the bug (k = len(cands) = 3), only
	// the closest 3 vectors GLOBALLY are considered; in a 33-fact index,
	// the methodology rows are unlikely to all be in that window.
	for i := 0; i < 3; i++ {
		path := fmt.Sprintf("kb/meta/reasoning/m%d.md", i)
		_, err = svc.Facts().WriteFact(ctx, branch, path,
			methFactBody(fmt.Sprintf("M%d", i), fmt.Sprintf("methodology body %d", i),
				[]string{"meta", "reasoning", "methodology"}, nil),
			"meth", "")
		require.NoError(t, err)
	}

	got, err := svc.Search().RelevantMethodology(ctx, branch,
		"some source body for vector ranking",
		nil, nil, 10)
	require.NoError(t, err)
	require.Len(t, got, 3)

	// Every methodology row must have a non-zero VectorScore — the bug
	// would leave most at 0.
	for _, m := range got {
		require.Greater(t, m.VectorScore, 0.0,
			"methodology candidate %s has VectorScore=0 — KNN window did not cover it", m.Path)
	}
}
