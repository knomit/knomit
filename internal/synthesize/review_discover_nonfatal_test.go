package synthesize

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// TestReviewer_DiscoverParseFailure_NonFatal guards the non-fatal discovery
// contract: a malformed discover response must NOT abort an in-progress review
// session. Discovery is enrichment layered on top of the standard prune/distill
// work; before the fix, parseDiscoverResponse failure returned an error from
// ContinueSession, killing the session and discarding its queued work. The
// hypothesize pipeline already treated the same failure as a no-op skip — this
// brings review into line.
func TestReviewer_DiscoverParseFailure_NonFatal(t *testing.T) {
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))
	branch := "agent/test"

	// Seed a couple facts so StartSession produces a real session with queued
	// prune/distill items.
	for _, slug := range []string{"alpha", "beta"} {
		f := fact.NewFact("kb/test/" + slug + ".md")
		f.Title, f.Body, f.Type = slug, "body "+slug, fact.Observation
		f.Domain, f.Confidence, f.Sources = []string{"test"}, 0.5, 1
		body, serr := fact.SerializeFact(f)
		require.NoError(t, serr)
		_, werr := svc.Facts().WriteFact(context.Background(), branch, f.Path(), body, "seed", "")
		require.NoError(t, werr)
	}

	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "test", AgentBranch: branch, Svc: svc, OntologyRoot: "kb",
	})
	r := NewReviewerWithEffort(ri, nil, EffortHigh)

	res, err := r.StartSession(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, res.SessionID)

	// Inject a discover work item at a high priority so it is the next item
	// ContinueSession picks up. The payload itself is well-formed JSON (an
	// internal-corruption guard stays fatal); only the agent RESPONSE is junk.
	payload := DiscoverWorkPayload{
		Direction: DiscoverForward,
		Bridge: BridgeSeedSet{
			Token: "x", Kind: BridgeEntity,
			Members: []factForLLM{{File: "kb/test/alpha.md", Title: "alpha"}},
		},
	}
	pj, err := json.Marshal(payload)
	require.NoError(t, err)
	require.NoError(t, svc.Pipeline().InsertPipelineWorkItem(context.Background(), store.PipelineWorkItem{
		SessionID:  res.SessionID,
		StepType:   "discover",
		ClusterKey: "discover-inject",
		FactsJSON:  string(pj),
		Priority:   1000, // picked before any standard item
	}))

	// Garbage (non-JSON) response → must be swallowed, session advances.
	_, err = r.ContinueSession(context.Background(), res.SessionID, "this is not json at all")
	require.NoError(t, err, "malformed discover response must be non-fatal")

	// Session must remain active (not aborted) and the discover item answered.
	sess, err := svc.Pipeline().GetPipelineSession(context.Background(), res.SessionID)
	require.NoError(t, err)
	require.Equal(t, "active", sess.Status, "session must stay active after a non-fatal discover skip")
}
