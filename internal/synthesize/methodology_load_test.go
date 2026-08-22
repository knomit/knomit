package synthesize

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// newReviewerForMethodologyTest builds a Reviewer with a real on-disk
// store rooted at t.TempDir(). The agent branch is "agent/test" and the
// methodology min-score floor defaults to 0 (admit all candidates) so
// callers can write tests without engineering specific composite scores.
func newReviewerForMethodologyTest(t *testing.T, minScore float64) (*Reviewer, *store.Service) {
	t.Helper()
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:                "test",
		AgentBranch:         "agent/test",
		Svc:                 svc,
		OntologyRoot:        "kb",
		MethodologyMinScore: minScore,
	})
	return NewReviewer(ri, nil), svc
}

func writeFactForTest(t *testing.T, svc *store.Service, branch, path, title, body string, ftype fact.Type, doms, ents []string) {
	t.Helper()
	f := fact.NewFact(path)
	f.Title = title
	f.Body = body
	f.Type = ftype
	f.Confidence = 0.7
	f.Sources = 1
	f.Domain = doms
	f.Entities = ents
	serialized, err := fact.SerializeFact(f)
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(context.Background(), branch, path, serialized, "add", "")
	require.NoError(t, err)
}

// TestLoadDistillMethodology_InjectsWhenRelevant covers the distill
// injection site: when methodology shares the cluster's tags, the
// rendered section carries the title/path/score bullets and omits the
// body (model fetches via knomit_query).
func TestLoadDistillMethodology_InjectsWhenRelevant(t *testing.T) {
	r, svc := newReviewerForMethodologyTest(t, 0.0)
	writeFactForTest(t, svc, "agent/test", "kb/meta/reasoning/distill-rule.md",
		"Distill rule of thumb",
		"Prefer distillation over pruning when clusters span multiple entities.",
		fact.Methodology,
		[]string{"meta", "reasoning", "methodology", "security"},
		[]string{"Anthropic"})

	facts := []factForLLM{
		{File: "kb/synth/a.md", Title: "A", Body: "first body", Type: "synthesis", Domain: []string{"security"}, Entities: []string{"Anthropic"}},
		{File: "kb/synth/b.md", Title: "B", Body: "second body", Type: "synthesis", Domain: []string{"security"}, Entities: []string{"Anthropic"}},
	}
	section := r.loadDistillMethodology(context.Background(), "agent/test", facts)

	require.NotEmpty(t, section, "methodology with overlapping tags must surface")
	require.Contains(t, section, "Distill rule of thumb")
	require.Contains(t, section, "kb/meta/reasoning/distill-rule.md")
	require.Contains(t, section, "score=")
	require.NotContains(t, section, "Prefer distillation over pruning",
		"body must NOT be inlined; LLM fetches via knomit_query on demand")
}

// TestLoadDistillMethodology_EmptyWhenNoMethodologyExists asserts the
// section is "" (not an empty heading) when no methodology is on the
// branch — so the surrounding template's {{if .ApplicableMethodology}}
// block omits the entire wrapper text.
func TestLoadDistillMethodology_EmptyWhenNoMethodologyExists(t *testing.T) {
	r, _ := newReviewerForMethodologyTest(t, 0.0)
	facts := []factForLLM{
		{File: "kb/synth/a.md", Title: "A", Body: "x", Type: "synthesis", Domain: []string{"security"}},
	}
	require.Equal(t, "", r.loadDistillMethodology(context.Background(), "agent/test", facts))
}

// TestLoadDistillMethodology_RespectsCanceledContext asserts that a
// cancellation between hot-loop iterations returns "" rather than
// partial results — the methodology section must not silently degrade
// on cancellation.
func TestLoadDistillMethodology_RespectsCanceledContext(t *testing.T) {
	r, svc := newReviewerForMethodologyTest(t, 0.0)
	writeFactForTest(t, svc, "agent/test", "kb/meta/reasoning/m.md",
		"M", "body", fact.Methodology,
		[]string{"meta", "reasoning", "methodology", "security"},
		[]string{"Anthropic"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	section := r.loadDistillMethodology(ctx, "agent/test", []factForLLM{
		{File: "kb/synth/a.md", Title: "A", Body: "x", Type: "synthesis", Domain: []string{"security"}, Entities: []string{"Anthropic"}},
	})
	require.Equal(t, "", section, "canceled context must yield empty section, not partial results")
}

// TestLoadReflectMethodology_InjectsForTransitionTags covers the reflect
// injection site: methodology relevant to the union of transition-fact
// tags surfaces as bullets in the section.
func TestLoadReflectMethodology_InjectsForTransitionTags(t *testing.T) {
	r, svc := newReviewerForMethodologyTest(t, 0.0)

	writeFactForTest(t, svc, "agent/test", "kb/meta/reasoning/reflect-lesson.md",
		"Reflect lesson",
		"Hypothesis confidence updates should trigger a methodology review.",
		fact.Methodology,
		[]string{"meta", "reasoning", "methodology", "security"},
		[]string{"Anthropic"})
	writeFactForTest(t, svc, "agent/test", "kb/hyp/breach.md",
		"Breach hypothesis", "h body", fact.Hypothesis,
		[]string{"security"}, []string{"Anthropic"})

	transitions := []hypothesisTransition{
		{Path: "kb/hyp/breach.md", OriginalType: "hypothesis", Action: "promoted"},
	}
	transitionsJSON, err := json.Marshal(transitions)
	require.NoError(t, err)

	section := r.loadReflectMethodology(context.Background(), "agent/test", transitionsJSON)
	require.NotEmpty(t, section)
	require.Contains(t, section, "Reflect lesson")
	require.Contains(t, section, "kb/meta/reasoning/reflect-lesson.md")
	require.Contains(t, section, "score=")
	require.NotContains(t, section, "Hypothesis confidence updates should trigger",
		"body must NOT be inlined")
}

// TestLoadReflectMethodology_EmptyOnMalformedJSON asserts the section
// is "" (and the operator gets a logged warning, verified by the
// warn-log path in the implementation) when the transitions JSON is
// unparseable — rather than silently swallowing the corruption.
func TestLoadReflectMethodology_EmptyOnMalformedJSON(t *testing.T) {
	r, _ := newReviewerForMethodologyTest(t, 0.0)
	require.Equal(t, "", r.loadReflectMethodology(context.Background(), "agent/test", []byte("not json")))
}

// TestLoadReflectMethodology_EmptyOnEmptyTransitions guards the early
// return: zero transitions short-circuits before any DB work.
func TestLoadReflectMethodology_EmptyOnEmptyTransitions(t *testing.T) {
	r, _ := newReviewerForMethodologyTest(t, 0.0)
	require.Equal(t, "", r.loadReflectMethodology(context.Background(), "agent/test", []byte("[]")))
}

// TestRenderDistillWorkItem_HeaderAppearsWhenSectionPresent verifies the
// distill template wraps the formatter bullets in its "Applicable
// methodology candidates" framing with the mandatory-fetch directive,
// and omits the entire wrapper when the section is empty (no orphan
// heading).
func TestRenderDistillWorkItem_HeaderAppearsWhenSectionPresent(t *testing.T) {
	facts := []factForLLM{{File: "kb/x.md", Title: "X", Body: "y", Type: "synthesis"}}
	bullets := "• score=0.50  Lesson  (kb/meta/reasoning/lesson.md)\n"

	content, err := RenderDistillWorkItem(facts, "kb", bullets)
	require.NoError(t, err)
	require.Contains(t, content.Prompt, "Applicable methodology candidates")
	require.Contains(t, content.Prompt, "Lesson")
	require.Contains(t, content.Prompt, "kb/meta/reasoning/lesson.md")
	require.Contains(t, content.Prompt, "knomit_query",
		"prompt must reference knomit_query as the fetch tool")
	require.Contains(t, content.Prompt, "score ≥ 0.50",
		"prompt must state the mandatory-fetch threshold")
	require.Contains(t, content.Prompt, "EVERY candidate",
		"prompt must use forcing language, not 'if useful'")

	contentEmpty, err := RenderDistillWorkItem(facts, "kb", "")
	require.NoError(t, err)
	require.NotContains(t, contentEmpty.Prompt, "Applicable methodology candidates",
		"empty section must not render an orphan heading")
}

// TestLoadDistillMethodology_ThresholdComparesAgainstScore_NotTagOrVector
// pins down that MethodologyMinScore filters candidates by composite
// Score — not by TagOverlap or VectorScore. With no embedder configured,
// composite Score = 0.4·TagOverlap. With source tags [security/Anthropic]:
//   - "full": full tag overlap (TagOverlap=1.0, Score=0.4) — KEPT (>0.30)
//   - "half": domain-only overlap (TagOverlap=0.5, Score=0.2) — DROPPED (<0.30)
//
// If the implementation accidentally compared minScore to TagOverlap, both
// would be kept (1.0 > 0.30, 0.5 > 0.30). If it compared to VectorScore,
// both would be dropped (vector=0 here). Asserting "full kept, half
// dropped" discriminates between all three.
func TestLoadDistillMethodology_ThresholdComparesAgainstScore_NotTagOrVector(t *testing.T) {
	r, svc := newReviewerForMethodologyTest(t, 0.30)

	writeFactForTest(t, svc, "agent/test", "kb/meta/reasoning/full.md",
		"Full match", "body",
		fact.Methodology,
		[]string{"meta", "reasoning", "methodology", "security"},
		[]string{"Anthropic"})
	writeFactForTest(t, svc, "agent/test", "kb/meta/reasoning/half.md",
		"Half match", "body",
		fact.Methodology,
		[]string{"meta", "reasoning", "methodology", "security"},
		nil)

	facts := []factForLLM{
		{File: "kb/synth/a.md", Title: "A", Body: "x", Type: "synthesis", Domain: []string{"security"}, Entities: []string{"Anthropic"}},
		{File: "kb/synth/b.md", Title: "B", Body: "y", Type: "synthesis", Domain: []string{"security"}, Entities: []string{"Anthropic"}},
	}
	section := r.loadDistillMethodology(context.Background(), "agent/test", facts)

	require.Contains(t, section, "Full match",
		"Score=0.4 (above 0.30 threshold) must be kept; comparison against TagOverlap would also keep it but other half check disambiguates")
	require.NotContains(t, section, "Half match",
		"Score=0.2 (below 0.30) must be dropped; if the comparison used TagOverlap (0.5>0.30) this would incorrectly stay")
}

// TestRenderReflectWorkItem_HeaderAppearsWhenSectionPresent verifies the
// reflect template wraps the formatter bullets in its "Existing
// methodology candidates" framing (intentionally distinct from distill's
// "Applicable" wording) with mandatory-fetch directive, and omits the
// wrapper when the section is empty.
func TestRenderReflectWorkItem_HeaderAppearsWhenSectionPresent(t *testing.T) {
	bullets := "• score=0.40  Existing  (kb/meta/reasoning/existing.md)\n"

	content, err := RenderReflectWorkItem([]byte(`[{"path":"kb/hyp/a.md"}]`), "kb", bullets, "")
	require.NoError(t, err)
	require.Contains(t, content.Prompt, "Existing methodology candidates")
	require.Contains(t, content.Prompt, "Existing")
	require.Contains(t, content.Prompt, "kb/meta/reasoning/existing.md")
	require.Contains(t, content.Prompt, "score ≥ 0.50",
		"prompt must state the mandatory-fetch threshold")
	require.Contains(t, content.Prompt, "EVERY candidate",
		"prompt must use forcing language, not 'if useful'")

	contentEmpty, err := RenderReflectWorkItem([]byte(`[{"path":"kb/hyp/a.md"}]`), "kb", "", "")
	require.NoError(t, err)
	require.NotContains(t, contentEmpty.Prompt, "Existing methodology candidates",
		"empty section must not render an orphan heading")
}
