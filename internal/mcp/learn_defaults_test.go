package mcp

import (
	"testing"

	"github.com/stretchr/testify/require"
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
