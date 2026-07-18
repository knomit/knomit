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

// ── lens union search (GET /lenses/{lens}/search) ────────────────────────────

// lensSearchStub is a per-repo searchProvider: each mount's Search call is
// answered from byRepo keyed by the repo's name, so a single stub drives the
// whole fan-out. It records the last SearchOptions and whether a non-nil
// embedder was forwarded, per repo.
type lensSearchStub struct {
	byRepo   map[string][]store.SearchResult
	lastOpts map[string]store.SearchOptions
	embSeen  map[string]bool
}

func (s *lensSearchStub) Search(
	ri *repos.RepoInstance, emb store.Embedder, _ string, opts store.SearchOptions,
) ([]store.SearchResult, error) {
	if s.lastOpts == nil {
		s.lastOpts = map[string]store.SearchOptions{}
	}
	if s.embSeen == nil {
		s.embSeen = map[string]bool{}
	}
	s.lastOpts[ri.Name()] = opts
	s.embSeen[ri.Name()] = emb != nil
	return s.byRepo[ri.Name()], nil
}

// sr builds a minimal search result: a repo-relative path, title, and native
// relevance score. The score magnitude is what a naive interleave would sort
// by; RRF ignores it and orders by per-mount rank instead.
func sr(path, title string, score float64) store.SearchResult {
	return store.SearchResult{
		FactWithBody: store.FactWithBody{
			FactRecord: store.FactRecord{Path: path, Title: title, Type: "observation"},
		},
		Score: score,
	}
}

// lensSearchBody mirrors the lens union search wire shape.
type lensSearchBody struct {
	Results []struct {
		Path       string   `json:"path"`
		Title      string   `json:"title"`
		Score      float64  `json:"score"`
		Kind       string   `json:"kind"`
		Type       string   `json:"type"`
		Domain     []string `json:"domain"`
		Entities   []string `json:"entities"`
		Confidence float64  `json:"confidence"`
		Source     struct {
			Repo   string `json:"repo"`
			ID     string `json:"id"`
			Branch string `json:"branch"`
		} `json:"source"`
	} `json:"results"`
	Total int `json:"total"`
}

func decodeLensSearch(t *testing.T, rec *httptest.ResponseRecorder) lensSearchBody {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	var body lensSearchBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	return body
}

// A duplicate relative path across the write and a read mount collapses to a
// single row: the WRITE repo's copy, with a bare (unqualified) path — even when
// the read mount's copy scores HIGHER (so it ranks first under fusion). The
// shadowed higher-ranked copy must not appear at all.
func TestLensSearch_DuplicatePathWriteWins(t *testing.T) {
	m, _ := newTestLensManager(t, "zulu", "alpha") // write=zulu sorts after read=alpha
	stub := &lensSearchStub{byRepo: map[string][]store.SearchResult{
		"zulu":  {sr("kb/x/1.md", "Write copy", 10)},
		"alpha": {sr("kb/x/1.md", "Read copy", 99)}, // higher score, would rank first
	}}
	s := &Server{Manager: m, searchProvider: stub}
	r := s.NewAPIRouter()
	createLens(t, r, `{"name":"eng","write":"zulu","reads":[{"repo":"alpha"}]}`)

	body := decodeLensSearch(t, getLensFacts(t, r, "/lenses/eng/search?q=x"))
	if len(body.Results) != 1 {
		t.Fatalf("results: got %d, want 1 (deduped); body=%+v", len(body.Results), body)
	}
	if body.Total != 1 {
		t.Errorf("total: got %d, want 1", body.Total)
	}
	got := body.Results[0]
	if got.Path != "kb/x/1.md" {
		t.Errorf("path: got %q, want bare kb/x/1.md", got.Path)
	}
	if got.Title != "Write copy" {
		t.Errorf("title: got %q, want the WRITE repo's copy (read copy is shadowed even though it ranks higher)", got.Title)
	}
	if got.Source.Repo != "zulu" {
		t.Errorf("source.repo: got %q, want zulu (write)", got.Source.Repo)
	}
}

// The fused order is reciprocal-rank fusion, NOT a naive interleave by native
// score. Constructed so the two orderings disagree. Mounts fan out in Reads()
// order — sorted by repo name — so alpha is mount 0, beta is mount 1. alpha's
// only fact ranks #0 in its mount with a LOW native score (5); beta has a
// high-score #0 (100) and a mid-score #1 (50). RRF interleaves by rank —
// alpha#0, beta#0, beta#1 — so alpha's low-score fact LEADS. A naive score sort
// would order beta-top(100), beta-second(50), alpha-top(5), putting alpha LAST.
// This test fails under naive interleaving.
func TestLensSearch_RRFNotNaiveInterleave(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	stub := &lensSearchStub{byRepo: map[string][]store.SearchResult{
		// Reads() is sorted by repo name → alpha is mount 0, beta is mount 1.
		"alpha": {sr("kb/a/0.md", "alpha-top", 5)}, // rank 0, LOW score
		"beta": {
			sr("kb/b/0.md", "beta-top", 100),
			sr("kb/b/1.md", "beta-second", 50),
		},
	}}
	s := &Server{Manager: m, searchProvider: stub}
	r := s.NewAPIRouter()
	createLens(t, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	body := decodeLensSearch(t, getLensFacts(t, r, "/lenses/eng/search?q=x"))
	got := make([]string, len(body.Results))
	for i, res := range body.Results {
		got[i] = res.Title
	}
	want := []string{"alpha-top", "beta-top", "beta-second"}
	if len(got) != len(want) {
		t.Fatalf("count: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order: got %v, want %v (RRF by rank, not naive score interleave)", got, want)
		}
	}
}

// A fact that lives only on a read mount is qualified with kb://<id12>/… and
// carries that mount's source identity.
func TestLensSearch_ReadMountQualifiedPath(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	stub := &lensSearchStub{byRepo: map[string][]store.SearchResult{
		"beta": {sr("kb/y/2.md", "Read only", 42)},
	}}
	s := &Server{Manager: m, searchProvider: stub}
	r := s.NewAPIRouter()
	createLens(t, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	beta := m.Get("beta")
	wantID := federate.ID12(beta.ID())
	wantPath := "kb://" + wantID + "/kb/y/2.md"

	body := decodeLensSearch(t, getLensFacts(t, r, "/lenses/eng/search?q=x"))
	if len(body.Results) != 1 {
		t.Fatalf("results: got %d, want 1; body=%+v", len(body.Results), body)
	}
	got := body.Results[0]
	if got.Path != wantPath {
		t.Errorf("path: got %q, want %q", got.Path, wantPath)
	}
	if got.Source.Repo != "beta" || got.Source.ID != wantID || got.Source.Branch != beta.AgentBranch() {
		t.Errorf("source: got %+v, want {beta %s %s}", got.Source, wantID, beta.AgentBranch())
	}
}

// repo=<name> narrows the fan-out to the named mount(s); other mounts' results
// do not appear.
func TestLensSearch_RepoFilterNarrows(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	stub := &lensSearchStub{byRepo: map[string][]store.SearchResult{
		"alpha": {sr("kb/a/1.md", "Write fact", 100)},
		"beta":  {sr("kb/b/2.md", "Read fact", 10)},
	}}
	s := &Server{Manager: m, searchProvider: stub}
	r := s.NewAPIRouter()
	createLens(t, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	body := decodeLensSearch(t, getLensFacts(t, r, "/lenses/eng/search?q=x&repo=beta"))
	if len(body.Results) != 1 {
		t.Fatalf("results: got %d, want 1; body=%+v", len(body.Results), body)
	}
	if body.Results[0].Source.Repo != "beta" {
		t.Errorf("source.repo: got %q, want beta only", body.Results[0].Source.Repo)
	}
}

// An unknown repo= name is a well-formed request naming a nonexistent mount → 422.
func TestLensSearch_UnknownRepoFilter422(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	s := &Server{Manager: m, searchProvider: &lensSearchStub{}}
	r := s.NewAPIRouter()
	createLens(t, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	rec := getLensFacts(t, r, "/lenses/eng/search?q=x&repo=ghost")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: got %q, want application/problem+json", got)
	}
}

// With embeddings disabled (Server.Embedder nil), the endpoint still serves
// results — it forwards the nil embedder to the provider exactly as the repo
// /search handler does (keyword fallback, no query vector), never 500s.
func TestLensSearch_EmbedderOffFallback(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	stub := &lensSearchStub{byRepo: map[string][]store.SearchResult{
		"beta": {sr("kb/b/2.md", "Read fact", 10)},
	}}
	s := &Server{Manager: m, searchProvider: stub} // Embedder left nil
	r := s.NewAPIRouter()
	createLens(t, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	body := decodeLensSearch(t, getLensFacts(t, r, "/lenses/eng/search?q=x"))
	if len(body.Results) != 1 {
		t.Fatalf("results: got %d, want 1; body=%+v", len(body.Results), body)
	}
	if stub.embSeen["beta"] {
		t.Error("expected nil embedder forwarded to provider (embeddings disabled), but a non-nil one was seen")
	}
}

// An empty union (no mount returns results) yields results:[] and total:0, never null.
func TestLensSearch_EmptyMounts(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	s := &Server{Manager: m, searchProvider: &lensSearchStub{}}
	r := s.NewAPIRouter()
	createLens(t, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	rec := getLensFacts(t, r, "/lenses/eng/search?q=x")
	body := decodeLensSearch(t, rec)
	if body.Total != 0 {
		t.Errorf("total: got %d, want 0", body.Total)
	}
	if body.Results == nil {
		t.Error("results must be [] not null")
	}
}

// An unknown lens is 404 (from LensMiddleware, before the handler runs).
func TestLensSearch_UnknownLens404(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha")
	s := &Server{Manager: m, searchProvider: &lensSearchStub{}}
	r := s.NewAPIRouter()

	rec := getLensFacts(t, r, "/lenses/missing/search?q=x")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}
