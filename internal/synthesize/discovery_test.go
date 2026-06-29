package synthesize

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/retrieval"
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
// origin=discovered so a follow-up bridge engine call won't re-feed it.
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

// TestRenderDiscoverPrompt_TokenPresent_ContainsBridgeTokenLine verifies the
// existing (token != "") variant still emits the "Bridge token:" line.
func TestRenderDiscoverPrompt_TokenPresent_ContainsBridgeTokenLine(t *testing.T) {
	payload := DiscoverWorkPayload{
		Direction: DiscoverForward,
		Bridge: BridgeSeedSet{
			Token: "auth",
			Kind:  BridgeEntity,
			Members: []factForLLM{
				{File: "kb/a.md", Title: "A"},
			},
		},
	}
	out := renderDiscoverPrompt(payload, "kb")
	require.Contains(t, out, `Bridge token:`, "token-present prompt must emit Bridge token: line")
	require.Contains(t, out, `Members (1):`)
	require.Contains(t, out, "(d) You can cite every seed fact above in refs")

	// extract RESPONSE SCHEMA line for comparison with token-optional variant
	schemaLine := extractResponseSchemaLine(out)
	require.NotEmpty(t, schemaLine, "token-present prompt must contain RESPONSE SCHEMA line")

	// backward direction also works
	payload.Direction = DiscoverBackward
	bwd := renderDiscoverPrompt(payload, "kb")
	require.Contains(t, bwd, `Bridge token:`)
}

// TestRenderDiscoverPrompt_TokenEmpty_Forward verifies the token-optional
// (scope-framed) variant for forward direction.
func TestRenderDiscoverPrompt_TokenEmpty_Forward(t *testing.T) {
	payload := DiscoverWorkPayload{
		Direction:  DiscoverForward,
		ScopeLabel: "auth",
		Bridge: BridgeSeedSet{
			Token: "",
			Kind:  BridgeDomain,
			Members: []factForLLM{
				{File: "kb/a.md", Title: "A", Body: "body a"},
				{File: "kb/b.md", Title: "B"},
			},
		},
	}
	out := renderDiscoverPrompt(payload, "kb")

	require.NotContains(t, out, "Bridge token:", "token-optional prompt must NOT emit Bridge token: line")
	require.Contains(t, out, "auth", "scope label must appear in prompt")
	require.Contains(t, out, "Members (2):")
	require.Contains(t, out, "(d) You can cite every seed fact above in refs")
	require.Contains(t, out, "RESPONSE SCHEMA")
	require.Contains(t, out, "strictly ENTAILED", "forward token-optional must keep entailment language")

	// RESPONSE SCHEMA line must be byte-identical to the token-present variant.
	tokenPresent := DiscoverWorkPayload{
		Direction: DiscoverForward,
		Bridge: BridgeSeedSet{
			Token: "auth", Kind: BridgeEntity,
			Members: []factForLLM{{File: "kb/a.md", Title: "A"}},
		},
	}
	require.Equal(t, extractResponseSchemaLine(renderDiscoverPrompt(tokenPresent, "kb")),
		extractResponseSchemaLine(out),
		"RESPONSE SCHEMA line must be identical between token-present and token-optional variants")
}

// TestRenderDiscoverPrompt_MandatesDiscoveredOrigin verifies that, at proposal
// time, every discover prompt variant instructs the agent that a fact persisted
// from this bridge-formed group is origin=discovered — keyed off the grouping
// method (a bridge), not the review act. This is the load-bearing instruction
// that keeps emergent facts from defaulting to authored when saved via
// knomit_learn.
func TestRenderDiscoverPrompt_MandatesDiscoveredOrigin(t *testing.T) {
	variants := []struct {
		name    string
		payload DiscoverWorkPayload
	}{
		{"forward-token", DiscoverWorkPayload{Direction: DiscoverForward, Bridge: BridgeSeedSet{Token: "auth", Kind: BridgeEntity, Members: []factForLLM{{File: "kb/a.md", Title: "A"}}}}},
		{"backward-token", DiscoverWorkPayload{Direction: DiscoverBackward, Bridge: BridgeSeedSet{Token: "auth", Kind: BridgeEntity, Members: []factForLLM{{File: "kb/a.md", Title: "A"}}}}},
		{"forward-scoped", DiscoverWorkPayload{Direction: DiscoverForward, ScopeLabel: "auth", Bridge: BridgeSeedSet{Kind: BridgeDomain, Members: []factForLLM{{File: "kb/a.md", Title: "A"}}}}},
		{"backward-scoped", DiscoverWorkPayload{Direction: DiscoverBackward, ScopeLabel: "auth", Bridge: BridgeSeedSet{Kind: BridgeDomain, Members: []factForLLM{{File: "kb/a.md", Title: "A"}}}}},
	}
	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			out := renderDiscoverPrompt(v.payload, "kb")
			require.Contains(t, out, "origin: discovered",
				"discover prompt must name the discovered origin at proposal time")
			require.Contains(t, out, "MUST set origin: discovered and cite every seed fact above in refs",
				"discover prompt must mandate origin + refs for the knomit_learn save path")
			require.Contains(t, out, "BRIDGE",
				"the instruction must tie origin to the bridge grouping method")
		})
	}
}

// TestRenderDistillWorkItem_MandatesDistilledOrigin verifies the symmetric
// rule for the regular-cluster path: a fact distilled from a cluster is
// origin=distilled, and saving it directly via knomit_learn after previewing
// requires setting that origin and citing the cluster sources.
func TestRenderDistillWorkItem_MandatesDistilledOrigin(t *testing.T) {
	wic, err := RenderDistillWorkItem(
		[]factForLLM{{File: "kb/a.md", Title: "A"}, {File: "kb/b.md", Title: "B"}},
		"kb", "")
	require.NoError(t, err)
	require.Contains(t, wic.Prompt, "origin: distilled",
		"distill prompt must name the distilled origin at proposal time")
	require.Contains(t, wic.Prompt, "knomit_learn",
		"distill prompt must address the direct-save path")
}

// TestRenderDiscoverPrompt_TokenEmpty_Backward verifies the token-optional
// (scope-framed) variant for backward direction.
func TestRenderDiscoverPrompt_TokenEmpty_Backward(t *testing.T) {
	payload := DiscoverWorkPayload{
		Direction:  DiscoverBackward,
		ScopeLabel: "auth",
		Bridge: BridgeSeedSet{
			Token: "",
			Kind:  BridgeDomain,
			Members: []factForLLM{
				{File: "kb/a.md", Title: "A"},
			},
		},
	}
	out := renderDiscoverPrompt(payload, "kb")

	require.NotContains(t, out, "Bridge token:", "backward token-optional prompt must NOT emit Bridge token: line")
	require.Contains(t, out, "auth", "scope label must appear in backward token-optional prompt")
	require.Contains(t, out, "Members (1):")
	require.Contains(t, out, "(d) You can cite every seed fact above in refs")
	require.Contains(t, out, "strictly REQUIRED", "backward token-optional must keep required-by language")

	// RESPONSE SCHEMA must match backward token-present.
	tokenPresent := DiscoverWorkPayload{
		Direction: DiscoverBackward,
		Bridge: BridgeSeedSet{
			Token: "auth", Kind: BridgeEntity,
			Members: []factForLLM{{File: "kb/a.md", Title: "A"}},
		},
	}
	require.Equal(t, extractResponseSchemaLine(renderDiscoverPrompt(tokenPresent, "kb")),
		extractResponseSchemaLine(out),
		"RESPONSE SCHEMA line must be identical between token-present and token-optional variants")
}

// TestRenderDiscoverPrompt_TokenEmpty_NoScopeLabel_FallsBack verifies that
// when Token=="" and ScopeLabel=="" the prompt falls back to "the scoped area".
func TestRenderDiscoverPrompt_TokenEmpty_NoScopeLabel_FallsBack(t *testing.T) {
	payload := DiscoverWorkPayload{
		Direction: DiscoverForward,
		Bridge: BridgeSeedSet{
			Token: "",
			Kind:  BridgeDomain,
			Members: []factForLLM{
				{File: "kb/a.md", Title: "A"},
			},
		},
	}
	out := renderDiscoverPrompt(payload, "kb")
	require.NotContains(t, out, "Bridge token:")
	require.Contains(t, out, "the scoped area", "empty ScopeLabel must fall back to 'the scoped area'")
}

// extractResponseSchemaLine returns the line starting with "RESPONSE SCHEMA:"
// from a rendered discover prompt.
func extractResponseSchemaLine(prompt string) string {
	for _, line := range strings.Split(prompt, "\n") {
		if strings.HasPrefix(line, "RESPONSE SCHEMA:") {
			return line
		}
	}
	return ""
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

// TestApplyDiscoveredProposals_EmbedError_FallsThrough is the regression guard
// for the isDuplicate-error-drops-proposal bug. Before the fix, an embedder
// runtime error caused `continue`, dropping the proposal before the BlastRadius
// gate ran. A valid keystone was silently lost whenever the embedder was
// unavailable.
func TestApplyDiscoveredProposals_EmbedError_FallsThrough(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))
	branch := "agent/test"

	seedSimpleFact(t, svc, branch, "kb/a.md")
	seedSimpleFact(t, svc, branch, "kb/b.md")

	// Mock embedder that always returns an error on EmbedDocument.
	mockEmb := NewMockEmbedder(ctrl)
	mockEmb.EXPECT().EmbedDocument(gomock.Any(), gomock.Any()).
		Return(nil, fmt.Errorf("embedder unavailable")).AnyTimes()
	mockEmb.EXPECT().Dim().Return(768).AnyTimes()
	mockEmb.EXPECT().ID().Return("mock").AnyTimes()
	mockEmb.EXPECT().Thresholds().Return(retrieval.Thresholds{}).AnyTimes()

	payload := DiscoverWorkPayload{
		Direction: DiscoverForward,
		Bridge: BridgeSeedSet{
			Token: "auth",
			Kind:  BridgeEntity,
			Members: []factForLLM{
				{File: "kb/a.md", Title: "A"},
				{File: "kb/b.md", Title: "B"},
			},
		},
	}
	proposals := []DiscoveredFact{
		{
			Path:       "kb/p/q.md",
			Title:      "Q",
			Body:       "body",
			Type:       "synthesis",
			Domain:     []string{"auth"},
			Confidence: 0.9,
			Refs:       []string{"kb/a.md", "kb/b.md"},
		},
	}
	gates := DiscoveryGates{ConfidenceThreshold: 0.5, DedupThreshold: 0.9}
	var warnings []string
	onProgress := func(e ProgressEvent) {
		if e.Phase == "warn" {
			warnings = append(warnings, e.Message)
		}
	}

	written, err := applyDiscoveredProposals(context.Background(), svc.Facts(), svc.Search(),
		mockEmb, payload, proposals, gates, branch, "kb", onProgress)
	require.NoError(t, err)
	require.Len(t, written, 1,
		"proposal must survive an embedder error: embed error ≠ duplicate; got warnings: %v", warnings)
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
