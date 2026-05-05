package store

import (
	"context"
	"path/filepath"
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
// formatter renders header + per-match title/path/body with bullet markers.
// Pure function (no DB) — used by mcp and synthesize callers in Tasks 4-6.
func TestFormatMethodologySection_NonEmpty(t *testing.T) {
	matches := []MethodologyMatch{
		{Path: "kb/meta/reasoning/m1.md", Title: "M1 title", Body: "M1 body about evidence weighting."},
		{Path: "kb/meta/reasoning/m2.md", Title: "M2 title", Body: "M2 body about pitfall detection."},
	}
	got := FormatMethodologySection(matches)
	require.Contains(t, got, "Applicable methodology")
	require.Contains(t, got, "M1 title")
	require.Contains(t, got, "kb/meta/reasoning/m1.md")
	require.Contains(t, got, "M1 body about evidence weighting.")
	require.Contains(t, got, "M2 title")
	require.Contains(t, got, "M2 body about pitfall detection.")
	require.Contains(t, got, "• M1 title")
}

// TestFormatMethodologySection_Empty returns empty string for empty input
// so callers can omit the entire section.
func TestFormatMethodologySection_Empty(t *testing.T) {
	require.Equal(t, "", FormatMethodologySection(nil))
	require.Equal(t, "", FormatMethodologySection([]MethodologyMatch{}))
}
