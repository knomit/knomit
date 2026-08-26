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
// THE RULE (Q8): no prompt on a DIRECT fact-writing path may carry vocabulary
// drawn from the WIDER CORPUS.
//
// MN1 IS NOW EXCEPTION-FREE. It had exactly one: motif backfill, which wrote
// motifs straight onto existing facts and was excepted on the grounds that the
// fact's claim was already fixed, so corpus vocabulary could only bias which
// existing name got REUSED. That pass was removed as a one-off migration
// wrongly built as permanent machinery, and the exception left with it — every
// surviving step type writes facts only through review-apply.
//
// So the entries below are NOT exceptions to the rule. Each is a path the rule
// does not reach, and each states why its exposure is a different class from
// the one MN1 forbids. An entry that cannot state such a reason IS an
// exception, and there is no longer a precedent for admitting one:
// TestMN1_VocabularyAllowListIsExceptionFree fails if this map grows.
var vocabularyBearingItems = map[string]string{
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

	"discover": "§4/§5 bridging, and the exposure is DISTILL's class rather than " +
		"backfill's (Phase-3 review, M2). The prompt names the group's OWN shared " +
		"motif — the token the members are grouped BY, already on every member fact " +
		"in the payload beside it — never candidates drawn from the wider corpus. It " +
		"cannot bias minting toward a name the model would not otherwise have seen. " +
		"Q8's concern is vocabulary shown at AUTHORING time distorting what gets " +
		"written; here the vocabulary shown IS the input. Covered deterministically " +
		"by TestMN1_DiscoverRendererCarriesOnlyTheGroupsOwnMotif, because a session " +
		"produces a discover item only when a bridge happens to form.",

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
						"direct fact-writing path may carry the wider corpus's vocabulary, "+
						"and MN1 no longer has an exception to point at. If this item's "+
						"exposure is genuinely a different class, declare it in "+
						"vocabularyBearingItems with that reason AND update "+
						"TestMN1_VocabularyAllowListIsExceptionFree, which exists so the "+
						"list cannot grow quietly.", item.StepType, what)
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
	// STILL NOT COVERED HERE: the DISCOVER renderer. Discovery does not run at
	// EffortMedium, so no discover item exists in THIS session to inspect.
	//
	// Phase 2 named this gap in advance — "whoever gives discover a
	// vocabulary-bearing payload finds it named" — and Phase 3 is whoever: the
	// far-lane prompt now prints the group's shared motif. The gap is closed by
	// TestMN1_DiscoverRendererCarriesOnlyTheGroupsOwnMotif below, which renders
	// the item directly rather than waiting for a bridge to form, and by the
	// declared register entry above. Naming it worked; it is left named.
	//
	// markedReached is recorded rather than asserted for the reason above: it
	// is a property of what this corpus clustered into, not of the rule.
	t.Logf("ordinary item saw the marked facts: %v", markedReached)

	// Bidirectional. A declared exception that never carries vocabulary is a
	// permission nobody needs, and one nobody notices going stale — the
	// os.Stat-and-continue shape counts as no test.
	for step := range vocabularyBearingItems {
		if step == "distill" || step == "reflect" || step == "discover" {
			continue // covered by the deterministic renderer tests below; not
			// every session produces one
		}
		require.Containsf(t, seen, step,
			"%s is a declared vocabulary-bearing item but this session produced none — "+
				"either the fixture no longer exercises it or the entry is stale", step)
	}
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

// TestMN1_DiscoverRendererCarriesOnlyTheGroupsOwnMotif closes the gap Phase 2
// named and Phase 3 walked into (review M2).
//
// Rendered directly rather than through a session: a session produces a discover
// item only when a bridge happens to form, and a guard that depends on a gate
// upstream of it can pass for want of the opportunity to fail — which is
// exactly how a planted leak once survived the scan above.
func TestMN1_DiscoverRendererCarriesOnlyTheGroupsOwnMotif(t *testing.T) {
	const marker = "zzzmarker-unique-shape"
	const foreign = "zzzforeign-other-shape"

	members := []factForLLM{
		{File: "kb/a.md", Title: "Alpha", Body: "a body", Motifs: []string{marker}},
		{File: "kb/b.md", Title: "Bravo", Body: "another body", Motifs: []string{marker}},
	}
	payload := DiscoverWorkPayload{
		Direction: DiscoverBackward, Lane: LaneFar,
		Bridge: BridgeSeedSet{Token: marker, Kind: BridgeMotif, Members: members},
	}
	view := RenderDiscoverWorkItem(payload, "kb")

	// The group's own token appears — that is the authorized exposure, and the
	// far-lane SHIP line cannot say what the members claim without naming it.
	require.Contains(t, view.Prompt, marker)

	// The distinction that makes it distill's class rather than backfill's: a
	// motif no member carries must never appear. If the corpus's vocabulary
	// ever reaches this renderer, this is the assertion that fails.
	require.NotContains(t, view.Prompt, foreign)
	require.NotContains(t, view.ResponseSchema, foreign)
	require.NotContains(t, view.ResponseSchema, marker,
		"the response schema asks for motifs in the ABSTRACT; naming this corpus's "+
			"vocabulary there would be the leak MN1 forbids")

	// And a group whose members carry no motifs at all renders no motif
	// vocabulary anywhere — the entity/domain bridges that share this renderer.
	bare := RenderDiscoverWorkItem(DiscoverWorkPayload{
		Direction: DiscoverForward,
		Bridge: BridgeSeedSet{Token: "some-entity", Kind: BridgeEntity, Members: []factForLLM{
			{File: "kb/c.md", Title: "Charlie", Body: "body"}}},
	}, "kb")
	require.NotContains(t, bare.Prompt, marker)
	require.NotContains(t, bare.Prompt, foreign)
}

// TestMN1_VocabularyAllowListIsExceptionFree pins the SHAPE of the allow-list,
// which the scan above cannot.
//
// The scan is behavioural: it proves no item LEAKED the marker this session.
// That is necessary and not sufficient — an exception added to the map is
// invisible to it, because the map is what the scan consults to decide whether
// a leak was permitted. Widening the allow-list makes the scan agree with the
// widening. So the list needs a test of its own, and this is it.
//
// Backfill's removal is what makes an exact assertion possible: with the sole
// direct-fact-writing exception gone, the permitted set is closed, and any
// growth is a deliberate act that has to be argued for here rather than merely
// added above.
func TestMN1_VocabularyAllowListIsExceptionFree(t *testing.T) {
	permitted := make([]string, 0, len(vocabularyBearingItems))
	for k := range vocabularyBearingItems {
		permitted = append(permitted, k)
	}
	require.ElementsMatch(t, []string{
		motifAliasStepType, motifDefineStepType, "distill", "discover", "reflect",
	}, permitted,
		"the MN1 vocabulary allow-list changed. Every entry must be a path the "+
			"Q8 rule does not reach — one that writes facts through review-apply, "+
			"not directly — with its reason stated in the map. Backfill was the only "+
			"direct-write exception and it no longer exists; do not re-open the "+
			"category without a designer ruling.")
}
