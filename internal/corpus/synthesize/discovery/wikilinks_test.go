package discovery

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/synthesize/bridge"
	"knomit/internal/corpus/synthesize/internal/synthtest"
	"knomit/internal/corpus/synthesize/step"
	"knomit/internal/engine/store"
)

// A discovered fact is a new body: [[...]] in its prose is rejected, the
// knomit_learn rule.
func TestApplyDiscoveredProposals_WikiLinkInProseRejected(t *testing.T) {
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, "agent/test"))
	branch := "agent/test"
	seedSimpleFact(t, svc, branch, "kb/a.md")
	seedSimpleFact(t, svc, branch, "kb/b.md")

	payload := DiscoverWorkPayload{
		Direction: DiscoverForward,
		Bridge: bridge.BridgeSeedSet{Token: "x", Kind: bridge.BridgeEntity,
			Members: []step.FactForLLM{{File: "kb/a.md", Title: "A"}, {File: "kb/b.md", Title: "B"}}},
	}
	props := []DiscoveredFact{
		{Path: "kb/bad.md", Title: "bad", Body: "bridges [[a]] and [[b]]", Type: "synthesis", Domain: []string{"x"}, Confidence: 0.9, Refs: []string{"kb/a.md", "kb/b.md"}},
		{Path: "kb/good.md", Title: "good", Body: "bridges a and b", Type: "synthesis", Domain: []string{"x"}, Confidence: 0.9, Refs: []string{"kb/a.md", "kb/b.md"}},
	}
	var warns []string
	onProgress := func(e step.ProgressEvent) {
		if e.Phase == "warn" {
			warns = append(warns, e.Message)
		}
	}
	gates := DiscoveryGates{ConfidenceThreshold: 0.6, DedupThreshold: 999}
	written, err := ApplyDiscoveredProposals(context.Background(), svc.Facts(), svc.FactsRepo(), svc.Search(), nil, payload, props, gates, branch, synthtest.BareRefFixture, "kb", onProgress)
	require.NoError(t, err)
	require.Len(t, written, 1, "only the plain body is written: %v", warns)
	rf, err := svc.Facts().ReadFact(context.Background(), branch, written[0], nil)
	require.NoError(t, err)
	require.Contains(t, rf.Content, "# good")
	require.Len(t, warns, 1, "%v", warns)
	require.Contains(t, warns[0], "discovery rejected: kb/bad.md")
	require.Contains(t, warns[0], "[[a]] (line 1)")
}
