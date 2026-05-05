package storytests

import (
	"context"
	"strings"
	"testing"

	"knomit/internal/fact"
	"knomit/internal/store"
	"knomit/internal/testenv"
)

// TestMethodologyLoop_CiteBackPlumbing verifies the loop closure: a
// hypothesis fact written with refs pointing at methodology results in
// the methodology's /incoming containing that hypothesis.
//
// The "LLM" here is the test author writing the hypothesis fact directly
// with refs set — modelling an LLM that follows the cite-back prompt.
func TestMethodologyLoop_CiteBackPlumbing(t *testing.T) {
	sb := testenv.NewStoryboard(t)
	agent := sb.Repo("alpha").Branch("main")

	// Seed: synthesis fact + methodology fact tagged for the same
	// domain + entity.
	cSynth := agent.Write("kb/synth/tva.md",
		testenv.Fact("Token volume amplification synthesis").
			Domain("AI economics").
			Entities("Anthropic"),
		"add synth")
	_ = cSynth

	cMeth := agent.Write("kb/meta/reasoning/tva-method.md",
		testenv.Fact("Token volume amplification reasoning").
			Type(fact.Methodology).
			Domain("meta", "reasoning", "methodology", "AI economics").
			Entities("Anthropic"),
		"add methodology")

	// Methodology has zero incoming refs before the hypothesis.
	cMeth.Fact("kb/meta/reasoning/tva-method.md").Incoming().MustHaveCount(0)

	// "LLM" writes a hypothesis citing the methodology (modelling
	// adherence to the cite-back prompt).
	cHyp := agent.Write("kb/hyp/tva-cost.md",
		testenv.Fact("TVA cost hypothesis").
			Type(fact.Hypothesis).
			Domain("AI economics").
			Entities("Anthropic").
			Refs("kb/meta/reasoning/tva-method.md"),
		"hypothesis with methodology cite")

	// Loop closed: methodology now has one incoming ref.
	view := cMeth.Fact("kb/meta/reasoning/tva-method.md").Incoming()
	view.MustHaveCount(1)
	view.MustHaveItem("kb/hyp/tva-cost.md", cHyp.Commit)
}

// TestMethodologyLoop_TagInheritancePlumbing verifies that a methodology
// fact written with the source synthesis fact's tags (modelling LLM
// adherence to the inheritance instruction) is correctly retrievable on
// the inherited tags via RelevantMethodology.
func TestMethodologyLoop_TagInheritancePlumbing(t *testing.T) {
	sb := testenv.NewStoryboard(t)
	agent := sb.Repo("alpha").Branch("main")

	// Seed: synthesis fact with domain=[security], entities=[Anthropic].
	_ = agent.Write("kb/synth/breach.md",
		testenv.Fact("Breach probability synthesis").
			Domain("security").
			Entities("Anthropic"),
		"add synth")

	// "LLM" writes a methodology fact inheriting the source's tags.
	_ = agent.Write("kb/meta/reasoning/breach-method.md",
		testenv.Fact("Breach probability reasoning").
			Type(fact.Methodology).
			Domain("meta", "reasoning", "methodology", "security").
			Entities("Anthropic"),
		"add inherited methodology")

	// RelevantMethodology with source tags retrieves the methodology
	// with full tag overlap.
	var matches []store.MethodologyMatch
	agent.WithRead(func(svc *store.Service) {
		matches, _ = svc.Search().RelevantMethodology(
			context.Background(), "main",
			"breach probability under structural attack",
			[]string{"security"}, []string{"Anthropic"}, 5,
		)
	})

	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}
	if matches[0].TagOverlap != 1.0 {
		t.Fatalf("expected full tag overlap (1.0), got %f", matches[0].TagOverlap)
	}
	if !strings.Contains(matches[0].Path, "breach-method.md") {
		t.Fatalf("unexpected match path: %q", matches[0].Path)
	}
}
