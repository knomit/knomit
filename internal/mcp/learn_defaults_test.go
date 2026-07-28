package mcp

import (
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// knomit_learn's input schema advertises `"sources": {"default": 1}` and
// `"confidence": {"default": 0.7}`, but a JSON Schema `default` is
// documentation for the client, not server-side coercion. learnFactInput held
// both as plain Go scalars and validateAndBuildFacts assigned them straight
// through, so an omitted field wrote Go's zero value — 0 sources, 0.0
// confidence — and the advertised default was never applied anywhere.
//
// That is not cosmetic. sources feeds evidence_weight as
// Σ(confidenceᵢ × sourcesᵢ)/(Σ+1) (spec §5.2), so a source with sources=0
// contributes exactly nothing and any synthesis built on such facts scores 0.
// It also decides the dedup identity tiebreak in newFactWins. 68 of the 100
// most recent facts in the project's own KB carry sources: 0 as a result.
//
// Presence, not truthiness: an explicit 0 is a legal value (§2.2 requires only
// >= 0) and must survive, so only an ABSENT field takes the default.

// ptr is the addressable-literal helper the pointer-valued optional fields
// need at their call sites.
func ptr[T any](v T) *T { return &v }

func TestValidateAndBuildFacts_AppliesAdvertisedDefaults(t *testing.T) {
	inputs := []learnFactInput{{
		Topic: "technology", Category: "go", Title: "T", Body: "B",
		// sources and confidence deliberately omitted
	}}
	facts, _, _, _, err := validateAndBuildFacts(nil, "kb", inputs)
	require.NoError(t, err)
	require.Len(t, facts, 1)

	require.Equal(t, 1, facts[0].Sources,
		"an omitted sources must take the schema's advertised default of 1, not Go's zero")
	require.InDelta(t, 0.7, facts[0].Confidence, 1e-9,
		"an omitted confidence must take the schema's advertised default of 0.7, not Go's zero")
}

func TestValidateAndBuildFacts_ExplicitValuesSurvive(t *testing.T) {
	sources := 5
	confidence := 0.95
	inputs := []learnFactInput{{
		Topic: "technology", Category: "go", Title: "T", Body: "B",
		Sources: &sources, Confidence: &confidence,
	}}
	facts, _, _, _, err := validateAndBuildFacts(nil, "kb", inputs)
	require.NoError(t, err)

	require.Equal(t, 5, facts[0].Sources)
	require.InDelta(t, 0.95, facts[0].Confidence, 1e-9)
}

// TestValidateAndBuildFacts_ExplicitZeroSurvives is the guard against
// over-correcting: defaulting on truthiness rather than presence would
// silently rewrite a deliberate "no independent corroborations yet" as 1.
func TestValidateAndBuildFacts_ExplicitZeroSurvives(t *testing.T) {
	sources := 0
	confidence := 0.0
	inputs := []learnFactInput{{
		Topic: "technology", Category: "go", Title: "T", Body: "B",
		Sources: &sources, Confidence: &confidence,
	}}
	facts, _, _, _, err := validateAndBuildFacts(nil, "kb", inputs)
	require.NoError(t, err)

	require.Equal(t, 0, facts[0].Sources, "an explicit 0 is a legal value and must not be defaulted away")
	require.InDelta(t, 0.0, facts[0].Confidence, 1e-9, "an explicit 0.0 confidence must survive")
}

// ── the remaining rows of the sources rule table (spec §5.1) ──────────────

// TestMergeFacts_TransfersLoserSources covers learn's dedup merge. It is a
// TRANSFER: the incoming fact's identity is folded into the existing path and
// only one file survives, so both counts pool. Two independent agents
// arriving at the same claim really is two corroborations.
func TestMergeFacts_TransfersLoserSources(t *testing.T) {
	existing := fact.NewFact("kb/tech/foo.md")
	existing.Title, existing.Body, existing.Type = "E", "eb", fact.Observation
	existing.Confidence, existing.Sources = 0.9, 2

	incoming := fact.NewFact("kb/tech/new.md")
	incoming.Title, incoming.Body, incoming.Type = "N", "nb", fact.Observation
	incoming.Confidence, incoming.Sources = 0.5, 3

	merged := mergeFacts(incoming, existing, "kb/tech/foo.md")
	require.Equal(t, 5, merged.Sources,
		"a dedup merge leaves one file, so both facts' corroborations must pool into it")
}

// TestSubsumeHypothesis_DoesNotPoolHypothesisSources pins the asymmetry that
// makes the rule coherent: when an observation subsumes a hypothesis, the
// hypothesis is retracted and cited as a ref, but its count does NOT transfer.
// A conjecture corroborates nothing — the same reason computeTransfer skips
// hypothesis-typed sources.
func TestSubsumeHypothesis_DoesNotPoolHypothesisSources(t *testing.T) {
	obs := fact.NewFact("kb/tech/obs.md")
	obs.Title, obs.Body, obs.Type = "O", "ob", fact.Observation
	obs.Confidence, obs.Sources = 0.9, 2

	const hypPath = "kb/tech/hyp.md"
	got, retract := subsumeHypothesis(obs, nil, hypPath)

	require.Equal(t, 2, got.Sources,
		"subsuming a hypothesis must not inflate the observation's corroboration count")
	require.Contains(t, got.Refs, hypPath, "the retracted hypothesis is still cited as lineage")
	require.Contains(t, retract, hypPath, "the hypothesis is retracted in the same commit")
}
