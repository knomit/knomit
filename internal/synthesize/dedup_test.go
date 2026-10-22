package synthesize

import (
	"context"
	"sync"
	"testing"

	"knomit/internal/store/index"

	"knomit/internal/fact"
	"knomit/internal/store"

	"github.com/stretchr/testify/require"
)

// ctxCapture records the first context passed to Search, and how many calls
// there were. The Search it is installed over returns ctx.Err() if the context
// is canceled, and nothing otherwise.
//
// mu is load-bearing, not decoration: dedupCluster fans Search out across
// one goroutine per cluster member (errgroup, limit
// maxConcurrentDedupSearches), so both fields below are written
// concurrently. Unguarded they raced each other, failing the ENTIRE
// package under -race for anyone running the detector — knomit#133. The
// production side guards its own shared state with exactly this shape;
// an override that does not is violating the contract the real
// Search meets, not exposing a defect in dedupCluster.
//
// Readers do NOT need the lock: dedupCluster calls g.Wait() before every
// return, which orders all of the writes above before anything the test
// goroutine does afterwards.
type ctxCapture struct {
	mu     sync.Mutex
	called int
	gotCtx context.Context
}

// captureSearchCtx opens a real in-memory store and overrides its Search with
// the capturing one, so dedupCluster is driven through the real *index.Facade.
func captureSearchCtx(t *testing.T) (*index.Facade, *ctxCapture) {
	t.Helper()
	svc, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { svc.Close() })
	c := &ctxCapture{}
	overridesOf(t, svc.Search()).search(func(index.SearchFunc) index.SearchFunc {
		return func(ctx context.Context, branch string, q index.SearchOptions) ([]index.SearchResult, error) {
			c.mu.Lock()
			c.called++
			// First write wins. The doc comment above has always promised "the
			// first context passed to Search"; last-write-wins never actually
			// delivered it, and with a fan-out there is no defined last.
			if c.gotCtx == nil {
				c.gotCtx = ctx
			}
			c.mu.Unlock()
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return nil, nil
		}
	})
	return svc.Search(), c
}

// TestDedupCluster_NoEmbedderDependency regresses the optimization where
// dedupCluster used to take an `embedders ...model.Embedder` variadic and
// run ONNX inference on every cluster member's title+body — even though
// each fact already has a 768-dim vector in facts_vec. The function now
// resolves vectors via SearchOptions.QueryByPath (a SQL subquery in the
// MATCH operand). If anyone re-introduces an embedder dependency, this
// test stops compiling.
//
// Behavioral coverage: a cluster with fewer than 2 members is a no-op —
// dedupCluster returns it unchanged before touching the facts index,
// the search index, or anything else. That's the cheapest exercise of the new
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
				nil, // facts.Index — never reached for len(cluster) < 2
				nil, // facts.Repo — never reached
				nil, // *index.Facade — never reached
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
	idx, cap := captureSearchCtx(t)

	_, err := dedupCluster(ctx, cluster, nil, nil, idx, 0.92, "test", func(ProgressEvent) {}, "agent/test", bareRefFixture)
	if err == nil {
		t.Fatal("dedupCluster with a canceled ctx must return an error")
	}
	if !errorsIs(err, context.Canceled) {
		t.Errorf("dedupCluster err = %v, want context.Canceled wrapped", err)
	}
	if cap.called == 0 {
		t.Fatal("idx.Search was never invoked — ctx propagation cannot be verified")
	}
	if cap.gotCtx == nil {
		t.Fatal("idx.gotCtx not captured")
	}
	// The captured ctx must carry the cancellation — proves the same
	// ctx tree was threaded down, not a fresh context.Background().
	if cap.gotCtx.Err() == nil {
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
		{File: winnerPath, Kind: "epistemic", Title: "winner", Body: "b", Type: string(fact.Observation), Confidence: winnerConf, Sources: 1},
		{File: loserPath, Kind: "epistemic", Title: "loser", Body: "b", Type: string(fact.Observation), Confidence: loserConf, Sources: 1},
	}
	idx := fixedPairSearch(t, svc, []index.SearchResult{
		searchHit(winnerPath),
		searchHit(loserPath),
	})

	var warnings []string
	surviving, err := dedupCluster(ctx, cluster, svc.Facts(), svc.FactsRepo(), idx, 0.92, "test",
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
