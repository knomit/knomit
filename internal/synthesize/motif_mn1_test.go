package synthesize

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// MN1, checked against what a MODEL ACTUALLY RECEIVES.
//
// Two earlier versions of this test were wrong in the same way. The first
// scanned the served instructions and two tool schemas while its comment spoke
// of "any prompt". The second widened to prompt TEMPLATES and still claimed the
// same coverage — but Phase-2 vocabulary rides in PAYLOADS, which no template
// scan can see, so the widening reproduced the original defect one level out:
// a test naming an exception it never enumerated against.
//
// This version runs a real session, renders every work item it produced, and
// inspects the prompt, the payload AND the response schema — the three things
// that reach a model. A marker motif planted in the corpus must appear only
// where the list below says it may.

// vocabularyBearingItems are the rendered work items permitted to carry this
// corpus's motif vocabulary, and where in each.
//
// THE RULE (Q8): no prompt on a FACT-WRITING path may carry the corpus's
// vocabulary; backfill is the single exception. The entries below are that
// rule plus the two the roadmap authorizes elsewhere, each with the reason its
// exposure is not the one MN1 forbids.
var vocabularyBearingItems = map[string]string{
	motifBackfillStepType: "THE exception (Q8). Backfill is genuinely a fact-writing path — it puts " +
		"motifs onto facts — and genuinely excepted: the fact already exists and its " +
		"claim is fixed, so vocabulary can only bias which existing name is REUSED, " +
		"which is the entire purpose. Reuse-before-minting is correct exactly here.",

	motifAliasStepType: "not a fact-writing path at all. §3.1 clusters the vocabulary, so the " +
		"vocabulary IS the subject; the pass writes derived state and no facts.",

	motifDefineStepType: "same — §3.2 defines names, and cannot do so without them. Note it " +
		"receives names ONLY, never carrier facts, which is a separate and stricter " +
		"blindness the payload test asserts.",

	"distill": "§6 distill enrichment, and the exposure is materially different from " +
		"backfill's. Distill is shown the motifs its OWN INPUT FACTS already carry, " +
		"not candidates drawn from the wider corpus. It cannot bias minting toward " +
		"names the model would not otherwise have seen, because those motifs are " +
		"already in front of it on the facts themselves.",

	"reflect": "§3.3 health metrics. Reflect writes methodology, not facts about the " +
		"corpus's subjects, and it receives aggregate COUNTS plus the vocabulary as " +
		"context for reasoning about the corpus's own habits.",
}

func TestMN1_RenderedWorkItemsCarryNoUnauthorizedVocabulary(t *testing.T) {
	const marker = "zzzmarker-unique-shape"
	ctx := context.Background()
	env := newRestatementEnv(t, 0)

	// A corpus that gives every pass something to do, with one motif whose
	// name cannot occur by accident.
	env.writeFactWithMotifs("kb/marked.md", "Marked fact", "a distinct body", []string{marker})
	env.writeFactWithMotifs("kb/marked2.md", "Second marked", "another distinct body", []string{marker})
	for i := range 16 {
		env.writeFactWithMotifs(
			fmt.Sprintf("kb/f%02d.md", i), fmt.Sprintf("Fact %d", i),
			fmt.Sprintf("A distinct body about subject number %d.", i),
			[]string{fmt.Sprintf("mechanism-%s", numberWord(i))})
	}
	env.writeFact("kb/bare.md", "Bare fact", "a body with no motif at all")

	out := env.vocabSession()
	require.NotEmpty(t, out.restatementItems, "the session must produce items to inspect")

	sess := store.PipelineSession{ID: out.sessionID, Branch: env.branch}
	seen := map[string]bool{}
	// markedReached records whether a NON-excepted item was handed the marked
	// facts at all. Without it the scan can pass for want of opportunity: if no
	// ordinary pass ever sees the marked fact, a leak in that pass is invisible
	// and the test reports success. Measured — a planted leak in prune's
	// response schema passed this test until this precondition was added.
	markedReached := false
	for _, item := range out.restatementItems {
		view, err := (reviewStrategy{}).Render(ctx, env.deps(), &sess, &item)
		require.NoErrorf(t, err, "render %s", item.StepType)
		seen[item.StepType] = true

		// Everything that reaches the model, not just the prompt.
		surfaces := map[string]string{
			"prompt":          view.Prompt,
			"payload":         view.Facts,
			"response schema": view.ResponseSchema,
		}
		_, allowed := vocabularyBearingItems[item.StepType]
		if !allowed && strings.Contains(view.Facts, "kb/marked") {
			markedReached = true
		}
		for what, text := range surfaces {
			if strings.Contains(text, marker) {
				require.Truef(t, allowed,
					"work item %q leaks corpus vocabulary in its %s. No prompt on a "+
						"fact-writing path may carry the corpus's vocabulary; if this item "+
						"legitimately must, declare it in vocabularyBearingItems with the "+
						"reason its exposure is not the one MN1 forbids.", item.StepType, what)
			}
		}
	}
	require.Greater(t, len(seen), 1, "the scan must cover more than one step type")
	// WHAT THIS SCAN DOES AND DOES NOT COVER — stated exactly, because the
	// honest boundary is the whole value of a conformance test.
	//
	// COVERED, deterministically: every step type this session actually
	// produced, inspected on all three surfaces a model receives. At
	// EffortMedium that is prune, distill, reflect and the three motif steps.
	//
	// COVERED elsewhere, deterministically: prune and distill as renderers,
	// by TestMN1_OrdinaryItemsNeverCarryVocabulary below — which exists
	// because THIS scan's coverage of ordinary passes depends on the fixture
	// producing an item that receives the marked facts, and a leak planted in
	// prune's response schema once passed here for want of the opportunity to
	// fail.
	//
	// NOT COVERED: the DISCOVER renderer. Discovery does not run at
	// EffortMedium, so no discover item exists to inspect here, and no
	// deterministic render test covers it either. A vocabulary leak in
	// discover's prompt or response schema would be invisible to this suite.
	// Phase 2 adds `motifs` to discover's OUTPUT schema (MN11) but no
	// vocabulary to its input, so there is nothing to leak today — this is a
	// gap in the guard, not a known defect, and it is named so that whoever
	// gives discover a vocabulary-bearing payload finds it named.
	//
	// markedReached is recorded rather than asserted for the reason above: it
	// is a property of what this corpus clustered into, not of the rule.
	t.Logf("ordinary item saw the marked facts: %v", markedReached)

	// Bidirectional. A declared exception that never carries vocabulary is a
	// permission nobody needs, and one nobody notices going stale — the
	// os.Stat-and-continue shape counts as no test.
	for step := range vocabularyBearingItems {
		if step == "distill" || step == "reflect" {
			continue // covered by the template assertions below; not every
			// session produces one
		}
		require.Containsf(t, seen, step,
			"%s is a declared vocabulary-bearing item but this session produced none — "+
				"either the fixture no longer exercises it or the entry is stale", step)
	}
}

// The backfill payload must ACTUALLY carry vocabulary. The exception exists to
// permit something; if it permits nothing, the pass is asking a model to prefer
// existing names while showing it none — which is what shipped before C2.
func TestMN1_BackfillActuallyReceivesTheVocabularyItIsAllowed(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	env.writeFactWithMotifs("kb/a.md", "Alpha", "a distinct body", []string{"silent-fallback"})
	env.writeFactWithMotifs("kb/b.md", "Bravo", "another distinct body", []string{"silent-fallback"})
	for i := range 16 {
		env.writeFactWithMotifs(
			fmt.Sprintf("kb/f%02d.md", i), fmt.Sprintf("Fact %d", i),
			fmt.Sprintf("A distinct body about subject number %d.", i),
			[]string{fmt.Sprintf("mechanism-%s", numberWord(i))})
	}
	env.writeFact("kb/bare.md", "Bare fact", "a body with no motif")

	out := env.vocabSession()
	sess := store.PipelineSession{ID: out.sessionID, Branch: env.branch}

	var found bool
	for _, item := range out.restatementItems {
		if item.StepType != motifBackfillStepType {
			continue
		}
		found = true
		view, err := (reviewStrategy{}).Render(ctx, env.deps(), &sess, &item)
		require.NoError(t, err)
		require.Contains(t, view.Facts, "vocabulary",
			"the backfill payload must carry the vocabulary field its prompt refers to")
		require.Contains(t, view.Facts, "silent-fallback",
			"...populated with the corpus's actual recurring motifs. An empty exception "+
				"is worse than no exception: the prompt tells the model to prefer an "+
				"existing name and then shows it none.")
	}
	require.True(t, found, "the fixture must produce a backfill item")
}

// The two authorized TEMPLATE carriers, asserted where they live. Both are
// conditional sections, so a corpus without the data sees neither.
func TestMN1_AuthorizedTemplateCarriersAreConditional(t *testing.T) {
	// distill: shared motifs of its own input facts.
	withShared, err := RenderDistillWorkItem([]factForLLM{
		{File: "a", Motifs: []string{"silent-fallback"}},
		{File: "b", Motifs: []string{"silent-fallback"}},
	}, "kb", "")
	require.NoError(t, err)
	require.Contains(t, withShared.Prompt, "silent-fallback")

	without, err := RenderDistillWorkItem([]factForLLM{{File: "a"}}, "kb", "")
	require.NoError(t, err)
	require.NotContains(t, without.Prompt, "Motifs already shared",
		"a cluster sharing no motifs must see no motif section")

	// reflect: the §3.3 metrics.
	withVocab, err := RenderReflectWorkItem([]byte(`[{"path":"kb/h.md"}]`), "kb", "",
		"12 motif clusters across authored facts. Recurrence 50%.")
	require.NoError(t, err)
	require.Contains(t, withVocab.Prompt, "Recurrence")

	withoutVocab, err := RenderReflectWorkItem([]byte(`[{"path":"kb/h.md"}]`), "kb", "", "")
	require.NoError(t, err)
	require.NotContains(t, withoutVocab.Prompt, "motif clusters",
		"a motif-free corpus must see no motif section")
}

// The non-excepted path, checked DETERMINISTICALLY rather than depending on
// what a corpus happens to cluster into.
//
// The session scan above is worth having — it inspects real items — but its
// coverage of ordinary passes depends on the fixture producing one that
// receives the marked facts, and it did not. A leak planted in prune's
// response schema passed that scan for want of the opportunity to fail. This
// renders the ordinary items directly, with facts that definitely carry motifs.
func TestMN1_OrdinaryItemsNeverCarryVocabulary(t *testing.T) {
	const marker = "zzzmarker-unique-shape"
	marked := []factForLLM{
		{File: "kb/a.md", Title: "Alpha", Body: "a body", Motifs: []string{marker}},
		{File: "kb/b.md", Title: "Bravo", Body: "another body", Motifs: []string{marker}},
	}

	// prune: a fact-writing path with no vocabulary exception.
	prune, err := RenderPruneWorkItem(marked, "kb")
	require.NoError(t, err)
	require.NotContains(t, prune.Prompt, marker,
		"the prune PROMPT must not enumerate corpus vocabulary")
	require.NotContains(t, prune.ResponseSchema, marker,
		"nor its response schema")
	require.Contains(t, prune.Facts, marker,
		"the PAYLOAD carries each fact's own motifs, which is §2.1's carry-over "+
			"requirement and not a vocabulary leak: these are the motifs of the facts "+
			"being judged, already in front of the model")

	// distill carries shared motifs by authorization, but only from its own
	// input facts — never a wider vocabulary.
	distill, err := RenderDistillWorkItem(marked, "kb", "")
	require.NoError(t, err)
	require.Contains(t, distill.Prompt, marker,
		"distill's authorized shared-motif line")
	require.Contains(t, distill.Facts, marker)

	// The distinction that makes distill's exposure safe: it is shown only what
	// its OWN facts carry. A motif no input fact has must never appear.
	unrelated := []factForLLM{{File: "kb/c.md", Title: "Charlie", Body: "body"}}
	clean, err := RenderDistillWorkItem(unrelated, "kb", "")
	require.NoError(t, err)
	require.NotContains(t, clean.Prompt, marker,
		"distill must never see a motif none of its input facts carries — that would "+
			"be the corpus vocabulary, which is backfill's exception and not distill's")
}
