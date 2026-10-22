package synthtest

import (
	"context"
	"sort"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/corpus/synthesize/step"
	"knomit/internal/domain/fact"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/index"
)

// DefaultNeighborKinds is the production default of [cluster_cache]
// neighbor_kinds, for tests that call a clustering entry point directly.
var DefaultNeighborKinds = []string{"epistemic"}

var WidenedNeighborKinds = []string{"epistemic", "pragmatic"}

const (
	KindDir     = "kb/technology/kinds/"
	KindObs     = KindDir + "obs.md"
	KindPol     = KindDir + "pol.md"
	KindObs2    = KindDir + "obs2.md"
	kindTitle   = "Retry the upload after a transient failure"
	kindBody    = "What happens to an upload when the store is briefly unavailable."
	kindPolText = "Always retry the upload after a transient failure"
	kindObs2Tt  = "Uploads are retried after transient store failures"

	// A cousin directory: a sibling of kindDir, so neither is a prefix of the
	// other and ScopedCluster's category fence separates them.
	cousinKindDir = "kb/technology/kinds-elsewhere/"
	CousinKindObs = cousinKindDir + "obs-cousin.md"
	CousinKindPol = cousinKindDir + "pol-cousin.md"
	CousinObsTt   = "An upload is retried when the store fails transiently"
	CousinPolTt   = "Retry every upload that failed transiently"
)

// KindEnv builds a store whose embedder places the facts on one axis:
// obs at 0, pol at 0.01, obs2 at 0.02 — so pol sits BETWEEN the two
// observations, as close to each as they are to each other, and far above the
// dedup floor. neighborKinds is what the repo instance's config says (nil =
// the default).
func KindEnv(t *testing.T, neighborKinds []string) *RestatementEnv {
	t.Helper()
	emb := &RestatementEmbedder{VectorFor: func(text string) []float32 {
		switch text {
		case kindTitle + " " + kindBody:
			return AxisVector(0)
		case kindPolText + " " + kindBody:
			return AxisVector(0.01)
		case kindObs2Tt + " " + kindBody:
			return AxisVector(0.02)
		case CousinObsTt + " " + kindBody:
			return AxisVector(0.005)
		case CousinPolTt + " " + kindBody:
			return AxisVector(0.015)
		}
		return nil
	}}
	env := NewRestatementEnvWith(t, 0, emb)
	env.RI = repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:          "test",
		AgentBranch:   env.Branch,
		Svc:           env.Svc,
		OntologyRoot:  "kb",
		Embedder:      emb,
		NeighborKinds: neighborKinds,
	})
	WriteKindedFact(t, env, KindObs, kindTitle, fact.Epistemic, fact.Observation)
	WriteKindedFact(t, env, KindPol, kindPolText, fact.Pragmatic, fact.Policy)
	WriteKindedFact(t, env, KindObs2, kindObs2Tt, fact.Epistemic, fact.Observation)
	return env
}

func WriteKindedFact(t *testing.T, env *RestatementEnv, path, title string, kind fact.Kind, typ fact.Type) {
	t.Helper()
	f := fact.NewFact(path)
	f.Title = title
	f.Body = kindBody
	f.Kind = kind
	f.Type = typ
	f.Confidence = 0.7
	f.Sources = 1
	content, err := fact.SerializeFact(f)
	require.NoError(t, err)
	_, err = env.Svc.Facts().WriteFact(context.Background(), env.Branch, path, content, "write "+path, "test")
	require.NoError(t, err)
}

// RequireUnfilteredSearchFinds is the premise every test here rests on: with
// no kind filter, a QueryByPath search from `from` returns `want`.
func RequireUnfilteredSearchFinds(t *testing.T, env *RestatementEnv, from, want string, opts index.SearchOptions) {
	t.Helper()
	opts.QueryByPath = from
	if opts.Limit == 0 {
		opts.Limit = 10
	}
	hits, err := env.Svc.Search().Search(context.Background(), env.Branch, opts)
	require.NoError(t, err)
	for _, h := range hits {
		if h.Path == want {
			return
		}
	}
	require.Failf(t, "PREMISE", "an unfiltered search from %s must return %s, or the filter is credited for nothing", from, want)
}

// CompleteEdges seeds the SIMILAR_TO edges the test store does not build. With
// none, the real SubgraphEdges returns none; Louvain then
// returns one SINGLETON per node — non-empty, so ScopedCluster's category
// fallback never fires — and filterSmallClusters drops every singleton: ZERO
// clusters, over which a "pol is in no cluster" assertion is vacuous. Every
// pair of the store's facts gets an edge instead (through the graph test seams,
// so the real SubgraphEdges reads them back), so membership is decided by
// exactly one thing: which paths the neighbour search let into the subgraph.
// The tests also pass resolution 1.0, because the production 4.0 splits even a
// connected 2-node graph.
func CompleteEdges(t *testing.T, svc *store.Service, branch string) *index.Facade {
	t.Helper()
	e := &GraphEnv{T: t, Svc: svc, Branch: branch}
	paths := make([]string, 0)
	for p := range e.nodeIDs() {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	e.SimilarAll(paths)
	return svc.Search()
}

// RecordingSearch records every path a QueryByPath search returned, and the
// kinds it asked for, so a test can say what a caller's neighbour searches
// could possibly have put into a cluster.
type RecordingSearch struct {
	Mu       sync.Mutex
	Returned map[string]bool
	Kinds    [][]string
}

// RecordSearch wraps svc's real Search so every call is recorded, and returns
// svc's *index.Facade with the wrapper installed until the test ends.
func RecordSearch(t *testing.T, svc *store.Service) (*index.Facade, *RecordingSearch) {
	t.Helper()
	r := &RecordingSearch{}
	idx := svc.Search()
	OverridesOf(t, idx).Search(func(next index.SearchFunc) index.SearchFunc {
		return func(ctx context.Context, branch string, q index.SearchOptions) ([]index.SearchResult, error) {
			res, err := next(ctx, branch, q)
			if q.QueryByPath != "" {
				r.Mu.Lock()
				defer r.Mu.Unlock()
				if r.Returned == nil {
					r.Returned = map[string]bool{}
				}
				for _, h := range res {
					r.Returned[h.Path] = true
				}
				r.Kinds = append(r.Kinds, append([]string(nil), q.IncludeKinds...))
			}
			return res, err
		}
	})
	return idx, r
}

func ClusterPaths(clusters [][]step.FactForLLM) map[string]bool {
	out := map[string]bool{}
	for _, c := range clusters {
		for _, f := range c {
			out[f.File] = true
		}
	}
	return out
}

func EpistemicSeed(path string) step.FactForLLM {
	return step.FactForLLM{File: path, Kind: string(fact.Epistemic), Type: string(fact.Observation)}
}
