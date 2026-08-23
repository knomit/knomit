package synthesize

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The Phase-3 additions to the discover prompt. All three blocks were read
// verbatim by the designer before they landed (phase3-rulings-2), so these
// tests pin the exact bytes rather than a paraphrase.

// shipFarLaneLine is blueprint §4's far-lane SHIP text, rendered for the motif
// used in the far-lane demo.
const shipFarLaneLine = `These facts are NOT semantically similar. Each claims motif "measure-becomes-target". Propose a keystone only if one mechanism genuinely underlies all members — default to NO.`

func farLanePayload() DiscoverWorkPayload {
	return DiscoverWorkPayload{
		Direction: DiscoverBackward,
		Lane:      LaneFar,
		Bridge: BridgeSeedSet{
			Token: "measure-becomes-target",
			Kind:  BridgeMotif,
			Members: []factForLLM{
				{File: "kb/gotchas/uitesting/1.md", Title: "A", Body: "body a"},
				{File: "kb/technology/benchmarks/2.md", Title: "B", Body: "body b"},
			},
		},
	}
}

// TestRenderDiscoverPrompt_FarLaneCarriesTheShipLine — the line must land after
// the token line and before the member list. Its position is the point: the
// backward preamble above it says the members "share the structural token",
// from which a model reasonably infers they are also SIMILAR, and that is the
// exact inference the far lane exists to prevent (far-lane demo, Part 1).
func TestRenderDiscoverPrompt_FarLaneCarriesTheShipLine(t *testing.T) {
	got := renderDiscoverPrompt(farLanePayload(), "kb")

	require.Contains(t, got, shipFarLaneLine)
	require.Greater(t, strings.Index(got, shipFarLaneLine), strings.Index(got, "Bridge token:"))
	require.Less(t, strings.Index(got, shipFarLaneLine), strings.Index(got, "Members ("))
}

// The far-lane line is the far lane's alone: near-lane motif items and ordinary
// entity/domain bridges must not tell the agent their members are dissimilar,
// because for them it is false.
func TestRenderDiscoverPrompt_ShipLineIsFarLaneOnly(t *testing.T) {
	for _, p := range []DiscoverWorkPayload{
		{Direction: DiscoverForward, Lane: LaneNear,
			Bridge: BridgeSeedSet{Token: "measure-becomes-target", Kind: BridgeMotif}},
		{Direction: DiscoverBackward,
			Bridge: BridgeSeedSet{Token: "some-entity", Kind: BridgeEntity}},
		{Direction: DiscoverForward,
			Bridge: BridgeSeedSet{Token: "some-domain", Kind: BridgeDomain}},
	} {
		require.NotContains(t, renderDiscoverPrompt(p, "kb"), "NOT semantically similar")
	}
}

// GATE rider 2. The hole the far-lane demo found is in the PROMPT, not in the
// motif axis: condition (c) is not answerable from the prompt at all, so every
// discover item must instruct the query — both directions, every kind.
func TestRenderDiscoverPrompt_InstructsRecallBeforeProposing(t *testing.T) {
	const header = "BEFORE YOU ANSWER — QUERY THE CORPUS."
	for _, dir := range []DiscoverDirection{DiscoverForward, DiscoverBackward} {
		for _, kind := range []BridgeKind{BridgeEntity, BridgeDomain, BridgeMotif} {
			got := renderDiscoverPrompt(DiscoverWorkPayload{Direction: dir,
				Bridge: BridgeSeedSet{Token: "tok", Kind: kind}}, "kb")

			require.Contains(t, got, header, "kind=%s dir=%s", kind, dir)
			require.Contains(t, got, "you QUERIED for it above and no existing fact states it",
				"condition (c) must name the query it now depends on")
			require.Less(t, strings.Index(got, header), strings.Index(got, "DECISION RULE"))
		}
	}
}

// GATE rider 3: the third outcome must be offered, and offered with its
// discipline attached — a stated reason, and propose-and-link when torn.
func TestRenderDiscoverPrompt_CarriesTheReinforceInstruction(t *testing.T) {
	got := renderDiscoverPrompt(farLanePayload(), "kb")

	require.Contains(t, got, "REINFORCE — the third outcome, for when the corpus already states it.")
	require.Contains(t, got, "Say why in one sentence; if you cannot write that sentence, they are not the same claim.")
	require.Contains(t, got, "PROPOSE AND LINK instead of reinforcing: a false link is recoverable, a false merge is not.")
	require.Contains(t, got, `"reinforcements"`, "the schema line must advertise the field")

	require.Less(t, strings.Index(got, "REINFORCE — the third outcome"),
		strings.Index(got, "PERSISTENCE"),
		"the outcome belongs with the decision rule, not after the persistence note")
}

// The response SCHEMA the dispatcher hands the agent must advertise
// reinforcements too — the prompt and the schema are two halves of one
// contract, and a field described in prose but absent from the schema is a
// field the agent is told to use and then cannot.
func TestDiscoverResponseSchema_AdvertisesReinforcements(t *testing.T) {
	require.Contains(t, discoverResponseSchema, `"reinforcements"`)
	require.Contains(t, discoverResponseSchema, `"reason"`)
}
