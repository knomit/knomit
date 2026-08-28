package web

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
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
	// totalByRepo overrides the count a mount reports, so a test can model the
	// real store: RecentFacts runs its own SELECT COUNT(*), so the total is
	// independent of how many rows the page asked for. Defaults to len(rows).
	totalByRepo map[string]int
}

func (s *lensFactsStub) RecentFacts(
	_ context.Context,
	ri *repos.RepoInstance, _ string, opts store.SearchOptions,
) ([]store.RecentFactEntry, int, error) {
	if s.lastOpts == nil {
		s.lastOpts = map[string]store.SearchOptions{}
	}
	s.lastOpts[ri.Name()] = opts
	e := s.byRepo[ri.Name()]
	// The count is a separate SELECT COUNT(*) in the real store, so it is taken
	// BEFORE the limit truncates the page — that independence is the whole
	// point of these tests.
	total := len(e)
	if n, ok := s.totalByRepo[ri.Name()]; ok {
		total = n
	}
	// Model the store's own limit so a depth-bounded fan-out truncates here,
	// exactly as a real mount would.
	if opts.Limit > 0 && len(e) > opts.Limit {
		e = e[:opts.Limit]
	}
	return e, total, nil
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

func createLens(t *testing.T, m *repos.Manager, r http.Handler, body string) {
	t.Helper()
	rec := postLens(t, m, r, body)
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
	s := &Server{Manager: m, providers: storeProviders{factsCollection: stub}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"zulu","reads":[{"repo":"alpha"}]}`)

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
	s := &Server{Manager: m, providers: storeProviders{factsCollection: stub}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

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
	s := &Server{Manager: m, providers: storeProviders{factsCollection: stub}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

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
	s := &Server{Manager: m, providers: storeProviders{factsCollection: &lensFactsStub{}}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

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
	s := &Server{Manager: m, providers: storeProviders{factsCollection: stub}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

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

// Every selecting filter (not just path/text) is forwarded to each mount's
// RecentFacts, so a lens answers a filtered browse exactly like the repos it
// federates. Regression for the bug where the handler dropped all filters
// except path and text.
func TestLensFacts_ForwardsFullFilterSet(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	stub := &lensFactsStub{byRepo: map[string][]store.RecentFactEntry{}}
	s := &Server{Manager: m, providers: storeProviders{factsCollection: stub}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	url := "/lenses/eng/facts?query=needle&domain=store,mcp&entities=Foo,Bar" +
		"&type=observation&exclude_type=hypothesis&kind=epistemic&exclude_kind=pragmatic" +
		"&origin=authored&ep=learn&domain_exact=true&min_confidence=0.4&min_similarity=0.2"
	if rec := getLensFacts(t, r, url); rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}

	for _, repo := range []string{"alpha", "beta"} {
		opts, ok := stub.lastOpts[repo]
		if !ok {
			t.Fatalf("mount %q was never queried", repo)
		}
		if opts.Text != "needle" {
			t.Errorf("%s Text: got %q, want needle", repo, opts.Text)
		}
		if got := fmt.Sprint(opts.Domain); got != "[store mcp]" {
			t.Errorf("%s Domain: got %v, want [store mcp]", repo, opts.Domain)
		}
		if got := fmt.Sprint(opts.Entities); got != "[Foo Bar]" {
			t.Errorf("%s Entities: got %v, want [Foo Bar]", repo, opts.Entities)
		}
		if got := fmt.Sprint(opts.IncludeTypes); got != "[observation]" {
			t.Errorf("%s IncludeTypes: got %v", repo, opts.IncludeTypes)
		}
		if got := fmt.Sprint(opts.ExcludeTypes); got != "[hypothesis]" {
			t.Errorf("%s ExcludeTypes: got %v", repo, opts.ExcludeTypes)
		}
		if got := fmt.Sprint(opts.IncludeKinds); got != "[epistemic]" {
			t.Errorf("%s IncludeKinds: got %v", repo, opts.IncludeKinds)
		}
		if got := fmt.Sprint(opts.ExcludeKinds); got != "[pragmatic]" {
			t.Errorf("%s ExcludeKinds: got %v", repo, opts.ExcludeKinds)
		}
		if got := fmt.Sprint(opts.IncludeOrigins); got != "[authored]" {
			t.Errorf("%s IncludeOrigins: got %v", repo, opts.IncludeOrigins)
		}
		if got := fmt.Sprint(opts.EpisodeOps); got != "[learn]" {
			t.Errorf("%s EpisodeOps: got %v", repo, opts.EpisodeOps)
		}
		if !opts.DomainExact {
			t.Errorf("%s DomainExact: got false, want true", repo)
		}
		if opts.MinConfidence != 0.4 {
			t.Errorf("%s MinConfidence: got %v, want 0.4", repo, opts.MinConfidence)
		}
		if opts.MinSimilarity != 0.2 {
			t.Errorf("%s MinSimilarity: got %v, want 0.2", repo, opts.MinSimilarity)
		}
	}
	// Path is still resolved per-mount (repo-relative), not overwritten by the
	// shared filter set.
	if p := stub.lastOpts["alpha"].Path; p != "" {
		t.Errorf("alpha Path: got %q, want empty (unqualified browse applies per mount)", p)
	}
}

// `entity` (singular) is the canonical filter name the HAL template advertises;
// `entities` (plural) is the back-compat alias. Both are forwarded and their CSV
// values merge (entity first), exactly as the repo facts collection does.
// Regression for the bug where the lens handler read only the plural alias, so a
// caller sending the canonical `entity=` got the unfiltered union silently.
func TestLensFacts_ForwardsCanonicalEntitySingular(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	stub := &lensFactsStub{byRepo: map[string][]store.RecentFactEntry{}}
	s := &Server{Manager: m, providers: storeProviders{factsCollection: stub}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	// Canonical singular alone, plus a merge of singular + plural.
	if rec := getLensFacts(t, r, "/lenses/eng/facts?entity=Foo&entities=Bar"); rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	for _, repo := range []string{"alpha", "beta"} {
		opts, ok := stub.lastOpts[repo]
		if !ok {
			t.Fatalf("mount %q was never queried", repo)
		}
		if got := fmt.Sprint(opts.Entities); got != "[Foo Bar]" {
			t.Errorf("%s Entities: got %v, want [Foo Bar] (entity singular merged with entities plural)", repo, opts.Entities)
		}
	}
}

// `topic` is shorthand for an ontology-root subdirectory filter: `?topic=X`
// rewrites to `path=kb/X/`, and an explicit `?path=` always wins. Mirrors the
// repo facts collection. Regression for the bug where the lens handler dropped
// `topic`, so a topic-scoped browse returned the full unscoped union.
func TestLensFacts_TopicShorthandRewritesPath(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	stub := &lensFactsStub{byRepo: map[string][]store.RecentFactEntry{}}
	s := &Server{Manager: m, providers: storeProviders{factsCollection: stub}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	// ?topic=technology → per-mount Path=kb/technology/ (the general ontology
	// preset the test repos use carries `technology`, so no mount's ontology-aware
	// topic-skip fires).
	if rec := getLensFacts(t, r, "/lenses/eng/facts?topic=technology"); rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	for _, repo := range []string{"alpha", "beta"} {
		opts, ok := stub.lastOpts[repo]
		if !ok {
			t.Fatalf("mount %q was never queried for ?topic=technology", repo)
		}
		if opts.Path != "kb/technology/" {
			t.Errorf("%s Path: got %q, want kb/technology/ (topic rewritten to path)", repo, opts.Path)
		}
	}

	// An explicit ?path= overrides ?topic= (both topics live in the preset, so
	// the skip does not confound the override check).
	stub.lastOpts = nil
	if rec := getLensFacts(t, r, "/lenses/eng/facts?topic=technology&path=kb/science/"); rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	if opts, ok := stub.lastOpts["alpha"]; !ok || opts.Path != "kb/science/" {
		t.Errorf("explicit path must win over topic: got %q (ok=%v), want kb/science/", opts.Path, ok)
	}
}

// A malformed numeric filter is a 400, matching the repo/lens search handlers,
// rather than being silently coerced to zero.
func TestLensFacts_InvalidNumericFilters400(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha")
	s := &Server{Manager: m, providers: storeProviders{factsCollection: &lensFactsStub{}}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[]}`)

	for _, q := range []string{"min_confidence=notanumber", "min_similarity=huh"} {
		rec := getLensFacts(t, r, "/lenses/eng/facts?"+q)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400; body=%s", q, rec.Code, rec.Body.String())
		}
	}
}

// A text query orders the union by relevance (per-mount rank fused with RRF),
// NOT by committed_at. Lens-of-one identity: with a single mount, RRF's N=1
// identity preserves the mount's relevance order byte-for-byte, matching the
// repo /facts endpoint. Regression for the bug where every text query was
// re-sorted by recency via MergeRecent, burying the best match and breaking
// lens-of-one parity.
func TestLensFacts_TextQueryPreservesRelevanceOrder(t *testing.T) {
	m, _ := newTestLensManager(t, "solo")
	// RecentFacts with a text query returns RELEVANCE order (store behaviour):
	// best match first, regardless of committed_at. Timestamps here are NOT
	// monotonic, so a recency re-sort would reorder them.
	stub := &lensFactsStub{byRepo: map[string][]store.RecentFactEntry{
		"solo": {
			{Path: "kb/x/best.md", Title: "best", CommittedAt: 100},
			{Path: "kb/x/mid.md", Title: "mid", CommittedAt: 300},
			{Path: "kb/x/worst.md", Title: "worst", CommittedAt: 200},
		},
	}}
	s := &Server{Manager: m, providers: storeProviders{factsCollection: stub}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"solo","reads":[]}`)

	body := decodeLensFacts(t, getLensFacts(t, r, "/lenses/eng/facts?query=needle"))
	got := make([]string, len(body.Facts))
	for i, f := range body.Facts {
		got[i] = f.Title
	}
	want := []string{"best", "mid", "worst"} // relevance order, NOT 300/200/100 recency
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("text-query order: got %v, want %v (relevance, not recency)", got, want)
	}
}

// Across mounts, a text query fuses by per-mount RANK (RRF), interleaving
// rank-0 rows first regardless of their timestamps — not a global recency sort.
func TestLensFacts_TextQueryFusesByRankNotTimestamp(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	stub := &lensFactsStub{byRepo: map[string][]store.RecentFactEntry{
		// Per-mount relevance order. beta's best match is OLDER than alpha's
		// second-best; a recency sort would put a-old-but-rank1 above b-rank0.
		"alpha": {
			{Path: "kb/a/0.md", Title: "a0", CommittedAt: 500},
			{Path: "kb/a/1.md", Title: "a1", CommittedAt: 400},
		},
		"beta": {
			{Path: "kb/b/0.md", Title: "b0", CommittedAt: 100},
			{Path: "kb/b/1.md", Title: "b1", CommittedAt: 50},
		},
	}}
	s := &Server{Manager: m, providers: storeProviders{factsCollection: stub}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	body := decodeLensFacts(t, getLensFacts(t, r, "/lenses/eng/facts?query=needle"))
	got := make([]string, len(body.Facts))
	for i, f := range body.Facts {
		got[i] = f.Title
	}
	// RRF: rank-0 rows first (a0, b0 — tie broken by mount order), then rank-1
	// (a1, b1). A recency sort would have produced a0,a1,b0,b1.
	want := []string{"a0", "b0", "a1", "b1"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("RRF fusion order: got %v, want %v (rank-interleaved, not timestamp-sorted)", got, want)
	}
}

// An unknown lens is 404 (from LensMiddleware, before the handler runs).
func TestLensFacts_UnknownLens404(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha")
	s := &Server{Manager: m, providers: storeProviders{factsCollection: &lensFactsStub{}}}
	r := s.NewAPIRouter()

	rec := getLensFacts(t, r, "/lenses/missing/facts")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// An empty union (no mount returns facts) yields facts:[] and total:0, never null.
func TestLensFacts_EmptyMounts(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	s := &Server{Manager: m, providers: storeProviders{factsCollection: &lensFactsStub{}}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

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
	_ context.Context,
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
	s := &Server{Manager: m, providers: storeProviders{search: stub}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"zulu","reads":[{"repo":"alpha"}]}`)

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
	s := &Server{Manager: m, providers: storeProviders{search: stub}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

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
	s := &Server{Manager: m, providers: storeProviders{search: stub}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

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
	s := &Server{Manager: m, providers: storeProviders{search: stub}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

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
	s := &Server{Manager: m, providers: storeProviders{search: &lensSearchStub{}}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

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
	s := &Server{Manager: m, providers: storeProviders{search: stub}} // Embedder left nil
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

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
	s := &Server{Manager: m, providers: storeProviders{search: &lensSearchStub{}}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

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
	s := &Server{Manager: m, providers: storeProviders{search: &lensSearchStub{}}}
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
	_ context.Context,
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
	s := &Server{Manager: m, providers: storeProviders{completions: &lensCompletionsStub{}}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"zulu","reads":[{"repo":"alpha"},{"repo":"beta"}]}`)

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
	s := &Server{Manager: m, providers: storeProviders{completions: &lensCompletionsStub{}}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"zulu","reads":[{"repo":"alpha"},{"repo":"beta"}]}`)

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
	s := &Server{Manager: m, providers: storeProviders{completions: stub}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

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
	s := &Server{Manager: m, providers: storeProviders{completions: &lensCompletionsStub{}}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

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
	s := &Server{Manager: m, providers: storeProviders{completions: &lensCompletionsStub{}}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	rec := getLensFacts(t, r, "/lenses/eng/completions?category=domain")
	body := decodeLensCompletions(t, rec)
	if body.Values == nil {
		t.Error("values must be [] not null")
	}
}

// An unknown lens is 404 (from LensMiddleware, before the handler runs).
func TestLensCompletions_UnknownLens404(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha")
	s := &Server{Manager: m, providers: storeProviders{completions: &lensCompletionsStub{}}}
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
	reads  map[string]int // per-repo Read call count
}

func (s *lensFactReaderStub) Read(_ context.Context, ri *repos.RepoInstance, _ hal.Anchor, path string, _ bool) (knomitfact.Fact, string, error) {
	if s.reads == nil {
		s.reads = map[string]int{}
	}
	s.reads[ri.Name()]++
	if s.err != nil {
		return knomitfact.Fact{}, "", s.err
	}
	f, ok := s.byRepo[ri.Name()][path]
	if !ok {
		return knomitfact.Fact{}, "", errFactNotFound
	}
	return f, s.head, nil
}

func (s *lensFactReaderStub) Exists(_ context.Context, _ *repos.RepoInstance, _ string, _, _ string) bool {
	return true
}

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
	s := &Server{Manager: m, providers: storeProviders{factReader: reader}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"zulu","reads":[{"repo":"alpha"}]}`)

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
	// No dedupe scan on a bare path: the read mount is NEVER queried — only the
	// write repo is read. Locks in "bare means write repo, period" behaviorally.
	if got := reader.reads["alpha"]; got != 0 {
		t.Errorf("read mount queried %d times on a bare-path request, want 0", got)
	}
	if got := reader.reads["zulu"]; got != 1 {
		t.Errorf("write repo read %d times, want exactly 1", got)
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
	s := &Server{Manager: m, providers: storeProviders{factReader: reader}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

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
	// The top-level path echoes the canonical QUALIFIED wire form for a read
	// mount, so a client can round-trip it into another lens request and land on
	// the same fact (not silently on the write repo).
	wantPath := "kb://" + id + "/kb/y/2.md"
	if body.Path != wantPath {
		t.Errorf("path: got %q, want qualified %q", body.Path, wantPath)
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
	s := &Server{Manager: m, providers: storeProviders{factReader: reader}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

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
	s := &Server{Manager: m, providers: storeProviders{factReader: &lensFactReaderStub{}}} // no facts
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	rec := getLensFacts(t, r, "/lenses/eng/facts/kb/gone.md")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}

	mErr, _ := newTestLensManager(t, "alpha", "beta")
	sErr := &Server{Manager: mErr, providers: storeProviders{factReader: &lensFactReaderStub{err: fmt.Errorf("disk on fire")}}}
	rErr := sErr.NewAPIRouter()
	createLens(t, mErr, rErr, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)
	recErr := getLensFacts(t, rErr, "/lenses/eng/facts/kb/x/1.md")
	if recErr.Code != http.StatusInternalServerError {
		t.Fatalf("backend-error status: got %d, want 500; body=%s", recErr.Code, recErr.Body.String())
	}
}

// ── Counts are not page sizes ────────────────────────────────────────────────
//
// The union total used to be len(merged): the size of the materialised
// candidate set, not the number of facts. With a fixed 500-row per-mount fetch
// a lens over a 1403-fact mount reported 500, and the dashboard — which sums
// each mount's real total — reported 1403 for the same corpus on the same
// screen. Two numbers, one truth, and the smaller one was in the browser.
//
// The store has always answered both questions separately: RecentFacts runs its
// own SELECT COUNT(*) and returns it alongside the page. The repo endpoint uses
// it. The lens handler discarded it with `entries, _, err`.

func TestLensFacts_TotalIsTheCorpusCountNotThePage(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	stub := &lensFactsStub{
		byRepo: map[string][]store.RecentFactEntry{
			"alpha": {{Path: "kb/a/1.md", Title: "a1", CommittedAt: 300}},
			"beta":  {{Path: "kb/b/1.md", Title: "b1", CommittedAt: 200}},
		},
		// Each mount holds far more than it returned on this page.
		totalByRepo: map[string]int{"alpha": 1403, "beta": 234},
	}
	s := &Server{Manager: m, providers: storeProviders{factsCollection: stub}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	body := decodeLensFacts(t, getLensFacts(t, r, "/lenses/eng/facts?limit=1"))
	if body.Total != 1637 {
		t.Fatalf("total: got %d, want 1637 (1403+234) — the count must not be the page size", body.Total)
	}
	if len(body.Facts) > 1 {
		t.Fatalf("page: got %d rows, want at most 1 — limit still bounds the transfer", len(body.Facts))
	}
}

func TestLensFacts_NarrowedTotalCountsOnlyTheSelectedMounts(t *testing.T) {
	// The count has to answer for the CURRENT query, or narrowing sources would
	// leave a total describing a set the reader is no longer looking at.
	m, _ := newTestLensManager(t, "alpha", "beta")
	stub := &lensFactsStub{
		byRepo: map[string][]store.RecentFactEntry{
			"alpha": {{Path: "kb/a/1.md", Title: "a1", CommittedAt: 300}},
			"beta":  {{Path: "kb/b/1.md", Title: "b1", CommittedAt: 200}},
		},
		totalByRepo: map[string]int{"alpha": 1403, "beta": 234},
	}
	s := &Server{Manager: m, providers: storeProviders{factsCollection: stub}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	body := decodeLensFacts(t, getLensFacts(t, r, "/lenses/eng/facts?repo=beta"))
	if body.Total != 234 {
		t.Fatalf("narrowed total: got %d, want 234", body.Total)
	}
}

// ── Depth follows the request, so nothing is out of reach ────────────────────
//
// The fixed 500 was a WINDOW: each mount handed over its 500 most recent and
// paging walked inside that set, so a mount's 501st-newest fact could not be
// reached at any offset. Commit timestamps are comparable across mounts, so
// there is no reason for a window — a row in the global page [offset, offset+
// limit) is always within its own mount's first offset+limit rows, which makes
// that the exact depth to ask each mount for.

func TestLensFacts_RecencyDepthFollowsTheOffset(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	stub := &lensFactsStub{byRepo: map[string][]store.RecentFactEntry{
		"alpha": {{Path: "kb/a/1.md", Title: "a1", CommittedAt: 300}},
		"beta":  {{Path: "kb/b/1.md", Title: "b1", CommittedAt: 200}},
	}}
	s := &Server{Manager: m, providers: storeProviders{factsCollection: stub}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	getLensFacts(t, r, "/lenses/eng/facts?limit=50&offset=1200")
	for _, repo := range []string{"alpha", "beta"} {
		if got := stub.lastOpts[repo].Limit; got != 1250 {
			t.Fatalf("%s depth: got %d, want 1250 (offset+limit) — a fixed cap makes deep rows unreachable", repo, got)
		}
	}
}

func TestLensFacts_ReachesPastTheOldCandidateCap(t *testing.T) {
	// The regression in one shot: a mount with more than 500 facts, read at an
	// offset beyond 500. Under the fixed window this returned nothing.
	m, _ := newTestLensManager(t, "alpha")
	big := make([]store.RecentFactEntry, 900)
	for i := range big {
		big[i] = store.RecentFactEntry{
			Path:        fmt.Sprintf("kb/a/%03d.md", i),
			Title:       fmt.Sprintf("a%03d", i),
			CommittedAt: int64(900 - i), // committed_at-DESC, as the store returns
		}
	}
	stub := &lensFactsStub{byRepo: map[string][]store.RecentFactEntry{"alpha": big}}
	s := &Server{Manager: m, providers: storeProviders{factsCollection: stub}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[]}`)

	body := decodeLensFacts(t, getLensFacts(t, r, "/lenses/eng/facts?limit=5&offset=600"))
	if len(body.Facts) != 5 {
		t.Fatalf("rows past the old cap: got %d, want 5 — offset 600 was unreachable before", len(body.Facts))
	}
	if body.Facts[0].Title != "a600" {
		t.Fatalf("first row: got %q, want a600", body.Facts[0].Title)
	}
	if body.Total != 900 {
		t.Fatalf("total: got %d, want 900", body.Total)
	}
}

func TestLensFacts_DepthHasABackstop(t *testing.T) {
	// Unbounded depth would let one absurd offset ask every mount to materialise
	// its whole corpus. The ceiling exists only for that, far above any page a
	// reader scrolls to.
	m, _ := newTestLensManager(t, "alpha")
	stub := &lensFactsStub{byRepo: map[string][]store.RecentFactEntry{
		"alpha": {{Path: "kb/a/1.md", Title: "a1", CommittedAt: 1}},
	}}
	s := &Server{Manager: m, providers: storeProviders{factsCollection: stub}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[]}`)

	getLensFacts(t, r, "/lenses/eng/facts?limit=10&offset=99000000")
	if got := stub.lastOpts["alpha"].Limit; got != maxLensRecencyDepth {
		t.Fatalf("depth: got %d, want the %d backstop", got, maxLensRecencyDepth)
	}
}

// ── Relevance keeps its cap, because it is a different thing ─────────────────
//
// With a text query the per-mount lists are RELEVANCE-ranked, and per-mount
// ranks are not comparable across mounts (RFC §7.1) — which is why the union
// fuses by reciprocal rank rather than merging on a shared key. There is no
// "first N globally" to walk toward, so the bound is a RETRIEVAL DEPTH, not a
// window, and it stays. Same number as the old cap; different justification,
// which is why it is now a different constant.

func TestLensFacts_TextQueryKeepsTheRetrievalDepth(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha")
	stub := &lensFactsStub{byRepo: map[string][]store.RecentFactEntry{
		"alpha": {{Path: "kb/a/1.md", Title: "a1", CommittedAt: 1, Score: 0.9}},
	}}
	s := &Server{Manager: m, providers: storeProviders{factsCollection: stub}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[]}`)

	getLensFacts(t, r, "/lenses/eng/facts?query=widgets&limit=10&offset=900")
	if got := stub.lastOpts["alpha"].Limit; got != maxLensSearchCandidates {
		t.Fatalf("relevance depth: got %d, want the %d retrieval cap", got, maxLensSearchCandidates)
	}
}

// The count is EXACT when the fan-out held every row: a path on two mounts is
// one fact, and dedup can prove it. Summing per-mount COUNT(*) unconditionally
// would report two — which is why the sum is the fallback, not the rule.
func TestLensFacts_ExactUnionCountWhenNothingWasTruncated(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	stub := &lensFactsStub{byRepo: map[string][]store.RecentFactEntry{
		"alpha": {{Path: "kb/shared.md", Title: "from-write", CommittedAt: 300}},
		"beta":  {{Path: "kb/shared.md", Title: "from-read", CommittedAt: 200}},
	}}
	s := &Server{Manager: m, providers: storeProviders{factsCollection: stub}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	body := decodeLensFacts(t, getLensFacts(t, r, "/lenses/eng/facts"))
	if body.Total != 1 {
		t.Fatalf("total: got %d, want 1 — one path is one fact when dedup can see both copies", body.Total)
	}
}

// ── Dedupe spends depth, so depth has to answer for it ───────────────────────
//
// lensFanoutDepth's proof holds for the PRE-dedupe union: a row in the global
// page sits within its own mount's first offset+limit rows. A duplicate that
// loses to the write mount still consumed a row of its mount's depth, so a
// re-rooted fork whose newest rows are all copies of the upstream's can spend
// the entire budget and contribute nothing of its own.
//
// The damage is not only a short page. Page 1 came back FULL — of the
// upstream's older facts — while the genuinely newest rows in the union were
// never fetched at all.

// forkBesideUpstream models exactly that: `beta` is a fork of `alpha` that
// re-committed all three shared facts recently (so its copies are its newest
// rows and all lose the dedupe), and holds two unique facts that are older than
// those copies but far newer than anything on the write mount.
func forkBesideUpstream() *lensFactsStub {
	return &lensFactsStub{
		byRepo: map[string][]store.RecentFactEntry{
			"alpha": {
				{Path: "kb/a.md", Title: "a", CommittedAt: 100},
				{Path: "kb/b.md", Title: "b", CommittedAt: 90},
				{Path: "kb/c.md", Title: "c", CommittedAt: 80},
			},
			"beta": {
				{Path: "kb/a.md", Title: "fork-a", CommittedAt: 500},
				{Path: "kb/b.md", Title: "fork-b", CommittedAt: 490},
				{Path: "kb/c.md", Title: "fork-c", CommittedAt: 480},
				{Path: "kb/u1.md", Title: "u1", CommittedAt: 300},
				{Path: "kb/u2.md", Title: "u2", CommittedAt: 290},
			},
		},
	}
}

func TestLensFacts_DedupedDuplicatesDoNotEatTheDepth(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	stub := forkBesideUpstream()
	s := &Server{Manager: m, providers: storeProviders{factsCollection: stub}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	// Depth starts at offset+limit = 2, which beta spends entirely on copies
	// that lose the dedupe. Its unique facts are the two newest rows in the
	// union and must still be what page 1 is.
	body := decodeLensFacts(t, getLensFacts(t, r, "/lenses/eng/facts?limit=2"))
	if len(body.Facts) != 2 {
		t.Fatalf("page: got %d rows, want 2; body=%+v", len(body.Facts), body)
	}
	got := []string{body.Facts[0].Title, body.Facts[1].Title}
	want := []string{"u1", "u2"}
	if got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("page 1: got %v, want %v — the fork's own facts are the newest in the union", got, want)
	}
}

func TestLensFacts_DeepenedFanoutStillReachesEveryRow(t *testing.T) {
	// The reachability half of the same failure: with the depth spent on
	// duplicates, u1/u2 were unreachable at EVERY offset.
	m, _ := newTestLensManager(t, "alpha", "beta")
	stub := forkBesideUpstream()
	s := &Server{Manager: m, providers: storeProviders{factsCollection: stub}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	seen := map[string]bool{}
	for off := 0; off < 6; off += 2 {
		body := decodeLensFacts(t, getLensFacts(t,
			r, fmt.Sprintf("/lenses/eng/facts?limit=2&offset=%d", off)))
		for _, f := range body.Facts {
			seen[f.Title] = true
		}
	}
	// Five distinct facts: three shared (write copy wins) and the fork's two.
	for _, want := range []string{"a", "b", "c", "u1", "u2"} {
		if !seen[want] {
			t.Fatalf("%q was unreachable at every offset; saw %v", want, seen)
		}
	}
}

func TestLensFacts_NoRefanWhenNothingWasTruncated(t *testing.T) {
	// The loop must not cost a second round in the ordinary case: if every mount
	// handed over everything it had, there is no deeper row to go get.
	m, _ := newTestLensManager(t, "alpha", "beta")
	stub := &lensFactsStub{byRepo: map[string][]store.RecentFactEntry{
		"alpha": {{Path: "kb/a.md", Title: "a", CommittedAt: 100}},
		"beta":  {{Path: "kb/b.md", Title: "b", CommittedAt: 200}},
	}}
	s := &Server{Manager: m, providers: storeProviders{factsCollection: stub}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	getLensFacts(t, r, "/lenses/eng/facts?limit=50")
	if got := stub.lastOpts["alpha"].Limit; got != 50 {
		t.Fatalf("depth: got %d, want 50 — a complete fan-out must not deepen", got)
	}
}

func TestLensFacts_DeepeningStopsAtTheBackstop(t *testing.T) {
	// A pathological overlap must not fan out without bound: the loop stops at
	// maxLensRecencyDepth and answers with what it has.
	m, _ := newTestLensManager(t, "alpha", "beta")
	dupes := make([]store.RecentFactEntry, 0, 200)
	upstream := make([]store.RecentFactEntry, 0, 200)
	for i := 0; i < 200; i++ {
		p := fmt.Sprintf("kb/shared-%03d.md", i)
		// The fork's copies are always newer, so they always lose AND always
		// sort first: every round of deepening spends itself on duplicates.
		dupes = append(dupes, store.RecentFactEntry{Path: p, Title: "fork", CommittedAt: int64(10000 - i)})
		upstream = append(upstream, store.RecentFactEntry{Path: p, Title: "up", CommittedAt: int64(100 - i)})
	}
	stub := &lensFactsStub{
		byRepo:      map[string][]store.RecentFactEntry{"alpha": upstream, "beta": dupes},
		totalByRepo: map[string]int{"beta": 100000},
	}
	s := &Server{Manager: m, providers: storeProviders{factsCollection: stub}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	getLensFacts(t, r, "/lenses/eng/facts?limit=10")
	if got := stub.lastOpts["beta"].Limit; got != maxLensRecencyDepth {
		t.Fatalf("depth: got %d, want the %d backstop", got, maxLensRecencyDepth)
	}
}

func TestLensFacts_TextQueryNeverRefans(t *testing.T) {
	// Relevance ranks are not comparable across mounts, so there is no timestamp
	// horizon to satisfy and no deeper row to walk toward. Its bound is a
	// retrieval cap by design and must stay one round.
	m, _ := newTestLensManager(t, "alpha", "beta")
	stub := forkBesideUpstream()
	stub.totalByRepo = map[string]int{"alpha": 9000, "beta": 9000}
	s := &Server{Manager: m, providers: storeProviders{factsCollection: stub}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	getLensFacts(t, r, "/lenses/eng/facts?query=widgets&limit=2")
	if got := stub.lastOpts["beta"].Limit; got != maxLensSearchCandidates {
		t.Fatalf("relevance depth: got %d, want the %d retrieval cap unchanged", got, maxLensSearchCandidates)
	}
}

func TestLensFacts_OrdinaryTruncationCostsOneRound(t *testing.T) {
	// Truncation is the COMMON case — any mount bigger than the page is
	// truncated — so deepening must be driven by the merge horizon, not by
	// truncation alone. Two ordinary mounts with interleaved timestamps have to
	// answer in a single fan-out, or every page of every lens pays for the fork
	// case that never happens to it.
	m, _ := newTestLensManager(t, "alpha", "beta")
	mk := func(prefix string, base int64) []store.RecentFactEntry {
		out := make([]store.RecentFactEntry, 0, 200)
		for i := 0; i < 200; i++ {
			out = append(out, store.RecentFactEntry{
				Path:        fmt.Sprintf("kb/%s%03d.md", prefix, i),
				Title:       fmt.Sprintf("%s%03d", prefix, i),
				CommittedAt: base - int64(i)*2,
			})
		}
		return out
	}
	stub := &lensFactsStub{byRepo: map[string][]store.RecentFactEntry{
		// Interleaved: alpha on even stamps, beta on odd, no shared paths.
		"alpha": mk("a", 10000),
		"beta":  mk("b", 9999),
	}}
	s := &Server{Manager: m, providers: storeProviders{factsCollection: stub}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	body := decodeLensFacts(t, getLensFacts(t, r, "/lenses/eng/facts?limit=50"))
	if len(body.Facts) != 50 {
		t.Fatalf("page: got %d rows, want 50", len(body.Facts))
	}
	if got := stub.lastOpts["alpha"].Limit; got != 50 {
		t.Fatalf("depth: got %d, want 50 — an ordinary truncated fan-out must not deepen", got)
	}
}

// An offset near MaxInt overflows offset+limit into a NEGATIVE depth, which
// clears the `> maxLensRecencyDepth` test rather than tripping it and reaches
// the store as `LIMIT -1` — SQLite's spelling of NO limit. The backstop would
// then do the exact opposite of its stated job: one absurd offset asking every
// mount to materialise its whole corpus.
func TestLensFanoutDepth_OverflowingOffsetStillClamps(t *testing.T) {
	for _, tc := range []struct {
		name          string
		offset, limit int
		want          int
	}{
		{"ordinary page", 0, 50, 50},
		{"deep but representable", 9000, 500, 9500},
		{"over the cap", 20000, 500, maxLensRecencyDepth},
		{"overflows to negative", math.MaxInt - 10, 500, maxLensRecencyDepth},
		{"overflows exactly at MaxInt+1", math.MaxInt, 1, maxLensRecencyDepth},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := lensFanoutDepth("", tc.offset, tc.limit)
			if got != tc.want {
				t.Fatalf("lensFanoutDepth(\"\", %d, %d): got %d, want %d", tc.offset, tc.limit, got, tc.want)
			}
			if got <= 0 {
				t.Fatalf("depth %d is not a bound: a non-positive Limit means NO limit in the store", got)
			}
		})
	}
}

// Motif filtering fans out to EVERY mount, carrying the same terms and tier —
// the per-mount-resolution semantics made testable. Each mount expands the
// terms against its own alias vocabulary; the handler's job is to make sure
// every mount is asked the same question.
func TestLensFacts_MotifFilterReachesEveryMount(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	stub := &lensFactsStub{byRepo: map[string][]store.RecentFactEntry{}}
	s := &Server{Manager: m, providers: storeProviders{factsCollection: stub}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	if rec := getLensFacts(t, r, "/lenses/eng/facts?motifs=a-b&motif_match=token-2"); rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}

	for _, repo := range []string{"alpha", "beta"} {
		opts, ok := stub.lastOpts[repo]
		if !ok {
			t.Fatalf("mount %q was never queried", repo)
		}
		if got := fmt.Sprint(opts.Motifs); got != "[a-b]" {
			t.Errorf("%s Motifs: got %v, want [a-b]", repo, opts.Motifs)
		}
		if opts.MotifMatch != store.MotifMatchToken2 {
			t.Errorf("%s MotifMatch: got %q, want token-2", repo, opts.MotifMatch)
		}
	}
}

// The tiers this surface refuses are refused before the fan-out starts — no
// mount is queried at all (C3/MN6).
func TestLensFacts_LooseMotifTierRejected(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	stub := &lensFactsStub{byRepo: map[string][]store.RecentFactEntry{}}
	s := &Server{Manager: m, providers: storeProviders{factsCollection: stub}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	rec := getLensFacts(t, r, "/lenses/eng/facts?motifs=a-b&motif_match=token-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	if len(stub.lastOpts) != 0 {
		t.Errorf("mounts were queried despite the rejection: %v", stub.lastOpts)
	}
}

// Same two properties on the relevance twin.
func TestLensSearch_MotifFilterReachesEveryMount(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	stub := &lensSearchStub{byRepo: map[string][]store.SearchResult{}}
	s := &Server{Manager: m, providers: storeProviders{search: stub}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	rec := getLensFacts(t, r, "/lenses/eng/search?q=x&motifs=a-b&motif_match=token-2")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	for _, repo := range []string{"alpha", "beta"} {
		opts, ok := stub.lastOpts[repo]
		if !ok {
			t.Fatalf("mount %q was never queried", repo)
		}
		if got := fmt.Sprint(opts.Motifs); got != "[a-b]" {
			t.Errorf("%s Motifs: got %v, want [a-b]", repo, opts.Motifs)
		}
		if opts.MotifMatch != store.MotifMatchToken2 {
			t.Errorf("%s MotifMatch: got %q, want token-2", repo, opts.MotifMatch)
		}
	}
}

func TestLensSearch_LooseMotifTierRejected(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	stub := &lensSearchStub{byRepo: map[string][]store.SearchResult{}}
	s := &Server{Manager: m, providers: storeProviders{search: stub}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	rec := getLensFacts(t, r, "/lenses/eng/search?q=x&motifs=a-b&motif_match=token-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	if len(stub.lastOpts) != 0 {
		t.Errorf("mounts were queried despite the rejection: %v", stub.lastOpts)
	}
}

// The union rows carry each fact's motifs; a motif-free row omits the key.
func TestLensFacts_MotifsOnTheWire(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha")
	stub := &lensFactsStub{byRepo: map[string][]store.RecentFactEntry{
		"alpha": {
			{Path: "kb/a.md", Title: "Carries a motif", Motifs: []string{"a-b"}, CommittedAt: 2},
			{Path: "kb/b.md", Title: "Carries none", CommittedAt: 1},
		},
	}}
	s := &Server{Manager: m, providers: storeProviders{factsCollection: stub}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha"}`)

	rec := getLensFacts(t, r, "/lenses/eng/facts")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Facts []map[string]any `json:"facts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if len(body.Facts) != 2 {
		t.Fatalf("facts: got %d, want 2; body=%s", len(body.Facts), rec.Body.String())
	}
	if got := body.Facts[0]["motifs"]; fmt.Sprint(got) != "[a-b]" {
		t.Errorf("row 0 motifs: got %v, want [a-b]", got)
	}
	if _, present := body.Facts[1]["motifs"]; present {
		t.Errorf("motif-free row must omit the key entirely; body=%s", rec.Body.String())
	}
}

func TestLensSearch_MotifsOnTheWire(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha")
	withMotif := sr("kb/a.md", "Carries a motif", 10)
	withMotif.Motifs = []string{"a-b"}
	stub := &lensSearchStub{byRepo: map[string][]store.SearchResult{
		"alpha": {withMotif, sr("kb/b.md", "Carries none", 5)},
	}}
	s := &Server{Manager: m, providers: storeProviders{search: stub}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha"}`)

	rec := getLensFacts(t, r, "/lenses/eng/search?q=x")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if len(body.Results) != 2 {
		t.Fatalf("results: got %d, want 2; body=%s", len(body.Results), rec.Body.String())
	}
	if got := body.Results[0]["motifs"]; fmt.Sprint(got) != "[a-b]" {
		t.Errorf("row 0 motifs: got %v, want [a-b]", got)
	}
	if _, present := body.Results[1]["motifs"]; present {
		t.Errorf("motif-free row must omit the key entirely; body=%s", rec.Body.String())
	}
}
