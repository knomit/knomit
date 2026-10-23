package web

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
	"knomit/internal/domain/modelparams"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/index"
)

// cosEmbedder is the embedder of the search tests, which run the REAL vector
// step of a real store. A query embeds to the unit vector e0. A document embeds
// to (c, sqrt(1-c^2), 0, ...) where c is the number written as `cos=<c>` in its
// title or body, so the cosine of a document with any query is exactly c, and
// the score the handler serves is c*100. A document with no marker embeds to
// e1 (cosine 0), which no cutoff lets through. Nothing here stubs a store read:
// the store runs its own KNN, cutoff, sort and diagnosis.
//
// It counts query inferences, remembers the text of the last one, and can be
// told to fail or to answer an empty vector.
type cosEmbedder struct {
	queries    atomic.Int64
	lastQuery  atomic.Value // string
	failQuery  atomic.Bool
	emptyQuery atomic.Bool
	// distinctQueries makes every EmbedQuery answer a DIFFERENT vector (a tiny
	// component 2 that grows with the call number, so the cosines barely move),
	// letting a test tell one shared inference from one per mount.
	distinctQueries atomic.Bool
}

const cosDim = 768

var (
	cosMarker        = regexp.MustCompile(`cos=([0-9.]+)`)
	errInferenceDown = errors.New("inference down")
)

func (e *cosEmbedder) doc(title, body string) []float32 {
	out := make([]float32, cosDim)
	m := cosMarker.FindStringSubmatch(title + "\n" + body)
	if m == nil {
		out[1] = 1
		return out
	}
	c, err := strconv.ParseFloat(m[1], 64)
	if err != nil || c < 0 || c > 1 {
		panic("cosEmbedder: bad cos marker " + m[0])
	}
	out[0] = float32(c)
	out[1] = float32(math.Sqrt(1 - c*c))
	return out
}

func (e *cosEmbedder) EmbedQuery(_ context.Context, text string) ([]float32, error) {
	n := e.queries.Add(1)
	e.lastQuery.Store(text)
	if e.failQuery.Load() {
		return nil, errInferenceDown
	}
	if e.emptyQuery.Load() {
		return []float32{}, nil
	}
	out := make([]float32, cosDim)
	out[0] = 1
	if e.distinctQueries.Load() {
		out[2] = float32(n) * 1e-3
	}
	return out, nil
}

func (e *cosEmbedder) EmbedDocument(_ context.Context, title, body string) ([]float32, error) {
	return e.doc(title, body), nil
}

func (e *cosEmbedder) EmbedDocuments(_ context.Context, titles, bodies []string) ([][]float32, error) {
	out := make([][]float32, len(titles))
	for i := range out {
		out[i] = e.doc(titles[i], bodies[i])
	}
	return out, nil
}

func (e *cosEmbedder) EmbedShortStrings(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, s := range texts {
		out[i] = e.doc(s, "")
	}
	return out, nil
}

func (*cosEmbedder) Dim() int                           { return cosDim }
func (*cosEmbedder) ID() string                         { return "cos-test" }
func (*cosEmbedder) Thresholds() modelparams.Thresholds { return modelparams.Defaults() }

// lastQueryText is the text of the most recent EmbedQuery ("" if none).
func (e *cosEmbedder) lastQueryText() string {
	s, _ := e.lastQuery.Load().(string)
	return s
}

// writeCosFact writes one observation to the agent branch of ri's real store.
// Its cosine with every query is cos (see cosEmbedder). mod, when set, adjusts
// the fact (kind, type, domain, motifs, ...) before it is serialized.
func writeCosFact(t *testing.T, ri *repos.RepoInstance, path, title string, cos float64, mod func(f *fact.Fact)) {
	t.Helper()
	writeFactOp(t, ri, path, title, cos, "", mod)
}

// factText is the serialized observation writeCosFact writes.
func factText(t *testing.T, path, title string, cos float64, mod func(f *fact.Fact)) string {
	t.Helper()
	f := fact.NewFact(path)
	f.Title = title
	f.Body = "cos=" + strconv.FormatFloat(cos, 'f', -1, 64)
	f.Kind = fact.Epistemic
	f.Type = fact.Observation
	f.Confidence = 0.9
	f.Sources = 1
	if mod != nil {
		mod(&f)
	}
	text, err := fact.SerializeFact(f)
	require.NoError(t, err)
	return text
}

// writeFactOp is writeCosFact whose commit carries the given operation
// ("learn", "update", ...), which the facts listing serves as `operation`.
func writeFactOp(t *testing.T, ri *repos.RepoInstance, path, title string, cos float64, op string, mod func(f *fact.Fact)) {
	t.Helper()
	text := factText(t, path, title, cos, mod)
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		require.NotNil(t, svc, "the test repo must have a live store")
		_, err := svc.Facts().WriteFact(context.Background(), ri.AgentBranch(), path, text, "add "+path, op)
		require.NoError(t, err)
	}))
}

// factSpec is one fact of writeFactsInOneCommit.
type factSpec struct {
	path, title string
	mod         func(f *fact.Fact)
	// cos is the fact's cosine with every query (see cosEmbedder); 0 means 0.9.
	cos float64
}

// plainFact is a factSpec at the default cosine.
func plainFact(path, title string, mod func(f *fact.Fact)) factSpec {
	return factSpec{path: path, title: title, mod: mod}
}

// writeFactsInOneCommit writes every spec to the agent branch of ri in ONE
// commit, so they share one committed_at and a recency listing orders them by
// path. A test that needs a total order across commits writes one commit per
// group and calls awaitNextSecond between them (the commit stamp has one-second
// resolution).
func writeFactsInOneCommit(t *testing.T, ri *repos.RepoInstance, specs ...factSpec) {
	t.Helper()
	files := map[string]string{}
	for _, s := range specs {
		cos := s.cos
		if cos == 0 {
			cos = 0.9
		}
		files[s.path] = factText(t, s.path, s.title, cos, s.mod)
	}
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		require.NotNil(t, svc, "the test repo must have a live store")
		_, _, err := svc.Facts().BatchWriteFacts(context.Background(), ri.AgentBranch(), files, nil, "add facts", "learn")
		require.NoError(t, err)
	}))
}

// awaitNextSecond returns once the wall clock is in a later second than it was
// on entry, so every commit written afterwards is stamped strictly after every
// commit written before the call.
func awaitNextSecond() {
	start := time.Now().Unix()
	for time.Now().Unix() == start {
		time.Sleep(20 * time.Millisecond)
	}
}

// searchLensEnv is a lens "eng" over real mounts that embed with emb, served by
// a Server whose embedder is emb too (as in production).
type searchLensEnv struct {
	m   *repos.Manager
	r   http.Handler
	emb *cosEmbedder
}

// searchLens creates the repos (write first, then reads), a lens "eng" binding
// them, and a router. providers carries whatever other provider a test still
// stubs (the motifs vocabulary); the zero value is all defaults.
func searchLens(t *testing.T, providers storeProviders, write string, reads ...string) searchLensEnv {
	t.Helper()
	emb := &cosEmbedder{}
	m, _ := newTestLensManagerWith(t, emb, append([]string{write}, reads...)...)
	r := (&Server{Manager: m, Embedder: emb, providers: providers}).NewAPIRouter()
	rs := make([]string, 0, len(reads))
	for _, n := range reads {
		rs = append(rs, `{"repo":"`+n+`"}`)
	}
	createLens(t, m, r, `{"name":"eng","write":"`+write+`","reads":[`+strings.Join(rs, ",")+`]}`)
	return searchLensEnv{m: m, r: r, emb: emb}
}

// get is GET url against the lens router.
func (e searchLensEnv) get(t *testing.T, url string) *httptest.ResponseRecorder {
	t.Helper()
	return getLensFacts(t, e.r, url)
}

// searchSeen is what the mounts' stores were asked to search.
type searchSeen struct {
	mu   sync.Mutex
	opts map[string][]index.SearchOptions
}

func (s *searchSeen) of(repo string) []index.SearchOptions {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]index.SearchOptions(nil), s.opts[repo]...)
}

// observeMountSearch records, per mount, every SearchOptions the mount's store
// is searched with, and then runs the real search. It is an observer: it calls
// next unchanged, so what the mount answers is the store's own answer. It fails
// the test when a mount already has an observer (see installMountOverrides).
func observeMountSearch(t testing.TB, m *repos.Manager, names ...string) *searchSeen {
	t.Helper()
	seen := &searchSeen{opts: map[string][]index.SearchOptions{}}
	for _, n := range names {
		n := n
		withMountStore(t, m, n, func(svc *store.Service) {
			installMountOverrides(t, svc, "observeMountSearch", index.Overrides{
				Search: func(next index.SearchFunc) index.SearchFunc {
					return func(ctx context.Context, branch string, q index.SearchOptions) ([]index.SearchResult, error) {
						seen.mu.Lock()
						seen.opts[n] = append(seen.opts[n], q)
						seen.mu.Unlock()
						return next(ctx, branch, q)
					}
				},
			})
		})
	}
	return seen
}
