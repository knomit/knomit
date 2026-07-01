package storytests

import (
	"encoding/json"
	"testing"

	"knomit/internal/fact"
	"knomit/test/testenv"
)

// TestMethodologyWorkflow_HypothesizeInjectsRelevantMethodology drives
// the real HypothesizeHandler and asserts the work item's Instructions
// carry the "Applicable methodology" section when relevant methodology
// exists on the agent branch.
func TestMethodologyWorkflow_HypothesizeInjectsRelevantMethodology(t *testing.T) {
	sb := testenv.NewStoryboard(t)
	agent := sb.Repo("alpha").Branch("agent/test")

	agent.Write("kb/meta/reasoning/tag-overlap-method.md",
		testenv.Fact("Use tag-overlap before vector for sparse domains").
			Type(fact.Methodology).
			Body("Cosine alone gives false positives when methodology bodies are short.").
			Domain("meta", "reasoning", "methodology", "AI economics").
			Entities("Anthropic"),
		"add methodology")

	agent.Write("kb/synth/tva.md",
		testenv.Fact("Token volume amplification synthesis").
			Domain("AI economics").
			Entities("Anthropic").
			Type(fact.Synthesis),
		"add synth")

	view := agent.Hypothesize(nil)
	view.MustNotBeDone()
	view.MustHaveInstructionsContaining("Applicable methodology")
	view.MustHaveInstructionsContaining("Use tag-overlap before vector")
	view.MustNotHaveInstructionsContaining("Cosine alone gives false positives")
	view.MustHaveInstructionsContaining("score=")
	view.MustHaveInstructionsContaining("kb/meta/reasoning/")
	view.MustHaveInstructionsContaining("knomit_query")
}

// TestMethodologyWorkflow_HypothesizeOmitsSectionWhenNoMatch asserts
// the "Applicable methodology" section is absent when no methodology
// fact exists on the branch, while the base instructions still surface.
func TestMethodologyWorkflow_HypothesizeOmitsSectionWhenNoMatch(t *testing.T) {
	sb := testenv.NewStoryboard(t)
	agent := sb.Repo("alpha").Branch("agent/test")

	agent.Write("kb/synth/lone.md",
		testenv.Fact("Lone synthesis").
			Type(fact.Synthesis).
			Domain("security"),
		"add synth")

	view := agent.Hypothesize(nil)
	view.MustNotBeDone()
	view.MustNotHaveInstructionsContaining("Applicable methodology")
	view.MustHaveInstructionsContaining("knomit_explain")
}

// TestMethodologyWorkflow_MethodologyPersistsAcrossCycles drives two
// synthesis cycles. Mid-cycle 1, the "agent" writes a hypothesis citing
// the methodology so /incoming evolves; cycle 2 picks up a new
// synthesis incrementally and still surfaces the methodology.
func TestMethodologyWorkflow_MethodologyPersistsAcrossCycles(t *testing.T) {
	sb := testenv.NewStoryboard(t)
	agent := sb.Repo("alpha").Branch("agent/test")

	cMeth := agent.Write("kb/meta/reasoning/breach-method.md",
		testenv.Fact("Adversarial breach reasoning").
			Type(fact.Methodology).
			Body("Weight base-rate updates higher than narrative coherence.").
			Domain("meta", "reasoning", "methodology", "security").
			Entities("Anthropic"),
		"add methodology")
	agent.Write("kb/synth/breach-1.md",
		testenv.Fact("Breach probability synthesis 1").
			Type(fact.Synthesis).
			Domain("security").
			Entities("Anthropic"),
		"add synth 1")
	agent.Write("kb/synth/breach-2.md",
		testenv.Fact("Breach probability synthesis 2").
			Type(fact.Synthesis).
			Domain("security").
			Entities("Anthropic"),
		"add synth 2")

	cMeth.Fact("kb/meta/reasoning/breach-method.md").Incoming().MustHaveCount(0)

	// Cycle 1: walk both synth items; mid-cycle, write a hypothesis
	// citing the methodology to evolve /incoming.
	view1 := agent.Hypothesize(nil)
	view1.MustNotBeDone().MustHaveInstructionsContaining("Adversarial breach reasoning")

	var firstFact fact.Fact
	if err := json.Unmarshal(view1.Item().Fact, &firstFact); err != nil {
		t.Fatalf("unmarshal first work item fact: %v", err)
	}
	if firstFact.Path() == "" {
		t.Fatalf("first work item fact has empty path")
	}
	cHyp := agent.Write("kb/hyp/breach-cycle1.md",
		testenv.Fact("Breach hypothesis 1").
			Type(fact.Hypothesis).
			Domain("security").
			Entities("Anthropic").
			Refs("kb/meta/reasoning/breach-method.md"),
		"hypothesis citing methodology")

	view2 := view1.Continue("done with first")
	view2.MustNotBeDone().MustHaveInstructionsContaining("Adversarial breach reasoning")

	view2.Continue("done with second").MustBeDone()

	view := cMeth.Fact("kb/meta/reasoning/breach-method.md").Incoming()
	view.MustHaveCount(1)
	view.MustHaveItem("kb/hyp/breach-cycle1.md", cHyp.Commit)

	// Cycle 2: a new synthesis lands; the watermark advanced after
	// cycle 1 so this run only processes the new fact, but methodology
	// must still inject.
	agent.Write("kb/synth/breach-3.md",
		testenv.Fact("Breach probability synthesis 3").
			Type(fact.Synthesis).
			Domain("security").
			Entities("Anthropic"),
		"add synth 3")

	cycle2 := agent.Hypothesize(nil)
	cycle2.MustNotBeDone().MustHaveInstructionsContaining("Adversarial breach reasoning")
	cycle2.Continue("done").MustBeDone()
}

// TestMethodologyWorkflow_LowScoreCandidatesDropped seeds a methodology
// fact whose only overlap with the synthesis is via the universal markers
// (which are excluded from the tag-overlap calculation) and no body
// similarity. With a high threshold the candidate's composite score
// falls below the floor and the section is omitted entirely.
//
// We override the threshold via StoryboardOpts.MethodologyMinScore
// because the deterministic embedder hashes inputs into a 768-d vector
// where cosine similarity between distinct strings is essentially
// random — engineering reliable sub-0.15 vector scores by tweaking
// strings is brittle. Forcing the threshold to 0.99 makes any candidate
// drop and observably exercises the user-facing filter.
func TestMethodologyWorkflow_LowScoreCandidatesDropped(t *testing.T) {
	high := 0.99
	sb := testenv.NewStoryboardWithOpts(t, testenv.StoryboardOpts{
		AutoVerify:          true,
		VerifyDeep:          true,
		MethodologyMinScore: &high,
	})
	agent := sb.Repo("alpha").Branch("agent/test")

	agent.Write("kb/meta/reasoning/markers-only.md",
		testenv.Fact("Markers-only methodology").
			Type(fact.Methodology).
			Body("A reasoning lesson with no domain-specific overlap.").
			Domain("meta", "reasoning", "methodology"),
		"add methodology")

	agent.Write("kb/synth/unrelated.md",
		testenv.Fact("Unrelated synthesis fact").
			Type(fact.Synthesis).
			Domain("security").
			Entities("Anthropic").
			Body("A completely different topic with no body similarity."),
		"add synth")

	view := agent.Hypothesize(nil)
	view.MustNotBeDone()
	view.MustNotHaveInstructionsContaining("Applicable methodology")
	view.MustHaveInstructionsContaining("knomit_explain")
	view.MustHaveInstructionsContaining("knomit_query")
}
