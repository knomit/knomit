package synthesize

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// TestRenderDiscoverPrompt_DefaultToNo asserts the strict default-skip
// language from Plan 04 Task 1 is present in both forward and backward
// prompts. The exact wording matters — it is the load-bearing instruction
// that keeps the corpus signal-to-noise in check.
func TestRenderDiscoverPrompt_DefaultToNo(t *testing.T) {
	payload := DiscoverWorkPayload{
		Direction: DiscoverForward,
		Bridge: BridgeSeedSet{
			Token: "auth",
			Kind:  BridgeEntity,
			Members: []factForLLM{
				{File: "a.md", Title: "A"},
				{File: "b.md", Title: "B"},
			},
		},
	}
	fwd := renderDiscoverPrompt(payload, "kb")
	require.Contains(t, fwd, "DEFAULT TO NO", "forward prompt missing strict default")
	require.Contains(t, fwd, "strictly ENTAILED", "forward prompt missing entailment language")
	require.Contains(t, fwd, "Skipping is the expected outcome", "forward prompt missing skip-expected language")

	payload.Direction = DiscoverBackward
	bwd := renderDiscoverPrompt(payload, "kb")
	require.Contains(t, bwd, "DEFAULT TO NO", "backward prompt missing strict default")
	require.Contains(t, bwd, "strictly REQUIRED", "backward prompt missing required-by language")
}

func TestParseDiscoverResponse(t *testing.T) {
	raw := `{"proposals":[{"path":"kb/x/p.md","title":"P","body":"b","type":"synthesis","domain":["x"],"confidence":0.8,"entities":[],"refs":["kb/a.md","kb/b.md"]}]}`
	got, err := parseDiscoverResponse(raw)
	require.NoError(t, err)
	require.Len(t, got.Proposals, 1)
	require.Equal(t, "kb/x/p.md", got.Proposals[0].Path)
	require.Equal(t, 0.8, got.Proposals[0].Confidence)
}

// TestApplyDiscoveredProposals_ConfidenceGate exercises Plan 04 Task 2: a
// proposal below the threshold is rejected, one at the threshold is accepted.
func TestApplyDiscoveredProposals_ConfidenceGate(t *testing.T) {
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))
	branch := "agent/test"

	// Seed the two bridge members so refs resolve.
	seedSimpleFact(t, svc, branch, "kb/a.md")
	seedSimpleFact(t, svc, branch, "kb/b.md")

	gates := DiscoveryGates{ConfidenceThreshold: 0.6, DedupThreshold: 999} // disable dedup

	payload := DiscoverWorkPayload{
		Direction: DiscoverForward,
		Bridge: BridgeSeedSet{
			Token: "x",
			Kind:  BridgeEntity,
			Members: []factForLLM{
				{File: "kb/a.md", Title: "A"},
				{File: "kb/b.md", Title: "B"},
			},
		},
	}
	props := []DiscoveredFact{
		{Path: "kb/below.md", Title: "below", Body: "below", Type: "synthesis", Domain: []string{"x"}, Confidence: 0.59, Refs: []string{"kb/a.md", "kb/b.md"}},
		{Path: "kb/at.md", Title: "at", Body: "at", Type: "synthesis", Domain: []string{"x"}, Confidence: 0.6, Refs: []string{"kb/a.md", "kb/b.md"}},
	}

	written, err := applyDiscoveredProposals(context.Background(), svc.Facts(), svc.Search(), nil, payload, props, gates, branch, "kb", nil)
	require.NoError(t, err)
	require.Len(t, written, 1, "only the proposal at-threshold should survive: %v", written)
	// Path normalization rewrites the basename to a UUID; the surviving fact is
	// the one whose title/body was "at", so read it back and confirm.
	rf, err := svc.Facts().ReadFact(context.Background(), branch, written[0], nil)
	require.NoError(t, err)
	require.Contains(t, rf.Content, "# at", "survivor should be the at-threshold proposal")
	require.True(t, strings.HasPrefix(written[0], "kb/"), "wrote %q", written[0])
}

// TestApplyDiscoveredProposals_RefsMustCoverSeeds: a proposal that omits
// one of the bridge members from refs is rejected (proves the agent
// engaged with every input).
func TestApplyDiscoveredProposals_RefsMustCoverSeeds(t *testing.T) {
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))
	branch := "agent/test"

	seedSimpleFact(t, svc, branch, "kb/a.md")
	seedSimpleFact(t, svc, branch, "kb/b.md")

	payload := DiscoverWorkPayload{
		Direction: DiscoverForward,
		Bridge: BridgeSeedSet{
			Token: "x", Kind: BridgeEntity,
			Members: []factForLLM{{File: "kb/a.md", Title: "A"}, {File: "kb/b.md", Title: "B"}},
		},
	}
	// Refs missing kb/b.md → reject.
	props := []DiscoveredFact{{
		Path: "kb/skipped.md", Title: "x", Body: "x", Type: "synthesis",
		Domain: []string{"x"}, Confidence: 0.9, Refs: []string{"kb/a.md"},
	}}
	written, err := applyDiscoveredProposals(context.Background(), svc.Facts(), svc.Search(), nil, payload, props, DiscoveryGates{}, branch, "kb", nil)
	require.NoError(t, err)
	require.Empty(t, written, "incomplete-refs proposal must be rejected")
}

// TestApplyDiscoveredProposals_OriginDiscovered: the written fact carries
// origin=discovered so a follow-up bridgeSeeds call won't re-feed it.
func TestApplyDiscoveredProposals_OriginDiscovered(t *testing.T) {
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))
	branch := "agent/test"

	seedSimpleFact(t, svc, branch, "kb/a.md")
	seedSimpleFact(t, svc, branch, "kb/b.md")

	payload := DiscoverWorkPayload{
		Direction: DiscoverForward,
		Bridge: BridgeSeedSet{
			Token: "x", Kind: BridgeEntity,
			Members: []factForLLM{{File: "kb/a.md", Title: "A"}, {File: "kb/b.md", Title: "B"}},
		},
	}
	props := []DiscoveredFact{{
		Path: "kb/d.md", Title: "d", Body: "d", Type: "synthesis",
		Domain: []string{"x"}, Confidence: 0.9, Refs: []string{"kb/a.md", "kb/b.md"},
	}}

	written, err := applyDiscoveredProposals(context.Background(), svc.Facts(), svc.Search(), nil, payload, props, DiscoveryGates{}, branch, "kb", nil)
	require.NoError(t, err)
	require.Len(t, written, 1)

	rf, err := svc.Facts().ReadFact(context.Background(), branch, written[0], nil)
	require.NoError(t, err)
	parsed, err := fact.ParseFact(written[0], rf.Content)
	require.NoError(t, err)
	require.Equal(t, fact.Discovered, parsed.Origin, "written discovery must carry origin=discovered")
}

// TestApplyDiscoveredProposals_TypeMustMatchDirection: forward = synthesis
// only; backward = hypothesis only.
func TestApplyDiscoveredProposals_TypeMustMatchDirection(t *testing.T) {
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))
	branch := "agent/test"

	seedSimpleFact(t, svc, branch, "kb/a.md")
	seedSimpleFact(t, svc, branch, "kb/b.md")

	payload := DiscoverWorkPayload{
		Direction: DiscoverForward,
		Bridge: BridgeSeedSet{
			Token: "x", Kind: BridgeEntity,
			Members: []factForLLM{{File: "kb/a.md", Title: "A"}, {File: "kb/b.md", Title: "B"}},
		},
	}
	// Try to write a hypothesis on the forward path → rejected.
	props := []DiscoveredFact{{
		Path: "kb/bad.md", Title: "h", Body: "h", Type: "hypothesis",
		Domain: []string{"x"}, Confidence: 0.9, Refs: []string{"kb/a.md", "kb/b.md"},
	}}
	written, err := applyDiscoveredProposals(context.Background(), svc.Facts(), svc.Search(), nil, payload, props, DiscoveryGates{}, branch, "kb", nil)
	require.NoError(t, err)
	require.Empty(t, written, "type/direction mismatch must reject the proposal")
}

// seedSimpleFact writes a minimal observation fact at the given path.
func seedSimpleFact(t *testing.T, svc *store.Service, branch, path string) {
	t.Helper()
	f := fact.NewFact(path)
	f.Title = path
	f.Body = path
	f.Type = fact.Observation
	f.Domain = []string{"test"}
	f.Confidence = 0.5
	f.Sources = 1
	body, err := fact.SerializeFact(f)
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(context.Background(), branch, f.Path(), body, "seed", "")
	require.NoError(t, err)
}

// TestApplyDiscoveredProposals_BackwardBlastGate exercises Plan 04 Task 4:
// a backward (hypothesis) proposal whose seed anchors have no live
// dependents is rejected when BlastRadiusThreshold ≥ 1. With threshold=0
// it passes.
func TestApplyDiscoveredProposals_BackwardBlastGate(t *testing.T) {
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))
	branch := "agent/test"
	// Seeds with no dependents → BlastRadius == 0.
	seedSimpleFact(t, svc, branch, "kb/a.md")
	seedSimpleFact(t, svc, branch, "kb/b.md")

	payload := DiscoverWorkPayload{
		Direction: DiscoverBackward,
		Bridge: BridgeSeedSet{
			Token: "x", Kind: BridgeEntity,
			Members: []factForLLM{{File: "kb/a.md", Title: "A"}, {File: "kb/b.md", Title: "B"}},
		},
	}
	props := []DiscoveredFact{{
		Path: "kb/k.md", Title: "k", Body: "k", Type: "hypothesis",
		Domain: []string{"x"}, Confidence: 0.9, Refs: []string{"kb/a.md", "kb/b.md"},
	}}

	// Threshold=1 → reject (a and b have zero dependents).
	written, err := applyDiscoveredProposals(context.Background(), svc.Facts(), svc.Search(), nil, payload, props, DiscoveryGates{BlastRadiusThreshold: 1}, branch, "kb", nil)
	require.NoError(t, err)
	require.Empty(t, written, "backward proposal must be rejected when seed has no live dependents")

	// Threshold=0 → accept.
	written, err = applyDiscoveredProposals(context.Background(), svc.Facts(), svc.Search(), nil, payload, props, DiscoveryGates{BlastRadiusThreshold: 0}, branch, "kb", nil)
	require.NoError(t, err)
	require.Len(t, written, 1, "with threshold=0 the proposal should land: %v", written)
}

// TestApplyDiscoveredProposals_NoEmbedder_NoDedup confirms emb=nil bypasses
// the dedup gate cleanly (embeddings disabled), without crashing.
func TestApplyDiscoveredProposals_NoEmbedder_NoDedup(t *testing.T) {
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))
	branch := "agent/test"
	seedSimpleFact(t, svc, branch, "kb/a.md")
	seedSimpleFact(t, svc, branch, "kb/b.md")

	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:        "test",
		AgentBranch: branch,
		Svc:         svc,
	})
	_ = ri // hold for future use

	payload := DiscoverWorkPayload{
		Direction: DiscoverForward,
		Bridge: BridgeSeedSet{
			Token: "x", Kind: BridgeEntity,
			Members: []factForLLM{{File: "kb/a.md", Title: "A"}, {File: "kb/b.md", Title: "B"}},
		},
	}
	props := []DiscoveredFact{{
		Path: "kb/d.md", Title: "d", Body: "d", Type: "synthesis",
		Domain: []string{"x"}, Confidence: 0.9, Refs: []string{"kb/a.md", "kb/b.md"},
	}}
	written, err := applyDiscoveredProposals(context.Background(), svc.Facts(), svc.Search(), nil, payload, props, DiscoveryGates{ConfidenceThreshold: 0.5, DedupThreshold: 999}, branch, "kb", nil)
	require.NoError(t, err)
	require.Len(t, written, 1)
}
