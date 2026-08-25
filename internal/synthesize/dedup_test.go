package synthesize

import (
	"context"
	"testing"

	"knomit/internal/fact"
	"knomit/internal/store"

	"github.com/stretchr/testify/require"
)

// ctxCapturingSearchIndex is a minimal SearchIndex stub that records
// the first context passed to Search and returns ctx.Err() if the
// context is canceled. All other interface methods would panic if
// invoked (they aren't, by dedupCluster's call shape).
type ctxCapturingSearchIndex struct {
	store.SearchIndex
	called int
	gotCtx context.Context
}

func (s *ctxCapturingSearchIndex) Search(ctx context.Context, branch string, q store.SearchOptions) ([]store.SearchResult, error) {
	s.called++
	s.gotCtx = ctx
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, nil
}

// TestDedupCluster_NoEmbedderDependency regresses the optimization where
// dedupCluster used to take an `embedders ...store.Embedder` variadic and
// run ONNX inference on every cluster member's title+body — even though
// each fact already has a 768-dim vector in facts_vec. The function now
// resolves vectors via SearchOptions.QueryByPath (a SQL subquery in the
// MATCH operand). If anyone re-introduces an embedder dependency, this
// test stops compiling.
//
// Behavioral coverage: a cluster with fewer than 2 members is a no-op —
// dedupCluster returns it unchanged before touching FactIndex,
// SearchIndex, or anything else. That's the cheapest exercise of the new
// signature without needing a real store stood up.
func TestDedupCluster_NoEmbedderDependency(t *testing.T) {
	cases := []struct {
		name    string
		cluster []factForLLM
	}{
		{"empty cluster", nil},
		{"single fact cluster", []factForLLM{{File: "kb/a.md", Title: "a", Body: "b"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := dedupCluster(
				context.Background(),
				tc.cluster,
				nil, // FactIndex — never reached for len(cluster) < 2
				nil, // SearchIndex — never reached
				0.92,
				"test",
				func(ProgressEvent) {},
				"agent/test",
				bareRefFixture,
			)
			if err != nil {
				t.Fatalf("dedupCluster err = %v", err)
			}
			if len(out) != len(tc.cluster) {
				t.Errorf("dedupCluster returned %d facts, want %d", len(out), len(tc.cluster))
			}
		})
	}
}

// TestDedupCluster_RespectsContextCancellation regresses the bug where
// review.go:90 invoked dedupCluster with context.Background(), so a
// task cancellation issued via the session ctx could not abort the
// dedup pass. The fix passes the session ctx through; this test
// asserts dedupCluster honors ctx cancellation by surfacing
// context.Canceled out of the bounded idx.Search fan-out.
func TestDedupCluster_RespectsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled — first idx.Search must surface ctx.Err()

	cluster := []factForLLM{
		{File: "kb/a.md", Title: "a", Body: "ba"},
		{File: "kb/b.md", Title: "b", Body: "bb"},
	}
	idx := &ctxCapturingSearchIndex{}

	_, err := dedupCluster(ctx, cluster, nil, idx, 0.92, "test", func(ProgressEvent) {}, "agent/test", bareRefFixture)
	if err == nil {
		t.Fatal("dedupCluster with a canceled ctx must return an error")
	}
	if !errorsIs(err, context.Canceled) {
		t.Errorf("dedupCluster err = %v, want context.Canceled wrapped", err)
	}
	if idx.called == 0 {
		t.Fatal("idx.Search was never invoked — ctx propagation cannot be verified")
	}
	if idx.gotCtx == nil {
		t.Fatal("idx.gotCtx not captured")
	}
	// The captured ctx must carry the cancellation — proves the same
	// ctx tree was threaded down, not a fresh context.Background().
	if idx.gotCtx.Err() == nil {
		t.Error("idx received a ctx without cancellation — dedupCluster used context.Background()")
	}
}

// errorsIs is a tiny shim so the test file does not have to import
// "errors" at the top (keeps the diff focused).
func errorsIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// TestDedupCluster_SkipsUnparsableMember regresses the other half of knomit#103:
// the run must survive a bad member, not just a bad ref. dedupCluster used to
// return an error from every per-merge step, so one cluster member whose stored
// content no longer parses — frontmatter from an older serializer, a hand-edit
// on the KB branch — aborted StartSession before it planned anything, and did
// so again on every retry. Every sibling write path in decision.go and
// discovery.go warns and continues; this one now does too.
func TestDedupCluster_SkipsUnparsableMember(t *testing.T) {
	ctx := context.Background()
	svc, branch := newSourcesTestRepo(t)

	const (
		winnerPath = "kb/technology/winner.md"
		loserPath  = "kb/technology/loser.md"
	)

	// Precondition: the confidences that decide which member is read as the
	// winner must differ, or which one is corrupted below is a coin toss.
	const winnerConf, loserConf = 0.9, 0.5
	require.NotEqual(t, winnerConf, loserConf)

	seedOriginFact(t, svc, branch, winnerPath, fact.Observation, fact.Authored, winnerConf, 1, nil)
	seedOriginFact(t, svc, branch, loserPath, fact.Observation, fact.Authored, loserConf, 1, nil)

	// Corrupt the loser in place: still a blob at that path, no longer a
	// parsable fact. The cluster and the index still know about it, which is
	// exactly the state that used to be unrecoverable.
	_, err := svc.Facts().WriteFact(ctx, branch, loserPath, "not a fact any more\n", "corrupt", "")
	require.NoError(t, err)

	cluster := []factForLLM{
		{File: winnerPath, Title: "winner", Body: "b", Type: string(fact.Observation), Confidence: winnerConf, Sources: 1},
		{File: loserPath, Title: "loser", Body: "b", Type: string(fact.Observation), Confidence: loserConf, Sources: 1},
	}
	idx := &fixedPairSearch{
		SearchQuery: svc.Search(),
		results: []store.SearchResult{
			searchHit(winnerPath),
			searchHit(loserPath),
		},
	}

	var warnings []string
	surviving, err := dedupCluster(ctx, cluster, svc.Facts(), idx, 0.92, "test",
		func(e ProgressEvent) {
			if e.Phase == "warn" {
				warnings = append(warnings, e.Message)
			}
		}, branch, bareRefFixture)
	require.NoError(t, err, "an unparsable cluster member must cost its own merge, not the whole pass")

	require.Len(t, warnings, 1, "the skipped merge must be reported, not swallowed")
	require.Contains(t, warnings[0], loserPath)

	// The merge was skipped whole: both facts are still live and still in the
	// surviving cluster, so a later pass can try the pair again.
	require.Len(t, surviving, 2)
}
