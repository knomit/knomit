package discovery

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/synthesize/bridge"
	"knomit/internal/corpus/synthesize/internal/synthtest"
	"knomit/internal/corpus/synthesize/step"
	"knomit/internal/domain/fact"
	"knomit/internal/engine/store"
)

// --- Task-17 follow-up: empty-token commit message must not render as "" ---

// TestApplyDiscoveredProposals_EmptyToken_CommitMessage verifies that when
// Bridge.Token == "" (filtered/scoped bridge), the commit message does not
// contain the literal empty-string form `via bridge ""`. Instead it should
// fall back to payload.ScopeLabel or "scoped".
//
// We test this by inspecting the commit message written to the git store after
// a successful proposal ingest. We set up a minimal fact that passes all gates.
func TestApplyDiscoveredProposals_EmptyToken_CommitMessage(t *testing.T) {
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, "agent/test"))
	branch := "agent/test"

	// Seed two member facts so refs-cover-seeds can pass.
	memberPaths := []string{"kb/auth/seed-a.md", "kb/auth/seed-b.md"}
	for _, p := range memberPaths {
		f := fact.NewFact(p)
		f.Title = "seed"
		f.Body = "seed body"
		f.Type = fact.Observation
		f.Domain = []string{"auth"}
		f.Confidence = 0.5
		f.Sources = 1
		body, serr := fact.SerializeFact(f)
		require.NoError(t, serr)
		_, werr := svc.Facts().WriteFact(context.Background(), branch, f.Path(), body, "seed", "")
		require.NoError(t, werr)
	}

	payload := DiscoverWorkPayload{
		Direction:  DiscoverForward,
		ScopeLabel: "auth",
		Bridge: bridge.BridgeSeedSet{
			Token: "", // empty token — filtered bridge
			Kind:  bridge.BridgeBoth,
			Members: []step.FactForLLM{
				{File: memberPaths[0], Title: "Seed A"},
				{File: memberPaths[1], Title: "Seed B"},
			},
		},
	}

	proposals := []DiscoveredFact{{
		Path:       "kb/auth/discovered-t21.md",
		Title:      "T21 test discovery",
		Body:       "Test body for T21.",
		Type:       "synthesis",
		Domain:     step.FlexStrings([]string{"auth"}),
		Confidence: 0.9,
		Entities:   step.FlexStrings([]string{}),
		Refs:       step.FlexStrings(memberPaths),
	}}

	gates := DiscoveryGates{ConfidenceThreshold: 0.5}
	written, err := ApplyDiscoveredProposals(
		context.Background(),
		svc.Facts(),
		svc.FactsRepo(),
		svc.Search(),
		nil, // no embedder → dedup gate disabled
		payload,
		proposals,
		gates,
		branch, synthtest.BareRefFixture, "kb",
		func(step.ProgressEvent) {},
	)
	require.NoError(t, err)
	require.Len(t, written, 1, "proposal should have been written")
	writtenPath := written[0]

	// Read the ACTUAL git commit message for the written fact back from history.
	// This directly guards the discovery.go fallback: empty Bridge.Token must
	// resolve to ScopeLabel (here "auth"), never the literal empty string `""`.
	// Asserting on renderDiscoverPrompt would NOT catch a revert of the bridgeRef
	// fallback, so we read the commit message itself.
	entries, logErr := svc.Search().Log(context.Background(), branch, writtenPath)
	require.NoError(t, logErr)
	require.NotEmpty(t, entries, "the discovered fact must have a commit in history")
	// Log returns newest-first; the discover write is the latest commit on this path.
	msg := entries[0].Message
	require.NotContains(t, msg, `bridge ""`,
		"empty-token discover commit message must NOT render the literal empty bridge string; got %q", msg)
	require.Contains(t, msg, "auth",
		"empty-token discover commit message must fall back to the ScopeLabel; got %q", msg)
	require.Contains(t, msg, "discover-forward: emergent fact via bridge",
		"commit message must keep the standard discover prefix; got %q", msg)

	// Belt-and-suspenders: the prompt surface also avoids the empty-token string.
	prompt := RenderDiscoverWorkItem(payload, "kb").Prompt
	require.NotContains(t, prompt, `bridge ""`, "empty-token prompt must not render empty bridge string")
	require.Contains(t, prompt, "auth", "token-optional prompt must reference the scope label")
}

// TestApplyDiscoveredProposals_EmptyToken_NoScopeLabel_FallsBackToScoped guards
// the second tier of the fallback chain: empty Bridge.Token AND empty ScopeLabel
// must render the bridge ref as "scoped", never `""`.
func TestApplyDiscoveredProposals_EmptyToken_NoScopeLabel_FallsBackToScoped(t *testing.T) {
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, "agent/test"))
	branch := "agent/test"

	memberPaths := []string{"kb/auth/seed-c.md", "kb/auth/seed-d.md"}
	for _, p := range memberPaths {
		f := fact.NewFact(p)
		f.Title, f.Body, f.Type = "seed", "seed body", fact.Observation
		f.Domain, f.Confidence, f.Sources = []string{"auth"}, 0.5, 1
		body, serr := fact.SerializeFact(f)
		require.NoError(t, serr)
		_, werr := svc.Facts().WriteFact(context.Background(), branch, f.Path(), body, "seed", "")
		require.NoError(t, werr)
	}

	payload := DiscoverWorkPayload{
		Direction:  DiscoverForward,
		ScopeLabel: "", // empty scope label → fallback must be "scoped"
		Bridge: bridge.BridgeSeedSet{
			Token: "", Kind: bridge.BridgeBoth,
			Members: []step.FactForLLM{{File: memberPaths[0], Title: "C"}, {File: memberPaths[1], Title: "D"}},
		},
	}
	proposals := []DiscoveredFact{{
		Path:       "kb/auth/discovered-t21b.md",
		Title:      "T21b",
		Body:       "Body.",
		Type:       "synthesis",
		Domain:     step.FlexStrings([]string{"auth"}),
		Confidence: 0.9,
		Refs:       step.FlexStrings(memberPaths),
	}}

	written, err := ApplyDiscoveredProposals(
		context.Background(), svc.Facts(), svc.FactsRepo(), svc.Search(), nil,
		payload, proposals, DiscoveryGates{ConfidenceThreshold: 0.5}, branch, synthtest.BareRefFixture, "kb", func(step.ProgressEvent) {},
	)
	require.NoError(t, err)
	require.Len(t, written, 1)

	entries, logErr := svc.Search().Log(context.Background(), branch, written[0])
	require.NoError(t, logErr)
	require.NotEmpty(t, entries)
	msg := entries[0].Message
	require.NotContains(t, msg, `bridge ""`, "empty token + empty label must not render empty bridge string; got %q", msg)
	require.Contains(t, msg, `bridge "scoped"`, "empty token + empty label must fall back to \"scoped\"; got %q", msg)
}
