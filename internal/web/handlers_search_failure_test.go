package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"knomit/internal/embeddings/params"
	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// failEmb is a 768-dim embedder whose EmbedQuery can be switched to fail or to
// return an empty vector with a nil error, and which counts query inferences.
type failEmb struct {
	mode  *atomic.Int32 // 0 ok, 1 error, 2 empty vector with nil error
	calls *atomic.Int64
}

func newFailEmb(mode int32) failEmb {
	var m atomic.Int32
	m.Store(mode)
	return failEmb{mode: &m, calls: &atomic.Int64{}}
}

func (e failEmb) vec() []float32 {
	out := make([]float32, 768)
	out[0] = 1
	return out
}

func (e failEmb) EmbedQuery(context.Context, string) ([]float32, error) {
	e.calls.Add(1)
	switch e.mode.Load() {
	case 1:
		return nil, errors.New("inference down")
	case 2:
		return []float32{}, nil
	}
	return e.vec(), nil
}

func (e failEmb) EmbedDocument(context.Context, string, string) ([]float32, error) {
	return e.vec(), nil
}

func (e failEmb) EmbedDocuments(_ context.Context, titles, _ []string) ([][]float32, error) {
	out := make([][]float32, len(titles))
	for i := range out {
		out[i] = e.vec()
	}
	return out, nil
}

func (e failEmb) EmbedShortStrings(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = e.vec()
	}
	return out, nil
}

func (failEmb) Dim() int                      { return 768 }
func (failEmb) ID() string                    { return "fail-emb" }
func (failEmb) Thresholds() params.Thresholds { return params.Defaults() }

// realRepo is a repo instance over a REAL store wired to emb.
func realRepo(t *testing.T, emb failEmb) (*repos.RepoInstance, *store.Service) {
	t.Helper()
	svc, err := store.Open(filepath.Join(t.TempDir(), "k.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	svc.SetEmbedder(emb)
	if err := svc.InitRepo(context.Background(), map[string]string{}, "agent/test"); err != nil {
		t.Fatal(err)
	}
	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "alpha", UID: "alpha-uid", AgentBranch: "agent/test", Svc: svc,
		Ontology: fact.CodeOntology(), OntologyRoot: "kb", Embedder: emb,
	})
	return ri, svc
}

// storeBackedProvider answers /search from a real store, so the handler's
// behaviour on a store failure is what is under test (not a stub's).
type storeBackedProvider struct{ svc *store.Service }

func (p storeBackedProvider) Search(ctx context.Context, _ *repos.RepoInstance, _ store.Embedder, _ string, q store.SearchOptions) ([]store.SearchResult, error) {
	return p.svc.FactQuery().Search(ctx, "agent/test", q)
}

// A text search whose query cannot be embedded is a 500, never a 200 with an
// empty list. Driven through a real store: the failure happens inside
// store.Search, which used to turn it into nil, nil.
func TestHandleSearch_EmbedderFailureIsServerError(t *testing.T) {
	for name, mode := range map[string]int32{"error": 1, "empty vector": 2} {
		t.Run(name, func(t *testing.T) {
			_, svc := realRepo(t, newFailEmb(mode))
			s := &Server{Manager: newTestManagerWithRepos(t, "alpha"), providers: storeProviders{search: storeBackedProvider{svc}}}
			rec := httptest.NewRecorder()
			s.NewAPIRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
				"/repos/alpha/branches/agent:test/search?q=x", nil))
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status: %d, want 500; body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

// The production provider hands the embedder's failure back as an error. Before,
// it logged and let the store re-embed (and the store then swallowed it).
func TestDefaultSearchProvider_EmbedErrorPropagates(t *testing.T) {
	emb := newFailEmb(1)
	ri, _ := realRepo(t, emb)
	res, err := defaultSearchProvider{}.Search(context.Background(), ri, emb, "agent/test", store.SearchOptions{Text: "x"})
	if err == nil {
		t.Fatalf("want the embedder's error, got results=%v err=nil", res)
	}
	if !strings.Contains(err.Error(), "inference down") {
		t.Errorf("error should carry the cause, got %v", err)
	}
	if got := emb.calls.Load(); got != 1 {
		t.Errorf("EmbedQuery called %d times, want 1 (no store-side retry of a failed embed)", got)
	}
}

// The lens fan-outs fail ONCE, up front, when the shared query vector cannot be
// computed: one inference (not one per mount), a 500, and no empty 200.
func TestLensSearchAndFacts_EmbedFailureFailsOnceUpFront(t *testing.T) {
	for name, mode := range map[string]int32{"error": 1, "empty vector": 2} {
		for _, path := range []string{"/lenses/eng/search?q=x", "/lenses/eng/facts?q=x"} {
			t.Run(name+" "+path, func(t *testing.T) {
				m, _ := newTestLensManager(t, "alpha", "beta", "gamma")
				emb := newFailEmb(mode)
				s := &Server{Manager: m, Embedder: emb, providers: storeProviders{
					search: &lensSearchStub{}, factsCollection: &lensFactsStub{}}}
				r := s.NewAPIRouter()
				createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"},{"repo":"gamma"}]}`)

				rec := getLensFacts(t, r, path)
				if rec.Code != http.StatusInternalServerError {
					t.Fatalf("status: %d, want 500; body=%s", rec.Code, rec.Body.String())
				}
				if got := emb.calls.Load(); got != 1 {
					t.Errorf("EmbedQuery called %d times across 3 mounts, want 1", got)
				}
			})
		}
	}
}

// diagSearchProvider / diagFactsProvider report a fixed vector-step result
// through SearchOptions.Diag and return no rows — a mount whose text query came
// back empty.
type diagSearchProvider struct{ d store.SearchDiag }

func (p diagSearchProvider) Search(_ context.Context, _ *repos.RepoInstance, _ store.Embedder, _ string, q store.SearchOptions) ([]store.SearchResult, error) {
	if q.Diag != nil {
		*q.Diag = p.d
	}
	return nil, nil
}

type diagFactsProvider struct{ d store.SearchDiag }

func (p diagFactsProvider) RecentFacts(_ context.Context, _ *repos.RepoInstance, _ string, q store.SearchOptions) ([]store.RecentFactEntry, int, error) {
	if q.Diag != nil {
		*q.Diag = p.d
	}
	return nil, 0, nil
}

// On both lens text reads, an empty result caused by the caller's cutoff carries
// the notice (cutoff, best cosine); an empty result with another cause, or no
// caller cutoff, does not.
func TestLensSearchAndFacts_CutoffNotice(t *testing.T) {
	emptied := store.SearchDiag{Text: true, VecHits: 9, BestCosine: 0.54, Cutoff: 0.7}
	keptThenFiltered := store.SearchDiag{Text: true, VecHits: 9, BestCosine: 0.54, Cutoff: 0.3, Candidates: 4}

	for _, ep := range []string{"search", "facts"} {
		get := func(d store.SearchDiag, query string) map[string]any {
			m, _ := newTestLensManager(t, "alpha", "beta")
			s := &Server{Manager: m, providers: storeProviders{
				search: diagSearchProvider{d}, factsCollection: diagFactsProvider{d}}}
			r := s.NewAPIRouter()
			createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)
			rec := getLensFacts(t, r, "/lenses/eng/"+ep+"?q=x"+query)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: status %d body=%s", ep, rec.Code, rec.Body.String())
			}
			var out map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			return out
		}

		notice, _ := get(emptied, "&min_similarity=0.7")["notice"].(string)
		if !strings.Contains(notice, "0.70") || !strings.Contains(notice, "0.54") {
			t.Errorf("%s: notice should carry cutoff and best cosine, got %q", ep, notice)
		}
		if _, ok := get(emptied, "")["notice"]; ok {
			t.Errorf("%s: no caller cutoff, so no notice", ep)
		}
		if _, ok := get(keptThenFiltered, "&min_similarity=0.3")["notice"]; ok {
			t.Errorf("%s: the cutoff kept candidates (a later filter emptied it), so no notice", ep)
		}
	}
}
