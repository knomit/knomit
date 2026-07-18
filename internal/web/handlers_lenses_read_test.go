package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	knomitfact "knomit/internal/fact"
	"knomit/internal/federate"
	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
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

// ── lens union completions (GET /lenses/{lens}/completions) ───────────────────

// knownCompletionCategories mirrors the store's real category set (internal/
// store/index.go Completions): a known category returns values (possibly empty),
// and any other category is an error — exactly as the repo handler surfaces it.
var knownCompletionCategories = map[string]bool{
	"domain": true, "entity": true, "type": true,
	"kind": true, "origin": true, "ep": true, "path": true,
}

// lensCompletionsStub is a per-repo completionsProvider: each mount's
// Completions call is answered from byRepo keyed by repo name then category, so
// a single stub drives the whole fan-out. An unrecognised category returns the
// same error string the store raises, so the lens handler's error path matches
// the repo handler byte-for-byte.
type lensCompletionsStub struct {
	byRepo map[string]map[string][]string
}

func (s *lensCompletionsStub) Completions(
	ri *repos.RepoInstance, _, category, _ string, _ int,
) ([]string, error) {
	if !knownCompletionCategories[category] {
		return nil, fmt.Errorf("unknown completion category: %s", category)
	}
	return s.byRepo[ri.Name()][category], nil
}

// lensCompletionsBody mirrors the union completions wire shape.
type lensCompletionsBody struct {
	Values []string `json:"values"`
}

func decodeLensCompletions(t *testing.T, rec *httptest.ResponseRecorder) lensCompletionsBody {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	var body lensCompletionsBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	return body
}

// category=repo is lens-only: it lists the lens's mount NAMES in binding order —
// the write repo first, then the read mounts in Reads() order (sorted by name).
// The write repo's self-mount is de-duplicated, so each name appears once. Here
// write=zulu sorts LAST among the mounts, yet it leads because it is the write.
func TestLensCompletions_RepoCategoryListsMountsInOrder(t *testing.T) {
	m, _ := newTestLensManager(t, "zulu", "alpha", "beta")
	s := &Server{Manager: m, completionsProvider: &lensCompletionsStub{}}
	r := s.NewAPIRouter()
	createLens(t, r, `{"name":"eng","write":"zulu","reads":[{"repo":"alpha"},{"repo":"beta"}]}`)

	body := decodeLensCompletions(t, getLensFacts(t, r, "/lenses/eng/completions?category=repo"))
	want := []string{"zulu", "alpha", "beta"}
	if len(body.Values) != len(want) {
		t.Fatalf("values: got %v, want %v", body.Values, want)
	}
	for i := range want {
		if body.Values[i] != want[i] {
			t.Fatalf("order: got %v, want %v (write first, then reads in binding order)", body.Values, want)
		}
	}
}

// prefix= narrows category=repo, case-insensitively (an uppercase prefix still
// matches a lower-cased mount name), matching the store's LIKE behaviour.
func TestLensCompletions_RepoCategoryPrefixNarrows(t *testing.T) {
	m, _ := newTestLensManager(t, "zulu", "alpha", "beta")
	s := &Server{Manager: m, completionsProvider: &lensCompletionsStub{}}
	r := s.NewAPIRouter()
	createLens(t, r, `{"name":"eng","write":"zulu","reads":[{"repo":"alpha"},{"repo":"beta"}]}`)

	body := decodeLensCompletions(t, getLensFacts(t, r, "/lenses/eng/completions?category=repo&prefix=A"))
	want := []string{"alpha"}
	if len(body.Values) != len(want) || body.Values[0] != want[0] {
		t.Fatalf("values: got %v, want %v (case-insensitive prefix narrows)", body.Values, want)
	}
}

// category=domain unions each mount's values, de-duplicated across mounts,
// preserving per-mount first-seen order (mounts fan out in Reads() order).
func TestLensCompletions_DomainMergesWithoutDuplicates(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	stub := &lensCompletionsStub{byRepo: map[string]map[string][]string{
		"alpha": {"domain": {"ai", "alignment"}},
		"beta":  {"domain": {"ai", "robotics"}}, // "ai" collides with alpha's
	}}
	s := &Server{Manager: m, completionsProvider: stub}
	r := s.NewAPIRouter()
	createLens(t, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	body := decodeLensCompletions(t, getLensFacts(t, r, "/lenses/eng/completions?category=domain"))
	want := []string{"ai", "alignment", "robotics"}
	if len(body.Values) != len(want) {
		t.Fatalf("values: got %v, want %v (union, deduped)", body.Values, want)
	}
	for i := range want {
		if body.Values[i] != want[i] {
			t.Fatalf("order: got %v, want %v", body.Values, want)
		}
	}
}

// An unknown category is the SAME error the repo handler gives: the store-shaped
// error flows through writeStoreError → 500 problem+json.
func TestLensCompletions_UnknownCategory500(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	s := &Server{Manager: m, completionsProvider: &lensCompletionsStub{}}
	r := s.NewAPIRouter()
	createLens(t, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	rec := getLensFacts(t, r, "/lenses/eng/completions?category=bogus")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: got %q, want application/problem+json", got)
	}
}

// An empty union (a known category with no values on any mount) yields
// values:[], never null.
func TestLensCompletions_EmptyUnion(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	s := &Server{Manager: m, completionsProvider: &lensCompletionsStub{}}
	r := s.NewAPIRouter()
	createLens(t, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	rec := getLensFacts(t, r, "/lenses/eng/completions?category=domain")
	body := decodeLensCompletions(t, rec)
	if body.Values == nil {
		t.Error("values must be [] not null")
	}
}

// An unknown lens is 404 (from LensMiddleware, before the handler runs).
func TestLensCompletions_UnknownLens404(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha")
	s := &Server{Manager: m, completionsProvider: &lensCompletionsStub{}}
	r := s.NewAPIRouter()

	rec := getLensFacts(t, r, "/lenses/missing/completions?category=repo")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// ---- Single fact read through a lens (GET /lenses/{lens}/facts/{path...}) ----

// lensFactReaderStub is a per-repo FactReader: Read is keyed by (repo name,
// repo-relative path), so one stub drives the whole binding. A read that finds
// no fact returns errFactNotFound (the sentinel the handler maps to 404); a
// preset err short-circuits every read (exercises the 500 path).
type lensFactReaderStub struct {
	byRepo map[string]map[string]knomitfact.Fact
	head   string
	err    error
}

func (s *lensFactReaderStub) Read(ri *repos.RepoInstance, _ hal.Anchor, path string, _ bool) (knomitfact.Fact, string, error) {
	if s.err != nil {
		return knomitfact.Fact{}, "", s.err
	}
	f, ok := s.byRepo[ri.Name()][path]
	if !ok {
		return knomitfact.Fact{}, "", errFactNotFound
	}
	return f, s.head, nil
}

func (s *lensFactReaderStub) Exists(_ *repos.RepoInstance, _ string, _, _ string) bool { return true }

// lensFactViewBody mirrors the single-fact wire shape: the repo FactView body
// (path/title/body/as_of/_links) plus the lens-level source block.
type lensFactViewBody struct {
	Path   string      `json:"path"`
	Title  string      `json:"title"`
	Body   string      `json:"body"`
	AsOf   AsOf        `json:"as_of"`
	Links  hal.LinkMap `json:"_links"`
	Source struct {
		Repo   string `json:"repo"`
		ID     string `json:"id"`
		Branch string `json:"branch"`
	} `json:"source"`
}

func mkFact(path, title string) knomitfact.Fact {
	f := knomitfact.NewFact(path)
	f.Title = title
	f.Type = knomitfact.Type("observation")
	return f
}

// A bare kb/... path reads from the WRITE repo — no dedupe scan — even when a
// read mount shadows the SAME repo-relative path with different content. The
// write repo's body wins and the source names the write repo.
func TestLensFact_BarePathReadsWriteRepo(t *testing.T) {
	m, _ := newTestLensManager(t, "zulu", "alpha") // write=zulu sorts after read=alpha
	reader := &lensFactReaderStub{
		head: "deadbeef",
		byRepo: map[string]map[string]knomitfact.Fact{
			"zulu":  {"kb/x/1.md": mkFact("kb/x/1.md", "Write copy")},
			"alpha": {"kb/x/1.md": mkFact("kb/x/1.md", "Read copy")},
		},
	}
	s := &Server{Manager: m, factReader: reader}
	r := s.NewAPIRouter()
	createLens(t, r, `{"name":"eng","write":"zulu","reads":[{"repo":"alpha"}]}`)

	rec := getLensFacts(t, r, "/lenses/eng/facts/kb/x/1.md")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	var body lensFactViewBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if body.Path != "kb/x/1.md" {
		t.Errorf("path: got %q, want bare kb/x/1.md", body.Path)
	}
	if body.Title != "Write copy" {
		t.Errorf("title: got %q, want the WRITE repo's copy", body.Title)
	}
	zulu := m.Get("zulu")
	if body.Source.Repo != "zulu" || body.Source.ID != federate.ID12(zulu.ID()) || body.Source.Branch != zulu.AgentBranch() {
		t.Errorf("source: got %+v, want {zulu %s %s}", body.Source, federate.ID12(zulu.ID()), zulu.AgentBranch())
	}
	if _, ok := body.Links["self"]; !ok {
		t.Errorf("missing _links.self; links=%+v", body.Links)
	}
}

// A kb://<id12>/kb/... path (URL-encoded by the client) resolves to that mount,
// returns its fact body, and carries that mount's source identity.
func TestLensFact_QualifiedPathHitsMount(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	reader := &lensFactReaderStub{
		head: "cafe1234",
		byRepo: map[string]map[string]knomitfact.Fact{
			"beta": {"kb/y/2.md": mkFact("kb/y/2.md", "Read only")},
		},
	}
	s := &Server{Manager: m, factReader: reader}
	r := s.NewAPIRouter()
	createLens(t, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	beta := m.Get("beta")
	id := federate.ID12(beta.ID())
	rawURL := "/lenses/eng/facts/" + url.PathEscape("kb://"+id+"/kb/y/2.md")

	rec := getLensFacts(t, r, rawURL)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	var body lensFactViewBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if body.Title != "Read only" {
		t.Errorf("title: got %q, want Read only", body.Title)
	}
	if body.Source.Repo != "beta" || body.Source.ID != id || body.Source.Branch != beta.AgentBranch() {
		t.Errorf("source: got %+v, want {beta %s %s}", body.Source, id, beta.AgentBranch())
	}
}

// A kb://<id12>/… whose id12 is not mounted in the lens returns 404 with the
// SAME body as a genuinely-missing fact — mount topology must not leak (a
// caller cannot tell "unknown mount" from "no such fact").
func TestLensFact_UnknownID404MatchesNotFound(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	reader := &lensFactReaderStub{head: "cafe1234"} // no facts anywhere
	s := &Server{Manager: m, factReader: reader}
	r := s.NewAPIRouter()
	createLens(t, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	beta := m.Get("beta")
	knownID := federate.ID12(beta.ID())
	unknownID := "0123456789ab" // 12 hex, not a mounted repo id

	// Missing fact at a KNOWN mount.
	recKnown := getLensFacts(t, r, "/lenses/eng/facts/"+url.PathEscape("kb://"+knownID+"/kb/y/2.md"))
	// Unknown mount id (same relative path).
	recUnknown := getLensFacts(t, r, "/lenses/eng/facts/"+url.PathEscape("kb://"+unknownID+"/kb/y/2.md"))

	if recKnown.Code != http.StatusNotFound {
		t.Fatalf("known-missing status: got %d, want 404; body=%s", recKnown.Code, recKnown.Body.String())
	}
	if recUnknown.Code != http.StatusNotFound {
		t.Fatalf("unknown-id status: got %d, want 404; body=%s", recUnknown.Code, recUnknown.Body.String())
	}
	if got := recUnknown.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: got %q, want application/problem+json", got)
	}
	// The two 404 bodies address different requested paths, so they will differ
	// in the echoed path — assert the TITLE is identical so an unknown id is
	// indistinguishable in KIND from a missing fact (no "not mounted" tell).
	var kb, ub struct {
		Title  string `json:"title"`
		Detail string `json:"detail"`
	}
	_ = json.Unmarshal(recKnown.Body.Bytes(), &kb)
	_ = json.Unmarshal(recUnknown.Body.Bytes(), &ub)
	if kb.Title != "Fact not found" || ub.Title != "Fact not found" {
		t.Errorf("titles: known=%q unknown=%q, want both %q", kb.Title, ub.Title, "Fact not found")
	}
}

// A retracted/missing fact on the write repo is a 404 (parity with the repo
// single-fact handler), and a real backend error is a 500 (not masked as 404).
func TestLensFact_MissingAndBackendError(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	s := &Server{Manager: m, factReader: &lensFactReaderStub{}} // no facts
	r := s.NewAPIRouter()
	createLens(t, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	rec := getLensFacts(t, r, "/lenses/eng/facts/kb/gone.md")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}

	mErr, _ := newTestLensManager(t, "alpha", "beta")
	sErr := &Server{Manager: mErr, factReader: &lensFactReaderStub{err: fmt.Errorf("disk on fire")}}
	rErr := sErr.NewAPIRouter()
	createLens(t, rErr, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)
	recErr := getLensFacts(t, rErr, "/lenses/eng/facts/kb/x/1.md")
	if recErr.Code != http.StatusInternalServerError {
		t.Fatalf("backend-error status: got %d, want 500; body=%s", recErr.Code, recErr.Body.String())
	}
}
