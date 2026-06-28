package mcp

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/synthesize"
)

// TestEnqueueBackwardBridges_ScopeLabel_SetOnPayload verifies that
// the exported synthesize.ScopeLabel helper returns the right string and
// that a DiscoverWorkPayload with that ScopeLabel survives JSON round-trip.
// This is the contract that enqueueBackwardBridgeItems will use to set
// ScopeLabel on backward discover payloads.
func TestEnqueueBackwardBridges_ScopeLabel_SetOnPayload(t *testing.T) {
	scope := synthesize.ScopeFilter{Domain: []string{"auth"}, Entities: []string{"alice"}}
	wantLabel := synthesize.ScopeLabel(scope)
	require.NotEmpty(t, wantLabel, "ScopeLabel must be non-empty for non-empty scope")

	// Construct a payload as enqueueBackwardBridgeItems will, and verify JSON round-trip.
	payload := synthesize.DiscoverWorkPayload{
		Direction:  synthesize.DiscoverBackward,
		ScopeLabel: wantLabel,
		Bridge:     synthesize.BridgeSeedSet{Token: "", Kind: synthesize.BridgeBoth},
	}
	pj, merr := json.Marshal(payload)
	require.NoError(t, merr)
	var decoded synthesize.DiscoverWorkPayload
	require.NoError(t, json.Unmarshal(pj, &decoded))
	require.Equal(t, wantLabel, decoded.ScopeLabel, "ScopeLabel must survive JSON round-trip")
	require.Equal(t, synthesize.DiscoverBackward, decoded.Direction)
}

// TestScopeLabel_EmptyReturnsEmptyString confirms the exported ScopeLabel
// helper returns "" for an empty filter (unscoped / whole-corpus case).
func TestScopeLabel_EmptyReturnsEmptyString(t *testing.T) {
	got := synthesize.ScopeLabel(synthesize.ScopeFilter{})
	require.Empty(t, got, "ScopeLabel(empty filter) must return \"\"")
}
