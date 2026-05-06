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
	out, err := fact.SerializeFact(f)
	if err != nil {
		panic(err)
	}
	return out
}

// srcFactBody builds a non-methodology source fact (synthesis by default)
// whose body and tags drive RelevantMethodologyForFact's retrieval.
func srcFactBody(title, body string, domains, entities []string) string {
	f := fact.NewFact("placeholder.md")
	f.Title = title
	f.Body = body
	f.Type = fact.Synthesis
	f.Confidence = 0.9
	f.Sources = 1
	f.Domain = domains
	f.Entities = entities
	out, err := fact.SerializeFact(f)
	if err != nil {
		panic(err)
	}
	return out
}

// writeSrcFact writes a source synthesis fact and returns its path.
// Use this BEFORE calling RelevantMethodologyForFact so the fact has a
// row in branch_facts (and, if an embedder is set, in facts_vec).
func writeSrcFact(t *testing.T, svc *Service, branch, path, body string, doms, ents []string) {
	t.Helper()
	_, err := svc.Facts().WriteFact(context.Background(), branch, path,
		srcFactBody("source", body, doms, ents), "src", "")
	require.NoError(t, err)
}

// TestRelevantMethodologyForFact_FiltersByTypeAndBranch verifies the
// candidate set is exactly the methodology facts visible on the
// requested branch — not other types, not other branches.
func TestRelevantMethodologyForFact_FiltersByTypeAndBranch(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/a"))

	ctx := context.Background()
	branch := "agent/a"

	// Source fact on agent/a — RelevantMethodologyForFact retrieves
	// against this fact's identity.
	writeSrcFact(t, svc, branch, "kb/synth/src.md", "source body",
		[]string{"security"}, []string{"Anthropic"})

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
	require.NoError(t, svc.Branches().CreateBranch(ctx, "agent/b", branch))
	_, err = svc.Facts().WriteFact(ctx, "agent/b", "kb/meta/reasoning/m3.md",
		methFactBody("M3", "branch-b lesson", []string{"meta", "reasoning", "methodology"}, nil),
		"add m3 on b", "")
	require.NoError(t, err)

	got, err := svc.Search().RelevantMethodologyForFact(ctx, branch,
		"kb/synth/src.md", nil, nil, 10, 0.0)
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

// TestRelevantMethodologyForFact_EmptyCandidateSet verifies graceful
// empty return (not an error) when a branch has no methodology facts.
func TestRelevantMethodologyForFact_EmptyCandidateSet(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/a"))

	writeSrcFact(t, svc, "agent/a", "kb/synth/src.md", "x", nil, nil)

	got, err := svc.Search().RelevantMethodologyForFact(context.Background(), "agent/a",
		"kb/synth/src.md", nil, nil, 10, 0.0)
	require.NoError(t, err)
	require.Empty(t, got)
}

// TestRelevantMethodologyForFact_SourceFactMissing returns an error when
// the source path doesn't resolve on the branch — this is a programming
// error at the call site, not a degraded-but-functional condition.
//
// The fetch falls through gracefully (tag-only ranking) only when the
// source fact exists but lacks an embedding. A nonexistent source path
// is a different failure: the entire branch_facts JOIN finds nothing.
// In that case the helper still returns nil candidates if methodology
// also doesn't exist, or errors at the candidate query if it does.
func TestRelevantMethodologyForFact_SourceFactMissing_DegradesToTagOnly(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/a"))

	// Methodology exists but no source fact at this path.
	_, err = svc.Facts().WriteFact(context.Background(), "agent/a",
		"kb/meta/reasoning/m.md",
		methFactBody("M", "body", []string{"meta", "reasoning", "methodology", "security"}, []string{"Anthropic"}),
		"add m", "")
	require.NoError(t, err)

	// Missing source path → no embedding row → falls through to tag-only.
	// Tag overlap with src tags is still computed, so the methodology surfaces.
	got, err := svc.Search().RelevantMethodologyForFact(context.Background(), "agent/a",
		"kb/synth/nonexistent.md",
		[]string{"security"}, []string{"Anthropic"},
		10, 0.0)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.InDelta(t, 1.0, got[0].TagOverlap, 0.001)
	require.Equal(t, 0.0, got[0].VectorScore, "no source vector → vector score must be zero")
}

// TestFormatMethodologySection_NonEmpty verifies the prompt-section
// formatter renders only ranked bullet lines (title + path + score),
// with no leading heading and with bodies omitted. Each call site
// supplies its own heading wording.
func TestFormatMethodologySection_NonEmpty(t *testing.T) {
	matches := []MethodologyMatch{
		{Path: "kb/meta/reasoning/m1.md", Title: "M1 title", Body: "M1 body about evidence weighting.", Score: 0.87},
		{Path: "kb/meta/reasoning/m2.md", Title: "M2 title", Body: "M2 body about pitfall detection.", Score: 0.62},
	}
	got := FormatMethodologySection(matches)
	require.NotContains(t, got, "Applicable methodology",
		"formatter must not emit a heading; callers own framing")
	require.NotContains(t, got, "Existing methodology")
	require.Contains(t, got, "M1 title")
	require.Contains(t, got, "kb/meta/reasoning/m1.md")
	require.Contains(t, got, "score=0.87")
	require.Contains(t, got, "score=0.62")
	require.NotContains(t, got, "M1 body about evidence weighting.")
	require.Equal(t, 2, strings.Count(got, "•"))
}

// TestFormatMethodologySection_Empty returns empty string for empty input
// so callers can omit the entire section.
func TestFormatMethodologySection_Empty(t *testing.T) {
	require.Equal(t, "", FormatMethodologySection(nil))
	require.Equal(t, "", FormatMethodologySection([]MethodologyMatch{}))
}

// TestRelevantMethodologyForFact_TagOverlap_RanksByMatch verifies the
// tag-overlap scoring: a methodology with both source domain AND source
// entity matching outranks one with only one matching, which outranks
// one with no matching. Verifies the universal-marker exclusion
// (meta/reasoning/methodology don't count toward overlap).
func TestRelevantMethodologyForFact_TagOverlap_RanksByMatch(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/a"))

	ctx := context.Background()
	branch := "agent/a"

	writeSrcFact(t, svc, branch, "kb/synth/src.md", "source body",
		[]string{"security"}, []string{"Anthropic"})

	// Three methodology facts with progressively-stronger overlap.
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

	got, err := svc.Search().RelevantMethodologyForFact(ctx, branch,
		"kb/synth/src.md",
		[]string{"security"}, []string{"Anthropic"},
		10, 0.0)
	require.NoError(t, err)
	require.Len(t, got, 3)

	// Ranking: full > halfdom > markersonly.
	require.Equal(t, "kb/meta/reasoning/full.md", got[0].Path)
	require.Equal(t, "kb/meta/reasoning/halfdom.md", got[1].Path)
	require.Equal(t, "kb/meta/reasoning/markersonly.md", got[2].Path)

	require.Equal(t, 0.0, got[2].TagOverlap)
	require.InDelta(t, 1.0, got[0].TagOverlap, 0.001)
	require.InDelta(t, 0.5, got[1].TagOverlap, 0.001)

	require.Equal(t, []string{"security"}, got[0].MatchedDomains)
	require.Equal(t, []string{"Anthropic"}, got[0].MatchedEntities)
	require.Empty(t, got[2].MatchedDomains)
	require.Empty(t, got[2].MatchedEntities)
}

// TestRelevantMethodologyForFact_TagOverlap_EmptySourceTags handles the
// edge case where the source has no tags. Tag-overlap collapses to 0
// for everyone; candidates are still returned (vector ranking only).
func TestRelevantMethodologyForFact_TagOverlap_EmptySourceTags(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/a"))

	writeSrcFact(t, svc, "agent/a", "kb/synth/src.md", "src", nil, nil)

	_, err = svc.Facts().WriteFact(context.Background(), "agent/a", "kb/meta/reasoning/m.md",
		methFactBody("M", "body",
			[]string{"meta", "reasoning", "methodology", "security"},
			[]string{"Anthropic"}),
		"add m", "")
	require.NoError(t, err)

	got, err := svc.Search().RelevantMethodologyForFact(context.Background(), "agent/a",
		"kb/synth/src.md", nil, nil, 10, 0.0)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, 0.0, got[0].TagOverlap)
}

// TestRelevantMethodologyForFact_VectorOnly_Fallback verifies that when
// source has no tag overlap, ranking still works via vector similarity
// against the source fact's stored embedding.
func TestRelevantMethodologyForFact_VectorOnly_Fallback(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	svc.SetEmbedder(&stub768Embedder{})
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/a"))

	ctx := context.Background()
	branch := "agent/a"

	writeSrcFact(t, svc, branch, "kb/synth/src.md", "evidence weighting under uncertainty", nil, nil)

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

	got, err := svc.Search().RelevantMethodologyForFact(ctx, branch,
		"kb/synth/src.md", nil, nil, 10, 0.0)
	require.NoError(t, err)
	require.Len(t, got, 2)
	for _, m := range got {
		require.Greater(t, m.VectorScore, 0.0,
			"vector score must be non-zero when source has stored embedding: %+v", m)
	}
}

// TestRelevantMethodologyForFact_Composite_FormulaIsApplied verifies the
// score formula Score == 0.6·VectorScore + 0.4·TagOverlap holds for
// every returned match.
func TestRelevantMethodologyForFact_Composite_FormulaIsApplied(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	svc.SetEmbedder(&stub768Embedder{})
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/a"))

	ctx := context.Background()
	branch := "agent/a"

	writeSrcFact(t, svc, branch, "kb/synth/src.md",
		"exact source body for high vector score",
		[]string{"security"}, []string{"Anthropic"})

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

	got, err := svc.Search().RelevantMethodologyForFact(ctx, branch,
		"kb/synth/src.md",
		[]string{"security"}, []string{"Anthropic"},
		10, 0.0)
	require.NoError(t, err)
	require.Len(t, got, 2)

	for _, m := range got {
		expected := 0.6*m.VectorScore + 0.4*m.TagOverlap
		require.InDelta(t, expected, m.Score, 0.0001,
			"composite formula violated for %s: vec=%f, tag=%f, score=%f",
			m.Path, m.VectorScore, m.TagOverlap, m.Score)
	}

	byPath := map[string]MethodologyMatch{}
	for _, m := range got {
		byPath[m.Path] = m
	}
	require.InDelta(t, 1.0, byPath["kb/meta/reasoning/tagged.md"].TagOverlap, 0.0001)
	require.InDelta(t, 0.0, byPath["kb/meta/reasoning/vectored.md"].TagOverlap, 0.0001)
}

// TestRelevantMethodologyForFact_NoEmbedder_TagOnly verifies that with
// no embedder configured, the source fact has no embedding row and
// ranking falls back to tag-only.
func TestRelevantMethodologyForFact_NoEmbedder_TagOnly(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	// No embedder: facts_vec stays empty.
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/a"))

	ctx := context.Background()
	branch := "agent/a"

	writeSrcFact(t, svc, branch, "kb/synth/src.md", "src",
		[]string{"security"}, []string{"Anthropic"})

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

	got, err := svc.Search().RelevantMethodologyForFact(ctx, branch,
		"kb/synth/src.md",
		[]string{"security"}, []string{"Anthropic"},
		10, 0.0)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, "kb/meta/reasoning/tagged.md", got[0].Path,
		"with no embedder, tag-overlap=1.0 must outrank tag-overlap=0")
	for _, m := range got {
		require.Equal(t, 0.0, m.VectorScore, "no embedder → no vector score")
	}
}

// TestRelevantMethodologyForFact_TopK limits the result count.
func TestRelevantMethodologyForFact_TopK(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	svc.SetEmbedder(&stub768Embedder{})
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/a"))

	ctx := context.Background()
	writeSrcFact(t, svc, "agent/a", "kb/synth/src.md", "src", nil, nil)

	for i := 0; i < 5; i++ {
		path := fmt.Sprintf("kb/meta/reasoning/m%d.md", i)
		title := fmt.Sprintf("M%d", i)
		_, err = svc.Facts().WriteFact(ctx, "agent/a", path,
			methFactBody(title, "body", []string{"meta", "reasoning", "methodology"}, nil),
			"add", "")
		require.NoError(t, err)
	}

	got, err := svc.Search().RelevantMethodologyForFact(ctx, "agent/a",
		"kb/synth/src.md", nil, nil, 3, 0.0)
	require.NoError(t, err)
	require.Len(t, got, 3)
}

// TestRelevantMethodologyForFact_MinScoreFiltering_DBSidePrune
// regresses the DB-side pruning path: when minScore is high enough
// that no candidate could possibly clear the threshold even with full
// tag overlap, the SQL WHERE clause filters them out before they reach
// the Go side.
//
// With minScore = 0.9 and weights 0.6/0.4, the per-vec lower bound is
// (0.9 - 0.4) / 0.6 ≈ 0.833. The stub embedder never produces vectors
// that close to the source body, so no candidate clears the bound. The
// composite filter in Go would also drop them, but this test pins the
// invariant that the function returns nothing rather than something the
// caller is expected to filter.
func TestRelevantMethodologyForFact_MinScoreFiltering_DBSidePrune(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	svc.SetEmbedder(&stub768Embedder{})
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/a"))

	ctx := context.Background()
	writeSrcFact(t, svc, "agent/a", "kb/synth/src.md", "source body",
		[]string{"security"}, []string{"Anthropic"})

	for i := 0; i < 3; i++ {
		path := fmt.Sprintf("kb/meta/reasoning/m%d.md", i)
		_, err = svc.Facts().WriteFact(ctx, "agent/a", path,
			methFactBody(fmt.Sprintf("M%d", i), "body",
				[]string{"meta", "reasoning", "methodology"}, nil),
			"add", "")
		require.NoError(t, err)
	}

	got, err := svc.Search().RelevantMethodologyForFact(ctx, "agent/a",
		"kb/synth/src.md", []string{"security"}, []string{"Anthropic"},
		10, 0.9)
	require.NoError(t, err)
	require.Empty(t, got, "minScore=0.9 must filter every candidate")
}

// TestRelevantMethodologyForFact_MinScoreFiltering_GoSidePrune
// covers the Go-side `composite < minScore` filter — exercised when
// the DB-side bound is loose enough to admit candidates whose actual
// composite score still falls below minScore (e.g. tag overlap = 0,
// vector below threshold).
//
// With minScore = 0.30 and a tag-only candidate (no vector match), the
// candidate's composite is 0.4 * tagOverlap. tagOverlap=0.5 yields
// composite=0.2 → dropped; tagOverlap=1.0 yields composite=0.4 → kept.
func TestRelevantMethodologyForFact_MinScoreFiltering_GoSidePrune(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	// No embedder → tag-only ranking.
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/a"))

	ctx := context.Background()
	writeSrcFact(t, svc, "agent/a", "kb/synth/src.md", "src",
		[]string{"security"}, []string{"Anthropic"})

	_, err = svc.Facts().WriteFact(ctx, "agent/a", "kb/meta/reasoning/full.md",
		methFactBody("Full", "body",
			[]string{"meta", "reasoning", "methodology", "security"},
			[]string{"Anthropic"}),
		"full", "")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, "agent/a", "kb/meta/reasoning/half.md",
		methFactBody("Half", "body",
			[]string{"meta", "reasoning", "methodology", "security"}, nil),
		"half", "")
	require.NoError(t, err)

	got, err := svc.Search().RelevantMethodologyForFact(ctx, "agent/a",
		"kb/synth/src.md", []string{"security"}, []string{"Anthropic"},
		10, 0.30)
	require.NoError(t, err)
	require.Len(t, got, 1, "only Full (Score=0.4) clears 0.30; Half (Score=0.2) is dropped")
	require.Equal(t, "kb/meta/reasoning/full.md", got[0].Path)
}

// TestRelevantMethodologyForFact_VectorCoverage_WithSiblingBranchNoise
// regresses the bug where the KNN window was sized to the current
// branch's fact count via `COUNT(*) FROM branch_facts WHERE branch_id =
// ?` — but facts_vec is keyed by the global facts.id (one row per fact
// across all branches). When a sibling branch holds many vector-closer
// facts, those rows fill the global top-k window first; the post-filter
// join then drops them, leaving zero rows for the current branch and
// silently downgrading the methodology section to empty.
func TestRelevantMethodologyForFact_VectorCoverage_WithSiblingBranchNoise(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	svc.SetEmbedder(&stub768Embedder{})
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/sibling"))

	ctx := context.Background()

	// Create the test branch from the sibling at init time — BEFORE any
	// noise is written — so the test branch does not inherit those rows
	// in its branch_facts view.
	require.NoError(t, svc.Branches().CreateBranch(ctx, "agent/test", "agent/sibling"))

	// Source fact on the test branch.
	writeSrcFact(t, svc, "agent/test", "kb/synth/src.md",
		"some source body for vector ranking", nil, nil)

	// Seed 30 noise observations on the sibling branch only. These rows
	// exist in facts/facts_vec (global) but not in the test branch's
	// branch_facts view. They will fill the KNN window first because the
	// stub embedder's similarity is dominated by body length (~32 chars
	// here) — close to the source's length but not to the methodology
	// bodies (18 chars).
	for i := 0; i < 30; i++ {
		path := fmt.Sprintf("kb/obs/n%d.md", i)
		body := fmt.Sprintf("noise body %d with varied content", i)
		_, err = svc.Facts().WriteFact(ctx, "agent/sibling", path, testFactBody(body, 0.5, nil), "noise", "")
		require.NoError(t, err)
	}

	// Seed 3 methodology facts on the test branch.
	for i := 0; i < 3; i++ {
		path := fmt.Sprintf("kb/meta/reasoning/m%d.md", i)
		_, err = svc.Facts().WriteFact(ctx, "agent/test", path,
			methFactBody(fmt.Sprintf("M%d", i), fmt.Sprintf("methodology body %d", i),
				[]string{"meta", "reasoning", "methodology"}, nil),
			"meth", "")
		require.NoError(t, err)
	}

	got, err := svc.Search().RelevantMethodologyForFact(ctx, "agent/test",
		"kb/synth/src.md", nil, nil, 10, 0.0)
	require.NoError(t, err)
	require.Len(t, got, 3, "branch isolation: only the 3 methodology rows on agent/test must surface")

	for _, m := range got {
		require.Greater(t, m.VectorScore, 0.0,
			"methodology candidate %s has VectorScore=0 — sibling-branch noise consumed the KNN window", m.Path)
	}
}

// TestRelevantMethodologyForFact_VectorCoverage_WithNoiseInIndex
// regresses the bug where sqlite-vec's `MATCH ... AND k = N` applied N
// as a global top-k before the type='methodology' filter, causing
// methodology rows to fall outside the window when the index has many
// non-methodology facts.
func TestRelevantMethodologyForFact_VectorCoverage_WithNoiseInIndex(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	svc.SetEmbedder(&stub768Embedder{})
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/a"))

	ctx := context.Background()
	branch := "agent/a"

	writeSrcFact(t, svc, branch, "kb/synth/src.md",
		"some source body for vector ranking", nil, nil)

	// Seed 30 non-methodology observation facts on the same branch.
	for i := 0; i < 30; i++ {
		path := fmt.Sprintf("kb/obs/n%d.md", i)
		body := fmt.Sprintf("noise body %d with varied content", i)
		_, err = svc.Facts().WriteFact(ctx, branch, path, testFactBody(body, 0.5, nil), "noise", "")
		require.NoError(t, err)
	}

	for i := 0; i < 3; i++ {
		path := fmt.Sprintf("kb/meta/reasoning/m%d.md", i)
		_, err = svc.Facts().WriteFact(ctx, branch, path,
			methFactBody(fmt.Sprintf("M%d", i), fmt.Sprintf("methodology body %d", i),
				[]string{"meta", "reasoning", "methodology"}, nil),
			"meth", "")
		require.NoError(t, err)
	}

	got, err := svc.Search().RelevantMethodologyForFact(ctx, branch,
		"kb/synth/src.md", nil, nil, 10, 0.0)
	require.NoError(t, err)
	require.Len(t, got, 3)

	for _, m := range got {
		require.Greater(t, m.VectorScore, 0.0,
			"methodology candidate %s has VectorScore=0 — KNN window did not cover it", m.Path)
	}
}
