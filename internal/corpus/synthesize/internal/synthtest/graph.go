package synthtest

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/synthesize/step"
	"knomit/internal/domain/fact"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/index"
	"knomit/internal/engine/store/storetest"
)

// GraphEnv is a real store opened for a test that drives the search handle
// (*index.Facade) over a seeded corpus: real facts, so TokenDF counts and
// DERIVED_FROM edges come from the index's own tables, and SIMILAR_TO edges
// written through the index's graph test seams, so SimilarityAdjacency and
// SubgraphEdges read them back through the real queries.
//
// Nothing here replaces a read. A test that needs a read to FAIL, or to prove
// that a code path made no read at all, uses guardReads.
type GraphEnv struct {
	T      *testing.T
	Svc    *store.Service
	Branch string
	// Log is the reads seen under the current policy.
	Log *ReadLog
}

func NewGraphEnv(t *testing.T, branch string) *GraphEnv {
	t.Helper()
	svc, err := store.Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, branch))
	e := &GraphEnv{T: t, Svc: svc, Branch: branch}
	// Strict by default: the handle fails the test on ANY read of the index
	// until the test widens the policy with allow / allowAll (or guardReads).
	e.Log = GuardReads(t, svc.Search(), ReadPolicy{})
	return e
}

// Allow widens the strict default to the named reads and returns the env.
func (e *GraphEnv) Allow(methods ...string) *GraphEnv {
	e.Log = GuardReads(e.T, e.Idx(), ReadPolicy{Allow: methods})
	return e
}

// allowAll turns the strict default off.
func (e *GraphEnv) allowAll() *GraphEnv {
	e.Log = GuardReads(e.T, e.Idx(), ReadPolicy{AllowAll: true})
	return e
}

// Idx is the store's own search handle.
func (e *GraphEnv) Idx() *index.Facade { return e.Svc.Search() }

// SeedFact is one fact to write.
type SeedFact struct {
	Path     string
	Domains  []string
	Entities []string
	Motifs   []string
	Refs     []string
	// Zero values mean: observation, authored, confidence 0.8, one source.
	Typ        fact.Type
	Origin     fact.Origin
	Confidence float64
	Sources    int
}

// seedFromResult is the fact a search-result fixture stands for.
func seedFromResult(r index.SearchResult) SeedFact {
	return SeedFact{
		Path: r.Path, Domains: r.Domain, Entities: r.Entities, Motifs: r.Motifs,
		Typ: fact.Type(r.Type), Origin: fact.Origin(r.Origin),
		Confidence: r.Confidence, Sources: r.Sources,
	}
}

// seedFromLLM is the fact a bridge seed stands for.
func seedFromLLM(f step.FactForLLM) SeedFact {
	s := SeedFact{Path: f.File, Domains: f.Domain, Entities: f.Entities, Motifs: f.Motifs, Origin: fact.Origin(f.Origin)}
	if s.Origin == fact.Discovered {
		s.Typ = fact.Synthesis // the only types a discovered fact may have
	}
	return s
}

// Seed writes the facts in ONE commit.
func (e *GraphEnv) Seed(specs ...SeedFact) *GraphEnv {
	e.T.Helper()
	files := make(map[string]string, len(specs))
	for _, s := range specs {
		f := fact.NewFact(s.Path)
		f.Title = s.Path
		f.Body = "body of " + s.Path
		f.Type, f.Origin, f.Confidence, f.Sources = fact.Observation, fact.Authored, 0.8, 1
		if s.Typ != "" {
			f.Type = s.Typ
		}
		if s.Origin != "" {
			f.Origin = s.Origin
		}
		if s.Confidence != 0 {
			f.Confidence = s.Confidence
		}
		if s.Sources != 0 {
			f.Sources = s.Sources
		}
		f.Domain = s.Domains
		f.Entities = s.Entities
		f.Motifs = s.Motifs
		f.Refs = s.Refs
		content, err := fact.SerializeFact(f)
		require.NoError(e.T, err)
		files[f.Path()] = content
	}
	_, _, err := e.Svc.Facts().BatchWriteFacts(context.Background(), e.Branch, files, nil, "seed", "")
	require.NoError(e.T, err)
	return e
}

// SeedAll writes one fact per seed, carrying its domains, entities and motifs.
func (e *GraphEnv) SeedAll(seeds []step.FactForLLM) *GraphEnv {
	e.T.Helper()
	specs := make([]SeedFact, len(seeds))
	for i, s := range seeds {
		specs[i] = seedFromLLM(s)
	}
	return e.Seed(specs...)
}

// nodeIDs maps every seeded path to its fact node in the graph.
func (e *GraphEnv) nodeIDs() map[string]int64 {
	e.T.Helper()
	rows, err := storetest.RawDB(e.Svc).Query(`
		SELECT p.value, p.node_id FROM node_props_text p
		JOIN property_keys k ON k.id = p.key_id AND k.key = 'path'`)
	require.NoError(e.T, err)
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var path string
		var id int64
		require.NoError(e.T, rows.Scan(&path, &id))
		out[path] = id
	}
	require.NoError(e.T, rows.Err())
	return out
}

// Similar writes a SIMILAR_TO edge for each pair, between the pair's seeded
// facts (the graph test seams, not a replaced read).
func (e *GraphEnv) Similar(pairs ...[2]string) *GraphEnv {
	e.T.Helper()
	ids := e.nodeIDs()
	db := storetest.RawDB(e.Svc)
	for _, p := range pairs {
		a, okA := ids[p[0]]
		b, okB := ids[p[1]]
		require.True(e.T, okA && okB, "similar(%v): both facts must be seeded first", p)
		require.NoError(e.T, index.GraphMergeEdgeForTest(context.Background(), db, a, b, index.EdgeSimilarTo))
	}
	return e
}

// SimilarAll writes SIMILAR_TO between every pair of paths.
func (e *GraphEnv) SimilarAll(paths []string) *GraphEnv {
	e.T.Helper()
	var pairs [][2]string
	for i := 0; i < len(paths); i++ {
		for j := i + 1; j < len(paths); j++ {
			pairs = append(pairs, [2]string{paths[i], paths[j]})
		}
	}
	return e.Similar(pairs...)
}

// OvSet composes the test-only overrides of one store. index.Overrides is
// installed as a whole, so every helper that needs one goes through the OvSet of
// its handle and the pieces accumulate instead of replacing each other.
type OvSet struct {
	T       *testing.T
	Idx     *index.Facade
	ov      index.Overrides
	Restore func()
}

var ovSets sync.Map // *index.Facade -> *ovSet

// OverridesOf returns the ovSet of idx, creating it on first use. The
// overrides are removed when the test ends.
func OverridesOf(t *testing.T, idx *index.Facade) *OvSet {
	t.Helper()
	if s, ok := ovSets.Load(idx); ok {
		return s.(*OvSet)
	}
	s := &OvSet{T: t, Idx: idx}
	ovSets.Store(idx, s)
	t.Cleanup(func() {
		if s.Restore != nil {
			s.Restore()
		}
		ovSets.Delete(idx)
	})
	return s
}

func (s *OvSet) Apply() {
	if s.Restore != nil {
		s.Restore()
	}
	s.Restore = s.Idx.SetOverridesForTest(s.ov)
}

// Search installs the Search wrapper.
func (s *OvSet) Search(w func(next index.SearchFunc) index.SearchFunc) {
	s.ov.Search = w
	s.Apply()
}

// SubgraphEdges installs the clustering edge read.
func (s *OvSet) SubgraphEdges(f func(ctx context.Context, paths []string) ([][2]string, error)) {
	s.ov.SubgraphEdges = f
	s.Apply()
}

// ReadPolicy says which reads of the search handle a test allows.
type ReadPolicy struct {
	// Allow names the reads that may happen. Any other read fails the test.
	Allow []string
	// AllowAll turns the strictness off (for a test that only injects Fail).
	AllowAll bool
	// Fail makes the named reads return the given error. The four reads that
	// return an error (SimilarityAdjacency, ReverseDependentPaths, TokenDF,
	// SubgraphEdges) hand it to the caller.
	Fail map[string]error
}

// ReadLog is the reads guardReads saw.
type ReadLog struct {
	Mu    sync.Mutex
	Names []string
}

func (l *ReadLog) Add(n string) {
	l.Mu.Lock()
	l.Names = append(l.Names, n)
	l.Mu.Unlock()
}

// Count is the number of reads seen, allowed or not.
func (l *ReadLog) Count() int {
	l.Mu.Lock()
	defer l.Mu.Unlock()
	return len(l.Names)
}

// CountOf is the number of reads of one method.
func (l *ReadLog) CountOf(method string) int {
	l.Mu.Lock()
	defer l.Mu.Unlock()
	n := 0
	for _, m := range l.Names {
		if m == method {
			n++
		}
	}
	return n
}

// GuardReads installs a read policy on the handle for the rest of the test.
// The policy is store-wide (see index.Overrides): every read of every query
// service of that store passes through it. It is strict by default: a read the
// policy does not allow fails the test, which restores the "any other call
// fails" property the gomock fakes had for every method. A Fail entry must name
// a read that can hand an error to its caller, and is also subject to the
// strict check.
func GuardReads(t *testing.T, idx *index.Facade, p ReadPolicy) *ReadLog {
	t.Helper()
	allowed := make(map[string]bool, len(p.Allow))
	for _, a := range p.Allow {
		allowed[a] = true
	}
	for m := range p.Fail {
		switch m {
		case "SimilarityAdjacency", "ReverseDependentPaths", "TokenDF", "SubgraphEdges":
		default:
			t.Fatalf("guardReads: Fail names %q, which does not return an error to its caller; only SimilarityAdjacency, ReverseDependentPaths, TokenDF and SubgraphEdges can fail", m)
		}
	}
	log := &ReadLog{}
	s := OverridesOf(t, idx)
	s.ov.Reads = func(method string) error {
		log.Add(method)
		if !p.AllowAll && !allowed[method] {
			t.Errorf("unexpected read of %s", method)
		}
		if err, ok := p.Fail[method]; ok {
			return err
		}
		return nil
	}
	s.Apply()
	return log
}

// SeedResults writes one fact per search-result fixture.
func (e *GraphEnv) SeedResults(results []index.SearchResult) *GraphEnv {
	e.T.Helper()
	specs := make([]SeedFact, len(results))
	for i, r := range results {
		specs[i] = seedFromResult(r)
	}
	return e.Seed(specs...)
}

// Partition fixes the community Partition ScopedCluster will find: the
// clustering edge read returns a star within each community and nothing across.
// This is the one canned graph answer (see index.Overrides.SubgraphEdges): the
// SIMILAR_TO table, which a test seeds for cohesion, may hold edges ACROSS
// these communities.
func (e *GraphEnv) Partition(comms [][]string) *GraphEnv {
	OverridesOf(e.T, e.Idx()).SubgraphEdges(func(context.Context, []string) ([][2]string, error) {
		return intraCommunityEdges(comms), nil
	})
	return e
}

// SeedFacts writes already-built facts in ONE commit.
func (e *GraphEnv) SeedFacts(fs ...fact.Fact) *GraphEnv {
	e.T.Helper()
	files := make(map[string]string, len(fs))
	for _, f := range fs {
		content, err := fact.SerializeFact(f)
		require.NoError(e.T, err)
		files[f.Path()] = content
	}
	_, _, err := e.Svc.Facts().BatchWriteFacts(context.Background(), e.Branch, files, nil, "seed", "")
	require.NoError(e.T, err)
	return e
}

// intraCommunityEdges returns a star of SIMILAR_TO edges within each community
// (first member linked to the rest) and no cross-community edge, so Louvain
// recovers exactly the given partition. Singleton communities contribute no
// edge.
func intraCommunityEdges(comms [][]string) [][2]string {
	var edges [][2]string
	for _, c := range comms {
		for i := 1; i < len(c); i++ {
			edges = append(edges, [2]string{c[0], c[i]})
		}
	}
	return edges
}
