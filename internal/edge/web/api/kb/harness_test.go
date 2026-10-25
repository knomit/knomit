package kb

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/corpus/repos/control"
	"knomit/internal/domain/fact"
	"knomit/internal/domain/modelparams"
	"knomit/internal/edge/web/hal"
	"knomit/internal/edge/web/httpx"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/model"
	"knomit/internal/foundation/config"
	"knomit/internal/testsupport/testsigner"
)

// Fixtures the package-private tests here share. They are COPIES of the
// helpers of the same name in internal/edge/web's tests (a test file cannot
// be imported, and web's full-router tests keep theirs): a change to one does
// not reach the other.

// newTestRouter is what this package's HTTP tests drive instead of web's
// Server: the routes Mount registers, at the paths and behind the {repo},
// {branch} and {lens} resolution web's router gives them (fleet gate and
// RepoMiddleware, httpx.BranchMiddleware, LensMiddleware). No auth edge, write
// gate or lifecycle route runs: web's router tests cover those, and these test
// the handlers.
func newTestRouter(m *repos.Manager) http.Handler {
	r := chi.NewRouter()
	repo := r.Route("/repos/{repo}", func(chi.Router) {})
	repo.Use(repoFromURL(m))
	branch := repo.Route("/branches/{branch}", func(chi.Router) {})
	branch.Use(httpx.BranchMiddleware)
	lens := r.Route("/lenses/{lens}", func(chi.Router) {})
	lens.Use(lensFromURL(m))
	Mount(branch, lens, "kb", nil)
	return r
}

// repoFromURL is the part of web's RepoMiddleware these handlers depend on:
// the {repo} instance in the request context, 404 when there is none.
func repoFromURL(m *repos.Manager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ri := m.Get(chi.URLParam(r, "repo"))
			if ri == nil {
				hal.WriteProblem(w, http.StatusNotFound, "Repo not found", "no repo", r.URL.Path)
				return
			}
			next.ServeHTTP(w, r.WithContext(repos.WithRepoInstance(r.Context(), ri)))
		})
	}
}

// lensFromURL is the part of web's LensMiddleware these handlers depend on:
// the {lens} binding in the request context, 404 when it does not resolve.
func lensFromURL(m *repos.Manager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, err := repos.ResolveLensBinding(r.Context(), m, chi.URLParam(r, "lens"))
			if err != nil {
				hal.WriteProblem(w, http.StatusNotFound, "Lens not found", err.Error(), r.URL.Path)
				return
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// createLens registers a lens named name over the repos named write (its
// write member) and reads, through Manager.CreateLens: what web's POST
// /lenses does, without the route (api/manage owns it).
func createLens(t *testing.T, m *repos.Manager, name, write string, reads ...string) {
	t.Helper()
	uid := func(repo string) string {
		ri := m.Get(repo)
		require.NotNil(t, ri, "no repo %q", repo)
		return ri.UID()
	}
	lens := control.Lens{Name: name, WriteUID: uid(write)}
	for _, rd := range reads {
		lens.Reads = append(lens.Reads, control.LensRead{RepoUID: uid(rd)})
	}
	_, err := m.CreateLens(context.Background(), lens)
	require.NoError(t, err)
}

// createRepo creates the preset repo name through Manager.Create, which
// returns once the repo is serving and indexed: what web's createViaAPI (a
// POST /repos and a poll of its create job) waits for.
func createRepo(t *testing.T, m *repos.Manager, name string) {
	t.Helper()
	_, err := m.Create(context.Background(), repos.CreateSpec{Name: name, Mode: "preset", OntologyPreset: "default"}, nil)
	require.NoError(t, err)
}

// newTestManagerWithRepos builds a repos.Manager and registers empty
// RepoInstance stubs under the given names.
func newTestManagerWithRepos(t *testing.T, names ...string) *repos.Manager {
	t.Helper()
	m := repos.New(context.Background(), repos.Deps{})
	for _, name := range names {
		m.Set(name, repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
			Name: name, AgentBranch: "agent/test",
		}))
	}
	return m
}

// newTestLensManager builds a started repos.Manager (so Registry() is live)
// and provisions the named preset repos. It returns the manager and its Home
// dir.
func newTestLensManager(t *testing.T, names ...string) (*repos.Manager, string) {
	t.Helper()
	home := t.TempDir()
	m := repos.New(context.Background(), repos.Deps{
		Cfg:     config.Config{Home: home},
		AgentID: testsigner.FixtureAgentID("machine/test"), AgentBranch: "machine/test",
		Machine: repos.Options{Synchronous: true},
	})
	if err := m.Start(); err != nil {
		t.Fatalf("manager start: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	for _, name := range names {
		if _, err := m.Create(context.Background(), repos.CreateSpec{
			Name: name, Mode: "preset", OntologyPreset: "default",
		}, nil); err != nil {
			t.Fatalf("create repo %q: %v", name, err)
		}
	}
	return m, home
}

// newRealManagerInHome is a started Manager over home with the agent branch
// "machine/test" and no repos.
func newRealManagerInHome(t *testing.T, home string) *repos.Manager {
	t.Helper()
	m := repos.New(context.Background(), repos.Deps{
		Cfg:     config.Config{Home: home},
		AgentID: testsigner.FixtureAgentID("machine/test"), AgentBranch: "machine/test",
	})
	if err := m.Start(); err != nil {
		t.Fatalf("start manager: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// newRealStoreManager is a Manager whose repos each hold a REAL store (agent
// branch "agent/test", the branch newTestManagerWithRepos names). It returns
// the manager and the stores by repo name.
func newRealStoreManager(t *testing.T, names ...string) (*repos.Manager, map[string]*store.Service) {
	t.Helper()
	return newRealStoreManagerWith(t, nil, names...)
}

// newRealStoreManagerWith is newRealStoreManager whose stores and repo
// instances embed with emb (nil: no embedder).
func newRealStoreManagerWith(t *testing.T, emb model.BatchEmbedder, names ...string) (*repos.Manager, map[string]*store.Service) {
	t.Helper()
	m := repos.New(context.Background(), repos.Deps{})
	svcs := map[string]*store.Service{}
	for _, name := range names {
		svc, err := store.Open(filepath.Join(t.TempDir(), name+".db"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = svc.Close() })
		cfg := repos.TestInstanceConfig{Name: name, AgentBranch: "agent/test", Svc: svc}
		if emb != nil {
			svc.SetEmbedder(emb)
			cfg.Embedder = emb
		}
		require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, "agent/test"))
		m.Set(name, repos.NewTestInstanceWithDeps(cfg))
		svcs[name] = svc
	}
	return m, svcs
}

// urlBranch spells a branch name as a URL segment ("/" becomes ":").
func urlBranch(b string) string { return strings.ReplaceAll(b, "/", ":") }

// unescape turns the JSON-escaped newlines of testFactContent into real ones.
func unescape(s string) string { return strings.ReplaceAll(s, `\n`, "\n") }

// testFactContent is valid fact markdown: frontmatter + # heading body.
const testFactContent = "---\\ntype: observation\\ndomain: [ai]\\nconfidence: 0.9\\nsources: 1\\n---\\n\\n# Test Fact\\n\\nBody text.\\n"

// writeProblem decodes a write handler's problem document.
func writeProblem(t *testing.T, rec *httptest.ResponseRecorder) hal.Problem {
	t.Helper()
	var p hal.Problem
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p), rec.Body.String())
	return p
}

// The branches of the fact-sub fixtures.
const (
	fsAgentBranch = "agent/test"
	fsOtherBranch = "agent/elsewhere"
	fsOtherURL    = "agent:elsewhere"
)

// cosEmbedder is the embedder of the search tests, which run the REAL vector
// step of a real store. A query embeds to the unit vector e0. A document embeds
// to (c, sqrt(1-c^2), 0, ...) where c is the number written as `cos=<c>` in its
// title or body, so the cosine of a document with any query is exactly c. A
// document with no marker embeds to e1 (cosine 0).
//
// It counts query inferences and can be told to fail or to answer an empty
// vector.
type cosEmbedder struct {
	queries    atomic.Int64
	failQuery  atomic.Bool
	emptyQuery atomic.Bool
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
	e.queries.Add(1)
	if e.failQuery.Load() {
		return nil, errInferenceDown
	}
	if e.emptyQuery.Load() {
		return []float32{}, nil
	}
	out := make([]float32, cosDim)
	out[0] = 1
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

// factText is a serialized observation whose cosine with every query is cos
// (see cosEmbedder). mod, when set, adjusts the fact before it is serialized.
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
