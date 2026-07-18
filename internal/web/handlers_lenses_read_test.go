package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"knomit/internal/federate"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// lensFactsStub is a per-repo factsCollectionProvider: each mount's RecentFacts
// call is answered from byRepo keyed by the repo's name, so a single stub drives
// the whole fan-out. It records the last SearchOptions seen per repo.
type lensFactsStub struct {
	byRepo   map[string][]store.RecentFactEntry
	lastOpts map[string]store.SearchOptions
}

func (s *lensFactsStub) RecentFacts(
	ri *repos.RepoInstance, _ string, opts store.SearchOptions,
) ([]store.RecentFactEntry, int, error) {
	if s.lastOpts == nil {
		s.lastOpts = map[string]store.SearchOptions{}
	}
	s.lastOpts[ri.Name()] = opts
	e := s.byRepo[ri.Name()]
	return e, len(e), nil
}

// lensFactsBody mirrors the union facts collection wire shape.
type lensFactsBody struct {
	Facts []struct {
		Path        string  `json:"path"`
		Title       string  `json:"title"`
		Kind        string  `json:"kind"`
		Type        string  `json:"type"`
		CommittedAt int64   `json:"committed_at"`
		Operation   string  `json:"operation"`
		Score       float64 `json:"score"`
		Source      struct {
			Repo   string `json:"repo"`
			ID     string `json:"id"`
			Branch string `json:"branch"`
		} `json:"source"`
	} `json:"facts"`
	Total int `json:"total"`
}

func createLens(t *testing.T, r http.Handler, body string) {
	t.Helper()
	rec := postLens(t, r, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create lens: got %d, body=%s", rec.Code, rec.Body.String())
	}
}

func getLensFacts(t *testing.T, r http.Handler, url string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	return rec
}

func decodeLensFacts(t *testing.T, rec *httptest.ResponseRecorder) lensFactsBody {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	var body lensFactsBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	return body
}

// A duplicate relative path across the write and a read mount collapses to a
// single row: the WRITE repo's copy, with a bare (unqualified) path — even when
// the write repo's name sorts AFTER the read mount's in binding order.
func TestLensFacts_DuplicatePathWriteWins(t *testing.T) {
	m, _ := newTestLensManager(t, "zulu", "alpha") // write=zulu sorts after read=alpha
	stub := &lensFactsStub{byRepo: map[string][]store.RecentFactEntry{
		"zulu":  {{Path: "kb/x/1.md", Title: "Write copy", Type: "observation", CommittedAt: 100}},
		"alpha": {{Path: "kb/x/1.md", Title: "Read copy", Type: "observation", CommittedAt: 200}},
	}}
	s := &Server{Manager: m, factsCollectionProvider: stub}
	r := s.NewAPIRouter()
	createLens(t, r, `{"name":"eng","write":"zulu","reads":[{"repo":"alpha"}]}`)

	body := decodeLensFacts(t, getLensFacts(t, r, "/lenses/eng/facts"))
	if len(body.Facts) != 1 {
		t.Fatalf("facts: got %d, want 1 (deduped); body=%+v", len(body.Facts), body)
	}
	if body.Total != 1 {
		t.Errorf("total: got %d, want 1", body.Total)
	}
	f := body.Facts[0]
	if f.Path != "kb/x/1.md" {
		t.Errorf("path: got %q, want bare kb/x/1.md", f.Path)
	}
	if f.Title != "Write copy" {
		t.Errorf("title: got %q, want the WRITE repo's copy", f.Title)
	}
	if f.Source.Repo != "zulu" {
		t.Errorf("source.repo: got %q, want zulu (write)", f.Source.Repo)
	}
}

// A fact that lives only on a read mount is qualified with kb://<id12>/… and
// carries that mount's source identity.
func TestLensFacts_ReadMountQualifiedPath(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	stub := &lensFactsStub{byRepo: map[string][]store.RecentFactEntry{
		"beta": {{Path: "kb/y/2.md", Title: "Read only", Type: "policy", CommittedAt: 300}},
	}}
	s := &Server{Manager: m, factsCollectionProvider: stub}
	r := s.NewAPIRouter()
	createLens(t, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	beta := m.Get("beta")
	wantID := federate.ID12(beta.ID())
	wantPath := "kb://" + wantID + "/kb/y/2.md"

	body := decodeLensFacts(t, getLensFacts(t, r, "/lenses/eng/facts"))
	if len(body.Facts) != 1 {
		t.Fatalf("facts: got %d, want 1; body=%+v", len(body.Facts), body)
	}
	f := body.Facts[0]
	if f.Path != wantPath {
		t.Errorf("path: got %q, want %q", f.Path, wantPath)
	}
	if f.Source.Repo != "beta" || f.Source.ID != wantID || f.Source.Branch != beta.AgentBranch() {
		t.Errorf("source: got %+v, want {beta %s %s}", f.Source, wantID, beta.AgentBranch())
	}
}

// repo=<name> narrows the fan-out to the named mount(s); other mounts' facts do
// not appear.
func TestLensFacts_RepoFilterNarrows(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	stub := &lensFactsStub{byRepo: map[string][]store.RecentFactEntry{
		"alpha": {{Path: "kb/a/1.md", Title: "Write fact", CommittedAt: 100}},
		"beta":  {{Path: "kb/b/2.md", Title: "Read fact", CommittedAt: 200}},
	}}
	s := &Server{Manager: m, factsCollectionProvider: stub}
	r := s.NewAPIRouter()
	createLens(t, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	body := decodeLensFacts(t, getLensFacts(t, r, "/lenses/eng/facts?repo=beta"))
	if len(body.Facts) != 1 {
		t.Fatalf("facts: got %d, want 1; body=%+v", len(body.Facts), body)
	}
	if body.Facts[0].Source.Repo != "beta" {
		t.Errorf("source.repo: got %q, want beta only", body.Facts[0].Source.Repo)
	}
}

// An unknown repo= name is a well-formed request naming a nonexistent mount → 422.
func TestLensFacts_UnknownRepoFilter422(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	s := &Server{Manager: m, factsCollectionProvider: &lensFactsStub{}}
	r := s.NewAPIRouter()
	createLens(t, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	rec := getLensFacts(t, r, "/lenses/eng/facts?repo=ghost")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: got %q, want application/problem+json", got)
	}
}

// Recency ordering is by committed_at ACROSS mounts, not grouped by mount.
func TestLensFacts_RecencyOrderingAcrossMounts(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	stub := &lensFactsStub{byRepo: map[string][]store.RecentFactEntry{
		// Each mount's list is committed_at-DESC, as RecentFacts returns.
		"alpha": {
			{Path: "kb/a/hi.md", Title: "a-300", CommittedAt: 300},
			{Path: "kb/a/lo.md", Title: "a-100", CommittedAt: 100},
		},
		"beta": {
			{Path: "kb/b/hi.md", Title: "b-400", CommittedAt: 400},
			{Path: "kb/b/lo.md", Title: "b-200", CommittedAt: 200},
		},
	}}
	s := &Server{Manager: m, factsCollectionProvider: stub}
	r := s.NewAPIRouter()
	createLens(t, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	body := decodeLensFacts(t, getLensFacts(t, r, "/lenses/eng/facts"))
	got := make([]string, len(body.Facts))
	for i, f := range body.Facts {
		got[i] = f.Title
	}
	want := []string{"b-400", "a-300", "b-200", "a-100"}
	if len(got) != len(want) {
		t.Fatalf("count: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order: got %v, want %v (interleaved by timestamp, not mount-grouped)", got, want)
		}
	}
}

// An unknown lens is 404 (from LensMiddleware, before the handler runs).
func TestLensFacts_UnknownLens404(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha")
	s := &Server{Manager: m, factsCollectionProvider: &lensFactsStub{}}
	r := s.NewAPIRouter()

	rec := getLensFacts(t, r, "/lenses/missing/facts")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// An empty union (no mount returns facts) yields facts:[] and total:0, never null.
func TestLensFacts_EmptyMounts(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	s := &Server{Manager: m, factsCollectionProvider: &lensFactsStub{}}
	r := s.NewAPIRouter()
	createLens(t, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	rec := getLensFacts(t, r, "/lenses/eng/facts")
	body := decodeLensFacts(t, rec)
	if body.Total != 0 {
		t.Errorf("total: got %d, want 0", body.Total)
	}
	if body.Facts == nil {
		t.Error("facts must be [] not null")
	}
}
