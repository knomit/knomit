package synthesize

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/corpus/synthesize/bridge"
	"knomit/internal/corpus/synthesize/discovery"
	"knomit/internal/corpus/synthesize/graph"
	"knomit/internal/corpus/synthesize/step"
	"knomit/internal/domain/fact"
	"knomit/internal/engine/store"
)

// TestReviewQualifyGate_EntityBridgesAreNotEnqueued is the behavioural half,
// and it is built as a PAIR of assertions on ONE fixture so it cannot pass
// vacuously:
//
//  1. the builder, run over the same seeds, DOES produce an entity/domain
//     bridge — so the corpus provably would have served discover items before
//     this change, and a green result is not "the fixture never bridged";
//  2. the session nevertheless enqueues NO discover item, and says why.
//
// Assertion 1 is the load-bearing one. Without it this test would stay green if
// the seeds stopped bridging for any unrelated reason — the fixture-vacuity
// shape that has cost this campaign four fixtures.
func TestReviewQualifyGate_EntityBridgesAreNotEnqueued(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, "agent/test"))
	branch := "agent/test"

	// Four facts on one shared domain token, no motifs anywhere — the exact
	// shape #125 measured on core, and a corpus the motif axis cannot serve.
	paths := []string{"kb/alpha/one.md", "kb/beta/two.md", "kb/gamma/three.md", "kb/delta/four.md"}
	seeds := make([]fact.Fact, 0, len(paths))
	for i, p := range paths {
		f := fact.NewFact(p)
		f.Title, f.Body, f.Type = p, "body of "+p, fact.Observation
		f.Domain, f.Confidence, f.Sources = []string{"auth"}, 0.5, 1
		f.Entities = []string{[]string{"Alpha", "Beta", "Gamma", "Delta"}[i]}
		body, serr := fact.SerializeFact(f)
		require.NoError(t, serr)
		_, werr := svc.Facts().WriteFact(ctx, branch, f.Path(), body, "seed", "")
		require.NoError(t, werr)
		seeds = append(seeds, f)
	}

	// (1) FIXTURE CHECK — these seeds really do form an entity/domain bridge.
	llm := step.FactsForLLM(seeds, testLocalRepoID)
	clusters := graph.ClusterResult{Clusters: map[int][]string{
		0: {paths[0], paths[1]}, 1: {paths[2], paths[3]},
	}}
	cands := bridge.EnumerateBridgeCandidates(llm, clusters, bridge.BridgeBoth, step.ScopeFilter{})
	_, wouldBridge := hasBridgeToken(cands, "auth")
	require.True(t, wouldBridge,
		"fixture check: this corpus DOES produce a domain bridge — without this, "+
			"the absence asserted below would prove nothing")

	// (2) BEHAVIOUR — a real high-effort session serves none of it.
	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "test", AgentBranch: branch, Svc: svc, OntologyRoot: "kb",
	})
	r := NewReviewerWithEffort(ri, nil, step.EffortHigh)

	res, err := r.StartSession(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, res.SessionID)

	items, err := svc.Pipeline().PendingPipelineWorkItems(ctx, res.SessionID)
	require.NoError(t, err)
	require.NotEmpty(t, items,
		"the session must have queued its grounded work — an empty queue would make "+
			"the discover-absence below meaningless")
	for _, it := range items {
		require.NotEqualf(t, "discover", it.StepType,
			"no discover item may be enqueued from a mechanism-free corpus (%s)", it.ClusterKey)
	}

	// (3) AND IT SAYS WHY. A zero that does not explain itself is the
	// saturated-vs-broken confusion this campaign already paid for once.
	require.Contains(t, strings.Join(res.Health, "\n"), "RANK-ONLY",
		"the session must state that the entity/domain axis cannot qualify, so a "+
			"later auditor does not read zero discover items as a stall")
}

// TestDiscoverPayloadShapeUnchanged is a cheap guard on the one thing the
// qualify gate must NOT have changed: a discover item's payload. The gate
// decides WHETHER an item is made, never what is in it.
func TestDiscoverPayloadShapeUnchanged(t *testing.T) {
	b, err := json.Marshal(discovery.DiscoverWorkPayload{
		Direction: discovery.DiscoverForward,
		Bridge:    bridge.BridgeSeedSet{Token: "x", Kind: bridge.BridgeMotif},
	})
	require.NoError(t, err)
	require.Contains(t, string(b), `"kind":"motif"`)
}

// TestHypothesizeQualifyGate_BackwardBridgesAreNotEnqueued covers the SECOND
// surface the qualify gate darkens, and it is the one a reader is most likely
// to misdiagnose.
//
// The hypothesize tool's own discovery is entity/domain-ONLY: the motif axis is
// enumerated in review, over the ordinary seed pool where the motif-carrying
// facts live, and its far lane already routes backward from there. So unlike
// the review surface — which still serves motif bridges on a motif-rich corpus
// — this one now yields nothing on EVERY corpus at EVERY effort. Backward
// discovery is not lost; this path to it is.
//
// Built as a pair, same discipline as the review test: prove the pool would
// have bridged, then prove nothing was served, then prove the session says why.
func TestHypothesizeQualifyGate_BackwardBridgesAreNotEnqueued(t *testing.T) {
	ctx := context.Background()
	svc, ri := newHypothesizeTestRepo(t)

	// Four synthesis facts sharing one domain token across two communities —
	// the pool BuildBackwardBridges is built to bridge.
	paths := []string{"kb/alpha/s1.md", "kb/beta/s2.md", "kb/gamma/s3.md", "kb/delta/s4.md"}
	for _, p := range paths {
		writeTestFact(t, svc, p, p, fact.Synthesis, "auth")
	}

	// FIXTURE CHECK — the same pool really does enumerate a domain bridge.
	// Without it, "no discover items" would be indistinguishable from "these
	// four facts never formed a candidate".
	llm := make([]step.FactForLLM, 0, len(paths))
	for _, p := range paths {
		llm = append(llm, step.FactForLLM{File: p, Domain: []string{"auth"}, Origin: "authored"})
	}
	cands := bridge.EnumerateBridgeCandidates(llm, graph.ClusterResult{Clusters: map[int][]string{
		0: {paths[0], paths[1]}, 1: {paths[2], paths[3]},
	}}, bridge.BridgeBoth, step.ScopeFilter{})
	_, wouldBridge := hasBridgeToken(cands, "auth")
	require.True(t, wouldBridge,
		"fixture check: this synthesis pool DOES enumerate a domain bridge")

	p := NewHypothesizer(ri, nil, step.EffortHigh, step.ScopeFilter{})
	res, err := p.StartSession(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, res.SessionID)

	items, err := svc.Pipeline().PendingPipelineWorkItems(ctx, res.SessionID)
	require.NoError(t, err)
	require.NotEmpty(t, items,
		"the session must have queued its per-fact work — an empty queue would make "+
			"the discover-absence below meaningless")
	for _, it := range items {
		require.NotEqualf(t, "discover", it.StepType,
			"the hypothesize surface can no longer qualify a bridge (%s)", it.ClusterKey)
	}

	// Health is IN-MEMORY ONLY on the session and rides the FIRST result, so
	// it is read from res rather than from the stored row.
	require.Contains(t, strings.Join(res.Health, "\n"), "RANK-ONLY",
		"this surface is dark on EVERY corpus, so it must say so — a reader who "+
			"finds zero discover items here has no other way to tell design from stall")
}

// hasBridgeToken reports whether any candidate carries the token.
func hasBridgeToken(sets []bridge.BridgeSeedSet, token string) (bridge.BridgeSeedSet, bool) {
	for _, s := range sets {
		if s.Token == token {
			return s, true
		}
	}
	return bridge.BridgeSeedSet{}, false
}
