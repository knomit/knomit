package synthesize

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/domain/fact"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/index"
	"knomit/internal/engine/store/storetest"
)

// graphEnv is a real store opened for a test that drives the search handle
// (*index.Facade) over a seeded corpus: real facts, so TokenDF counts and
// DERIVED_FROM edges come from the index's own tables, and SIMILAR_TO edges
// written through the index's graph test seams, so SimilarityAdjacency and
// SubgraphEdges read them back through the real queries.
//
// Nothing here replaces a read. A test that needs a read to FAIL, or to prove
// that a code path made no read at all, uses guardReads.
type graphEnv struct {
	t      *testing.T
	svc    *store.Service
	branch string
	// log is the reads seen under the current policy.
	log *readLog
}

func newGraphEnv(t *testing.T, branch string) *graphEnv {
	t.Helper()
	svc, err := store.Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, branch))
	e := &graphEnv{t: t, svc: svc, branch: branch}
	// Strict by default: the handle fails the test on ANY read of the index
	// until the test widens the policy with allow / allowAll (or guardReads).
	e.log = guardReads(t, svc.Search(), readPolicy{})
	return e
}

// allow widens the strict default to the named reads and returns the env.
func (e *graphEnv) allow(methods ...string) *graphEnv {
	e.log = guardReads(e.t, e.idx(), readPolicy{Allow: methods})
	return e
}

// allowAll turns the strict default off.
func (e *graphEnv) allowAll() *graphEnv {
	e.log = guardReads(e.t, e.idx(), readPolicy{AllowAll: true})
	return e
}

// idx is the store's own search handle.
func (e *graphEnv) idx() *index.Facade { return e.svc.Search() }

// seedFact is one fact to write.
type seedFact struct {
	path     string
	domains  []string
	entities []string
	motifs   []string
	refs     []string
	// Zero values mean: observation, authored, confidence 0.8, one source.
	typ        fact.Type
	origin     fact.Origin
	confidence float64
	sources    int
}

// seedFromResult is the fact a search-result fixture stands for.
func seedFromResult(r index.SearchResult) seedFact {
	return seedFact{
		path: r.Path, domains: r.Domain, entities: r.Entities, motifs: r.Motifs,
		typ: fact.Type(r.Type), origin: fact.Origin(r.Origin),
		confidence: r.Confidence, sources: r.Sources,
	}
}

// seedFromLLM is the fact a bridge seed stands for.
func seedFromLLM(f factForLLM) seedFact {
	s := seedFact{path: f.File, domains: f.Domain, entities: f.Entities, motifs: f.Motifs, origin: fact.Origin(f.Origin)}
	if s.origin == fact.Discovered {
		s.typ = fact.Synthesis // the only types a discovered fact may have
	}
	return s
}

// seed writes the facts in ONE commit.
func (e *graphEnv) seed(specs ...seedFact) *graphEnv {
	e.t.Helper()
	files := make(map[string]string, len(specs))
	for _, s := range specs {
		f := fact.NewFact(s.path)
		f.Title = s.path
		f.Body = "body of " + s.path
		f.Type, f.Origin, f.Confidence, f.Sources = fact.Observation, fact.Authored, 0.8, 1
		if s.typ != "" {
			f.Type = s.typ
		}
		if s.origin != "" {
			f.Origin = s.origin
		}
		if s.confidence != 0 {
			f.Confidence = s.confidence
		}
		if s.sources != 0 {
			f.Sources = s.sources
		}
		f.Domain = s.domains
		f.Entities = s.entities
		f.Motifs = s.motifs
		f.Refs = s.refs
		content, err := fact.SerializeFact(f)
		require.NoError(e.t, err)
		files[f.Path()] = content
	}
	_, _, err := e.svc.Facts().BatchWriteFacts(context.Background(), e.branch, files, nil, "seed", "")
	require.NoError(e.t, err)
	return e
}

// seedAll writes one fact per seed, carrying its domains, entities and motifs.
func (e *graphEnv) seedAll(seeds []factForLLM) *graphEnv {
	e.t.Helper()
	specs := make([]seedFact, len(seeds))
	for i, s := range seeds {
		specs[i] = seedFromLLM(s)
	}
	return e.seed(specs...)
}

// nodeIDs maps every seeded path to its fact node in the graph.
func (e *graphEnv) nodeIDs() map[string]int64 {
	e.t.Helper()
	rows, err := storetest.RawDB(e.svc).Query(`
		SELECT p.value, p.node_id FROM node_props_text p
		JOIN property_keys k ON k.id = p.key_id AND k.key = 'path'`)
	require.NoError(e.t, err)
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var path string
		var id int64
		require.NoError(e.t, rows.Scan(&path, &id))
		out[path] = id
	}
	require.NoError(e.t, rows.Err())
	return out
}

// similar writes a SIMILAR_TO edge for each pair, between the pair's seeded
// facts (the graph test seams, not a replaced read).
func (e *graphEnv) similar(pairs ...[2]string) *graphEnv {
	e.t.Helper()
	ids := e.nodeIDs()
	db := storetest.RawDB(e.svc)
	for _, p := range pairs {
		a, okA := ids[p[0]]
		b, okB := ids[p[1]]
		require.True(e.t, okA && okB, "similar(%v): both facts must be seeded first", p)
		require.NoError(e.t, index.GraphMergeEdgeForTest(context.Background(), db, a, b, index.EdgeSimilarTo))
	}
	return e
}

// similarAll writes SIMILAR_TO between every pair of paths.
func (e *graphEnv) similarAll(paths []string) *graphEnv {
	e.t.Helper()
	var pairs [][2]string
	for i := 0; i < len(paths); i++ {
		for j := i + 1; j < len(paths); j++ {
			pairs = append(pairs, [2]string{paths[i], paths[j]})
		}
	}
	return e.similar(pairs...)
}

// ovSet composes the test-only overrides of one store. index.Overrides is
// installed as a whole, so every helper that needs one goes through the ovSet of
// its handle and the pieces accumulate instead of replacing each other.
type ovSet struct {
	t       *testing.T
	idx     *index.Facade
	ov      index.Overrides
	restore func()
}

var ovSets sync.Map // *index.Facade -> *ovSet

// overridesOf returns the ovSet of idx, creating it on first use. The
// overrides are removed when the test ends.
func overridesOf(t *testing.T, idx *index.Facade) *ovSet {
	t.Helper()
	if s, ok := ovSets.Load(idx); ok {
		return s.(*ovSet)
	}
	s := &ovSet{t: t, idx: idx}
	ovSets.Store(idx, s)
	t.Cleanup(func() {
		if s.restore != nil {
			s.restore()
		}
		ovSets.Delete(idx)
	})
	return s
}

func (s *ovSet) apply() {
	if s.restore != nil {
		s.restore()
	}
	s.restore = s.idx.SetOverridesForTest(s.ov)
}

// search installs the Search wrapper.
func (s *ovSet) search(w func(next index.SearchFunc) index.SearchFunc) {
	s.ov.Search = w
	s.apply()
}

// subgraphEdges installs the clustering edge read.
func (s *ovSet) subgraphEdges(f func(ctx context.Context, paths []string) ([][2]string, error)) {
	s.ov.SubgraphEdges = f
	s.apply()
}

// readPolicy says which reads of the search handle a test allows.
type readPolicy struct {
	// Allow names the reads that may happen. Any other read fails the test.
	Allow []string
	// AllowAll turns the strictness off (for a test that only injects Fail).
	AllowAll bool
	// Fail makes the named reads return the given error. The four reads that
	// return an error (SimilarityAdjacency, ReverseDependentPaths, TokenDF,
	// SubgraphEdges) hand it to the caller.
	Fail map[string]error
}

// readLog is the reads guardReads saw.
type readLog struct {
	mu    sync.Mutex
	names []string
}

func (l *readLog) add(n string) {
	l.mu.Lock()
	l.names = append(l.names, n)
	l.mu.Unlock()
}

// Count is the number of reads seen, allowed or not.
func (l *readLog) Count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.names)
}

// CountOf is the number of reads of one method.
func (l *readLog) CountOf(method string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, m := range l.names {
		if m == method {
			n++
		}
	}
	return n
}

// guardReads installs a read policy on the handle for the rest of the test.
// The policy is store-wide (see index.Overrides): every read of every query
// service of that store passes through it. It is strict by default: a read the
// policy does not allow fails the test, which restores the "any other call
// fails" property the gomock fakes had for every method. A Fail entry must name
// a read that can hand an error to its caller, and is also subject to the
// strict check.
func guardReads(t *testing.T, idx *index.Facade, p readPolicy) *readLog {
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
	log := &readLog{}
	s := overridesOf(t, idx)
	s.ov.Reads = func(method string) error {
		log.add(method)
		if !p.AllowAll && !allowed[method] {
			t.Errorf("unexpected read of %s", method)
		}
		if err, ok := p.Fail[method]; ok {
			return err
		}
		return nil
	}
	s.apply()
	return log
}

// seedResults writes one fact per search-result fixture.
func (e *graphEnv) seedResults(results []index.SearchResult) *graphEnv {
	e.t.Helper()
	specs := make([]seedFact, len(results))
	for i, r := range results {
		specs[i] = seedFromResult(r)
	}
	return e.seed(specs...)
}

// partition fixes the community partition ScopedCluster will find: the
// clustering edge read returns a star within each community and nothing across.
// This is the one canned graph answer (see index.Overrides.SubgraphEdges): the
// SIMILAR_TO table, which a test seeds for cohesion, may hold edges ACROSS
// these communities.
func (e *graphEnv) partition(comms [][]string) *graphEnv {
	overridesOf(e.t, e.idx()).subgraphEdges(func(context.Context, []string) ([][2]string, error) {
		return intraCommunityEdges(comms), nil
	})
	return e
}

// seedFacts writes already-built facts in ONE commit.
func (e *graphEnv) seedFacts(fs ...fact.Fact) *graphEnv {
	e.t.Helper()
	files := make(map[string]string, len(fs))
	for _, f := range fs {
		content, err := fact.SerializeFact(f)
		require.NoError(e.t, err)
		files[f.Path()] = content
	}
	_, _, err := e.svc.Facts().BatchWriteFacts(context.Background(), e.branch, files, nil, "seed", "")
	require.NoError(e.t, err)
	return e
}
