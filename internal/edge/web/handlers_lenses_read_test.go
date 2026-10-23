package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/engine/store"

	"knomit/internal/engine/store/index"

	"knomit/internal/corpus/federate"
	"knomit/internal/corpus/repos"
	knomitfact "knomit/internal/domain/fact"
	"knomit/internal/edge/web/hal"
)

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

// ── lens union search (GET /lenses/{lens}/search) ────────────────────────────

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
	env := searchLens(t, "zulu", "alpha") // write=zulu sorts after read=alpha
	writeCosFact(t, env.m.Get("zulu"), "kb/x/1.md", "Write copy", 0.5, nil)
	writeCosFact(t, env.m.Get("alpha"), "kb/x/1.md", "Read copy", 0.99, nil) // higher score, would rank first

	body := decodeLensSearch(t, env.get(t, "/lenses/eng/search?q=x"))
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
// only fact ranks #0 in its mount with a LOW native score (50); beta has a
// high-score #0 (99) and a mid-score #1 (80). RRF interleaves by rank —
// alpha#0, beta#0, beta#1 — so alpha's low-score fact LEADS. A naive score sort
// would order beta-top(99), beta-second(80), alpha-top(50), putting alpha LAST.
// This test fails under naive interleaving.
func TestLensSearch_RRFNotNaiveInterleave(t *testing.T) {
	env := searchLens(t, "alpha", "beta")
	// Reads() is sorted by repo name → alpha is mount 0, beta is mount 1.
	writeCosFact(t, env.m.Get("alpha"), "kb/a/0.md", "alpha-top", 0.5, nil) // rank 0, LOW score
	writeCosFact(t, env.m.Get("beta"), "kb/b/0.md", "beta-top", 0.99, nil)
	writeCosFact(t, env.m.Get("beta"), "kb/b/1.md", "beta-second", 0.8, nil)

	body := decodeLensSearch(t, env.get(t, "/lenses/eng/search?q=x"))
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
	env := searchLens(t, "alpha", "beta")
	writeCosFact(t, env.m.Get("beta"), "kb/y/2.md", "Read only", 0.9, nil)

	beta := env.m.Get("beta")
	wantID := federate.ID12(beta.ID())
	wantPath := "kb://" + wantID + "/kb/y/2.md"

	body := decodeLensSearch(t, env.get(t, "/lenses/eng/search?q=x"))
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
	env := searchLens(t, "alpha", "beta")
	writeCosFact(t, env.m.Get("alpha"), "kb/a/1.md", "Write fact", 0.99, nil)
	writeCosFact(t, env.m.Get("beta"), "kb/b/2.md", "Read fact", 0.8, nil)

	body := decodeLensSearch(t, env.get(t, "/lenses/eng/search?q=x&repo=beta"))
	if len(body.Results) != 1 {
		t.Fatalf("results: got %d, want 1; body=%+v", len(body.Results), body)
	}
	if body.Results[0].Source.Repo != "beta" {
		t.Errorf("source.repo: got %q, want beta only", body.Results[0].Source.Repo)
	}
}

// An unknown repo= name is a well-formed request naming a nonexistent mount → 422.
func TestLensSearch_UnknownRepoFilter422(t *testing.T) {
	env := searchLens(t, "alpha", "beta")

	rec := env.get(t, "/lenses/eng/search?q=x&repo=ghost")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: got %q, want application/problem+json", got)
	}
}

// With no embedder handed to the handler (Server.Embedder nil) the handler does
// not hoist a query vector: each mount's own embedder answers for itself, and
// the mount rows are served. A store with no embedder at all has no vectors to
// search, so a text query answers an empty 200 (the store's documented
// exception), never an error from this handler.
func TestLensSearch_EmbedderOffFallback(t *testing.T) {
	t.Run("server without an embedder, mounts with one", func(t *testing.T) {
		emb := &cosEmbedder{}
		m, _ := newTestLensManagerWith(t, emb, "alpha", "beta")
		writeCosFact(t, m.Get("beta"), "kb/b/2.md", "Read fact", 0.9, nil)
		seen := observeMountSearch(t, m, "alpha", "beta")
		r := (&Server{Manager: m}).NewAPIRouter() // Embedder left nil
		createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)
		before := emb.queries.Load()

		body := decodeLensSearch(t, getLensFacts(t, r, "/lenses/eng/search?q=x"))

		if len(body.Results) != 1 {
			t.Fatalf("results: got %d, want 1; body=%+v", len(body.Results), body)
		}
		for _, n := range []string{"alpha", "beta"} {
			got := seen.of(n)
			require.Len(t, got, 1)
			require.Empty(t, got[0].QueryVec, "mount %s was handed a hoisted vector although the server has no embedder", n)
		}
		require.EqualValues(t, 2, emb.queries.Load()-before, "each mount embeds for itself")
	})
	t.Run("no embedder anywhere", func(t *testing.T) {
		m, _ := newTestLensManager(t, "alpha", "beta")
		writeCosFact(t, m.Get("beta"), "kb/b/2.md", "Read fact", 0.9, nil)
		r := (&Server{Manager: m}).NewAPIRouter()
		createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

		body := decodeLensSearch(t, getLensFacts(t, r, "/lenses/eng/search?q=x"))

		require.NotNil(t, body.Results)
		require.Empty(t, body.Results)
	})
}

// An empty union (no mount returns results) yields results:[] and total:0, never null.
func TestLensSearch_EmptyMounts(t *testing.T) {
	env := searchLens(t, "alpha", "beta")

	rec := env.get(t, "/lenses/eng/search?q=x")
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
	s := &Server{Manager: m}
	r := s.NewAPIRouter()

	rec := getLensFacts(t, r, "/lenses/missing/search?q=x")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// ── lens union completions (GET /lenses/{lens}/completions) ───────────────────

// seedDomains writes one fact per path into ri's agent branch, each carrying
// the given domains, so the repo's real store answers completions from them.
func seedDomains(t *testing.T, ri *repos.RepoInstance, path string, domains ...string) {
	t.Helper()
	dom, err := json.Marshal(domains)
	require.NoError(t, err)
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		require.NotNil(t, svc, "the test repo must have a live store")
		_, err := svc.Facts().WriteFact(context.Background(), ri.AgentBranch(), path,
			"---\ntype: observation\nconfidence: 0.9\ndomain: "+string(dom)+"\n---\n# "+path+"\n\nbody\n",
			"add "+path, "")
		require.NoError(t, err)
	}))
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
	s := &Server{Manager: m}
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
	s := &Server{Manager: m}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"zulu","reads":[{"repo":"alpha"},{"repo":"beta"}]}`)

	body := decodeLensCompletions(t, getLensFacts(t, r, "/lenses/eng/completions?category=repo&prefix=A"))
	want := []string{"alpha"}
	if len(body.Values) != len(want) || body.Values[0] != want[0] {
		t.Fatalf("values: got %v, want %v (case-insensitive prefix narrows)", body.Values, want)
	}
}

// category=domain unions each mount's REAL completions, de-duplicated across
// mounts. Mounts fan out in Reads() order, so a value only beta holds comes
// after what alpha holds; the order inside one mount is the store's, which
// does not sort, so it is not asserted.
func TestLensCompletions_DomainMergesWithoutDuplicates(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	seedDomains(t, m.Get("alpha"), "kb/technology/ai/one.md", "ai", "alignment")
	seedDomains(t, m.Get("beta"), "kb/technology/ai/two.md", "ai", "robotics") // "ai" collides with alpha's
	s := &Server{Manager: m, AgentBranch: "machine/test"}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	body := decodeLensCompletions(t, getLensFacts(t, r, "/lenses/eng/completions?category=domain"))
	if len(body.Values) != 3 {
		t.Fatalf("values: got %v, want ai, alignment, robotics once each (union, deduped)", body.Values)
	}
	if body.Values[2] != "robotics" {
		t.Errorf("order: got %v, want beta's own value last (mounts fan out in binding order)", body.Values)
	}
	got := append([]string(nil), body.Values...)
	sort.Strings(got)
	if want := []string{"ai", "alignment", "robotics"}; !reflect.DeepEqual(got, want) {
		t.Errorf("values: got %v, want %v", got, want)
	}
}

// An unknown category is the SAME error the repo handler gives: the store-shaped
// error flows through writeStoreError → 500 problem+json.
func TestLensCompletions_UnknownCategory500(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	s := &Server{Manager: m}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	rec := getLensFacts(t, r, "/lenses/eng/completions?category=bogus")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "unknown completion category: bogus") {
		t.Errorf("body should carry the store's own error, got %s", rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: got %q, want application/problem+json", got)
	}
}

// An empty union (a known category with no values on any mount) yields
// values:[], never null.
func TestLensCompletions_EmptyUnion(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	s := &Server{Manager: m}
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
	s := &Server{Manager: m}
	r := s.NewAPIRouter()

	rec := getLensFacts(t, r, "/lenses/missing/completions?category=repo")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// One mount's Completions read fails: the whole union request is a 500 that
// carries THAT mount's injected error. The hook is installed on beta's store
// only, matches the Completions read by name, and is hit exactly once (alpha's
// store is a different Service and is unaffected).
func TestLensCompletions_MountErrorFailsTheWholeRequest(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	seedDomains(t, m.Get("alpha"), "kb/technology/ai/one.md", "ai")
	injected := errors.New("injected beta completions read failure")
	var hits atomic.Int64
	require.NoError(t, m.Get("beta").WithRead(func(svc *store.Service) {
		t.Cleanup(svc.Search().SetReadFaultForTest(failIndexRead(index.ReadCompletions, injected, &hits)))
	}))
	s := &Server{Manager: m, AgentBranch: "machine/test"}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	rec := getLensFacts(t, r, "/lenses/eng/completions?category=domain")

	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), injected.Error())
	require.EqualValues(t, 1, hits.Load(), "beta's Completions read must have reached the hook exactly once")
}

// ---- Single fact read through a lens (GET /lenses/{lens}/facts/{path...}) ----
//
// The mounts are real stores. The fact a test reads lives on the RESOLVED
// mount at its pinned branch, and every other candidate — the other mount, and
// the resolved mount's own agent branch — holds a twin at the same path with
// another title. The fact cites two targets that exist on the resolved mount's
// branch and two that exist only on the candidates (lensFactRefs), so reading
// the fact or resolving its refs anywhere else is seen.

// lensFactRefs is the refs mod of a lens fact under dir: dir/p1.md and
// dir/p2.md are written with it, dir/d1.md and dir/d2.md only with its decoys.
func lensFactRefs(dir string) func(*knomitfact.Fact) {
	return refsTo(knomitfact.Observation, dir+"/p1.md", dir+"/d1.md", dir+"/p2.md", dir+"/d2.md")
}

// wantLensFactRefs is what lensFactRefs(dir) resolves to on the resolved mount.
func wantLensFactRefs(dir string) [][2]string {
	return [][2]string{{dir + "/p1.md", "fact"}, {dir + "/d1.md", "broken"}, {dir + "/p2.md", "fact"}, {dir + "/d2.md", "broken"}}
}

// writeLensFact writes path titled title, citing lensFactRefs(dir), and its
// targets dir/p1.md and dir/p2.md to (ri, branch) in one commit, and returns
// the commit.
func writeLensFact(t *testing.T, ri *repos.RepoInstance, branch, dir, path, title string) string {
	t.Helper()
	return writeLensStep(t, ri, branch,
		plainFact(path, title, lensFactRefs(dir)),
		plainFact(dir+"/p1.md", "Target one", nil),
		plainFact(dir+"/p2.md", "Target two", nil))
}

// writeLensDecoy writes a twin of path titled title, and the targets only the
// decoys have (dir/d1.md, dir/d2.md), to (ri, branch).
func writeLensDecoy(t *testing.T, ri *repos.RepoInstance, branch, dir, path, title string) {
	t.Helper()
	writeLensStep(t, ri, branch,
		plainFact(path, title, nil),
		plainFact(dir+"/d1.md", "Decoy one", nil),
		plainFact(dir+"/d2.md", "Decoy two", nil))
}

// hookMountReadFact is hookReadFact on the named mount's store.
func hookMountReadFact(t *testing.T, m *repos.Manager, name, fail string, err error) *readFactLog {
	t.Helper()
	var l *readFactLog
	withMountStore(t, m, name, func(svc *store.Service) { l = hookReadFact(t, svc, fail, err) })
	return l
}

// lensFactViewBody mirrors the single-fact wire shape: the repo FactView body
// (path/title/body/as_of/refs/_links) plus the lens-level source block.
type lensFactViewBody struct {
	factViewBody
	Source struct {
		Repo   string `json:"repo"`
		ID     string `json:"id"`
		Branch string `json:"branch"`
	} `json:"source"`
}

func decodeLensFactView(t *testing.T, rec *httptest.ResponseRecorder) lensFactViewBody {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body lensFactViewBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	return body
}

// requireLensProblem requires a problem+json answer with this status, title
// and detail.
func requireLensProblem(t *testing.T, rec *httptest.ResponseRecorder, status int, title, detail string) {
	t.Helper()
	require.Equal(t, status, rec.Code, rec.Body.String())
	require.Equal(t, "application/problem+json", rec.Header().Get("Content-Type"))
	gotTitle, gotDetail := decodeProblem(t, rec)
	require.Equal(t, title, gotTitle)
	require.Equal(t, detail, gotDetail)
}

// A bare kb/... path reads from the WRITE repo at its read-mount branch — no
// dedupe scan — even when a read mount shadows the SAME repo-relative path with
// different content. The write repo's body wins, the source and as_of name the
// write repo's pinned branch and its head, and refs resolve there. Sabotage:
// read the read mount (alpha), read the write repo at its agent branch, stamp
// another head, or resolve refs on another candidate.
func TestLensFact_BarePathReadsWriteRepo(t *testing.T) {
	m, _ := newTestLensManager(t, "zulu", "alpha") // write=zulu sorts after read=alpha
	zulu, alpha := m.Get("zulu"), m.Get("alpha")
	withMountStore(t, m, "zulu", func(svc *store.Service) {
		require.NoError(t, svc.CreateBranch(t.Context(), betaPinnedBranch, zulu.AgentBranch()))
	})
	head := writeLensFact(t, zulu, betaPinnedBranch, "kb/x", "kb/x/1.md", "Write copy")
	writeLensDecoy(t, zulu, zulu.AgentBranch(), "kb/x", "kb/x/1.md", "Agent copy")
	writeLensDecoy(t, alpha, alpha.AgentBranch(), "kb/x", "kb/x/1.md", "Read copy")
	s := &Server{Manager: m}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"zulu","reads":[{"repo":"zulu","branch":"`+betaPinnedBranch+`"},{"repo":"alpha"}]}`)
	zuluReads := hookMountReadFact(t, m, "zulu", "", nil)
	alphaReads := hookMountReadFact(t, m, "alpha", "", nil)

	body := decodeLensFactView(t, getLensFacts(t, r, "/lenses/eng/facts/kb/x/1.md"))
	require.Equal(t, "kb/x/1.md", body.Path, "a write-repo fact echoes the bare path")
	require.Equal(t, "Write copy", body.Title)
	require.Equal(t, "zulu", body.Source.Repo)
	require.Equal(t, federate.ID12(zulu.ID()), body.Source.ID)
	require.Equal(t, betaPinnedBranch, body.Source.Branch)
	require.Equal(t, betaPinnedBranch, body.AsOf.Branch)
	require.Equal(t, head, body.AsOf.Commit, "as_of is the pinned branch's head")
	require.Equal(t, APIBase+"/repos/zulu/branches/"+hal.EncodeBranch(betaPinnedBranch)+"/facts/kb/x/1.md",
		body.Links["self"].Href)
	requireRefKinds(t, body.factViewBody, wantLensFactRefs("kb/x")...)
	// No dedupe scan on a bare path: the read mount is NEVER read — only the
	// write repo is, once. Locks in "bare means write repo, period".
	require.Equal(t, []string{"head kb/x/1.md"}, zuluReads.all())
	require.Empty(t, alphaReads.all(), "a bare-path request must not read the read mount")
}

// A kb://<id12>/kb/... path (URL-encoded by the client) resolves to that mount
// at its own pinned branch, reads the MOUNT-RELATIVE path, returns its fact
// body, and carries that mount's source identity. Sabotage: read the write
// repo, read the mount's agent branch, keep the qualifier on the read path, or
// resolve refs on another candidate.
func TestLensFact_QualifiedPathHitsMount(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	alpha, beta := m.Get("alpha"), m.Get("beta")
	withMountStore(t, m, "beta", func(svc *store.Service) {
		require.NoError(t, svc.CreateBranch(t.Context(), betaPinnedBranch, beta.AgentBranch()))
	})
	head := writeLensFact(t, beta, betaPinnedBranch, "kb/y", "kb/y/2.md", "Read only")
	writeLensDecoy(t, beta, beta.AgentBranch(), "kb/y", "kb/y/2.md", "Beta agent copy")
	writeLensDecoy(t, alpha, alpha.AgentBranch(), "kb/y", "kb/y/2.md", "Write copy")
	s := &Server{Manager: m}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta","branch":"`+betaPinnedBranch+`"}]}`)
	betaReads := hookMountReadFact(t, m, "beta", "", nil)
	alphaReads := hookMountReadFact(t, m, "alpha", "", nil)

	id := federate.ID12(beta.ID())
	body := decodeLensFactView(t, getLensFacts(t, r, "/lenses/eng/facts/"+url.PathEscape("kb://"+id+"/kb/y/2.md")))
	require.Equal(t, "Read only", body.Title)
	// The top-level path echoes the canonical QUALIFIED wire form for a read
	// mount, so a client can round-trip it into another lens request and land on
	// the same fact (not silently on the write repo).
	require.Equal(t, "kb://"+id+"/kb/y/2.md", body.Path)
	require.Equal(t, "beta", body.Source.Repo)
	require.Equal(t, id, body.Source.ID)
	require.Equal(t, betaPinnedBranch, body.Source.Branch)
	require.Equal(t, betaPinnedBranch, body.AsOf.Branch)
	require.Equal(t, head, body.AsOf.Commit)
	require.Equal(t, APIBase+"/repos/beta/branches/"+hal.EncodeBranch(betaPinnedBranch)+"/facts/kb/y/2.md",
		body.Links["self"].Href)
	requireRefKinds(t, body.factViewBody, wantLensFactRefs("kb/y")...)
	require.Equal(t, []string{"head kb/y/2.md"}, betaReads.all())
	require.Empty(t, alphaReads.all())
}

// A kb://<id12>/… whose id12 is not mounted in the lens returns 404 with the
// SAME body as a genuinely-missing fact — mount topology must not leak (a
// caller cannot tell "unknown mount" from "no such fact").
func TestLensFact_UnknownID404MatchesNotFound(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta") // no facts anywhere
	s := &Server{Manager: m}
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
// single-fact handler), and a real backend error is a 500 carrying the store's
// error (not masked as 404).
func TestLensFact_MissingAndBackendError(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	alpha := m.Get("alpha")
	writeLensStep(t, alpha, alpha.AgentBranch(), plainFact("kb/gone.md", "Gone", nil), plainFact("kb/x/1.md", "Present", nil))
	withMountStore(t, m, "alpha", func(svc *store.Service) {
		_, _, err := svc.Facts().BatchWriteFacts(t.Context(), alpha.AgentBranch(), nil, []string{"kb/gone.md"}, "retract", "retract")
		require.NoError(t, err)
	})
	s := &Server{Manager: m}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	requireLensProblem(t, getLensFacts(t, r, "/lenses/eng/facts/kb/gone.md"), http.StatusNotFound,
		"Fact not found", `no fact at path "kb/gone.md"`)

	reads := hookMountReadFact(t, m, "alpha", "head kb/x/1.md", errors.New("disk on fire"))
	requireLensProblem(t, getLensFacts(t, r, "/lenses/eng/facts/kb/x/1.md"), http.StatusInternalServerError,
		"Failed to read fact", "disk on fire")
	require.Equal(t, []string{"head kb/x/1.md"}, reads.all())
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

// Both mounts are narrowed by the motif tier. The corpus separates the exact tier from the token-2 tier on each
// mount (a fact sharing two stemmed tokens with the query but not its spelling),
// so a mount that was not handed the tier answers differently from one that was.
func TestLensSearch_MotifFilterReachesEveryMount(t *testing.T) {
	env := searchLens(t, "alpha", "beta")
	spell := func(m string) func(*knomitfact.Fact) { return func(f *knomitfact.Fact) { f.Motifs = []string{m} } }
	writeCosFact(t, env.m.Get("alpha"), "kb/a1.md", "alpha exact", 0.9, spell("silent-fallback-path"))
	writeCosFact(t, env.m.Get("alpha"), "kb/a2.md", "alpha near", 0.9, spell("silent-fallback-retry"))
	writeCosFact(t, env.m.Get("beta"), "kb/b1.md", "beta near", 0.9, spell("silent-fallback-timeout"))
	writeCosFact(t, env.m.Get("beta"), "kb/b2.md", "beta unrelated", 0.9, spell("scope-creep"))
	reads := countMountReads(t, env.m, []string{"alpha", "beta"}, "Search")
	title := func(url string) []string {
		rec := env.get(t, url)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var got []string
		for _, r := range decodeLensSearch(t, rec).Results {
			got = append(got, r.Title)
		}
		sort.Strings(got)
		return got
	}

	require.Equal(t, []string{"alpha exact"}, title("/lenses/eng/search?q=x&motifs=silent-fallback-path&motif_match=exact"))
	require.Equal(t, []string{"alpha exact", "alpha near", "beta near"},
		title("/lenses/eng/search?q=x&motifs=silent-fallback-path&motif_match=token-2"))
	for _, n := range []string{"alpha", "beta"} {
		require.EqualValues(t, 2, reads[n]["Search"].Load(), "mount %s must be searched by both requests", n)
	}
}

func TestLensSearch_LooseMotifTierRejected(t *testing.T) {
	env := searchLens(t, "alpha", "beta")
	reads := countMountReads(t, env.m, []string{"alpha", "beta"}, "Search")

	rec := env.get(t, "/lenses/eng/search?q=x&motifs=a-b&motif_match=token-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	for _, n := range []string{"alpha", "beta"} {
		require.EqualValues(t, 0, reads[n]["Search"].Load(), "mount %s was queried despite the rejection", n)
	}

	// A permitted tier reaches every mount once, so the counter can move.
	rec = env.get(t, "/lenses/eng/search?q=x&motifs=a-b&motif_match=token-2")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	for _, n := range []string{"alpha", "beta"} {
		require.EqualValues(t, 1, reads[n]["Search"].Load(), "mount %s", n)
	}
}

func TestLensSearch_MotifsOnTheWire(t *testing.T) {
	env := searchLens(t, "alpha")
	writeCosFact(t, env.m.Get("alpha"), "kb/a.md", "Carries a motif", 0.9, func(f *knomitfact.Fact) { f.Motifs = []string{"a-b"} })
	writeCosFact(t, env.m.Get("alpha"), "kb/b.md", "Carries none", 0.8, nil)

	rec := env.get(t, "/lenses/eng/search?q=x")
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

// The /search twin of the inversion in TestLensFacts_AMergeOnAnyMountBindsTheWholeLens
// — the same rule, the other envelope, over the same real stores and
// vocabularies (seedDivergentVocabularies). Each store matches the spellings it
// is handed, so what the lens widens the query to decides who answers.
func TestLensSearch_AMergeOnAnyMountBindsTheWholeLens(t *testing.T) {
	env := searchLens(t, "alpha", "beta")
	seedDivergentVocabularies(t, env.m)

	rec := env.get(t, "/lenses/eng/search?q=x&motifs=config-drift")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Results []struct {
			Title string `json:"title"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	titles := map[string]bool{}
	for _, res := range body.Results {
		titles[res.Title] = true
	}
	if !titles["Alpha aliased"] || !titles["Alpha exact"] {
		t.Errorf("the merging mount's carriers are missing: %v", titles)
	}
	if !titles["Beta aliased"] {
		t.Errorf("the non-merging mount did not answer for the merged shape: %v", titles)
	}
	if !titles["Beta exact"] {
		t.Errorf("the non-merging mount contributed nothing at all: %v", titles)
	}
	if titles["Beta other"] {
		t.Errorf("a fact outside the shape answered: %v", titles)
	}
}

// ── Lens fact sub-resources ──────────────────────────────────────────────────
//
// The lens facts route registered only the fact READER, so
// /lenses/{lens}/facts/{path}/incoming matched the facts/* wildcard as the
// literal path "…/<uuid>.md/incoming" and fell into the reader, which 500s
// ("readFileWithHash: entry …: object not found") for EVERY fact. The web UI
// only escaped it by fetching edges through the source mount's repo-scoped
// route; any direct API consumer deriving an edge URL from a lens fact URL hit
// the 500, which reads like corruption but is a missing route (issue #178).
//
// The mounts are real stores, so the sub-resource reads what the RESOLVED
// mount holds at its OWN branch: each test gives every other candidate (the
// other mount, the mount's agent branch) the same fact path with other refs.
// The fact the sub-resource names exists at the resolved mount: the dispatch
// is gated on the same read the fact endpoint performs.

// writeLensStep writes files to branch of ri's store in one commit and returns
// the commit.
func writeLensStep(t *testing.T, ri *repos.RepoInstance, branch string, specs ...factSpec) string {
	t.Helper()
	files := map[string]string{}
	for _, s := range specs {
		files[s.path] = factText(t, s.path, s.title, 0.9, s.mod)
	}
	var commit string
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		require.NotNil(t, svc, "the test repo must have a live store")
		h, _, err := svc.Facts().BatchWriteFacts(context.Background(), branch, files, nil, "add facts", "learn")
		require.NoError(t, err)
		commit = h
	}))
	return commit
}

// A bare kb/… path's /incoming resolves the same way the lens fact READ does:
// the write repo at its read-mount branch, which here is pinned away from its
// agent branch. The store is asked for the fact path with /incoming stripped.
// Sabotage: read the write repo at its agent branch (Agent decoy), read
// another mount (Alpha decoy), or keep the suffix on the path (404).
func TestLensFactIncoming_BarePathReadsWriteRepo(t *testing.T) {
	m, _ := newTestLensManager(t, "zulu", "alpha")
	zulu, alpha := m.Get("zulu"), m.Get("alpha")
	withMountStore(t, m, "zulu", func(svc *store.Service) {
		require.NoError(t, svc.CreateBranch(t.Context(), betaPinnedBranch, zulu.AgentBranch()))
	})
	c := map[string]string{}
	c["w1"] = writeLensStep(t, zulu, betaPinnedBranch, plainFact("kb/x/1.md", "Write copy", nil))
	c["w2"] = writeLensStep(t, zulu, betaPinnedBranch, plainFact("kb/x/2.md", "Cites it", refsTo(knomitfact.Principle, "kb/x/1.md")))
	c["w3"] = writeLensStep(t, zulu, betaPinnedBranch, plainFact("kb/x/3.md", "Also cites it", refsTo(knomitfact.Hypothesis, "kb/x/1.md")))
	writeLensStep(t, zulu, zulu.AgentBranch(),
		plainFact("kb/x/1.md", "Agent copy", nil),
		plainFact("kb/x/4.md", "Agent decoy", refsTo(knomitfact.Insight, "kb/x/1.md")))
	writeLensStep(t, alpha, alpha.AgentBranch(),
		plainFact("kb/x/1.md", "Alpha copy", nil),
		plainFact("kb/x/5.md", "Alpha decoy", refsTo(knomitfact.Insight, "kb/x/1.md")))

	s := &Server{Manager: m}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"zulu","reads":[{"repo":"zulu","branch":"`+betaPinnedBranch+`"},{"repo":"alpha"}]}`)

	body := decodeRefs(t, getLensFacts(t, r, "/lenses/eng/facts/kb/x/1.md/incoming"))
	// The _links stay repo-scoped: they name the resolved mount, its branch
	// and the stripped fact path.
	zuluBranch := APIBase + "/repos/zulu/branches/" + hal.EncodeBranch(betaPinnedBranch)
	require.Equal(t, zuluBranch+"/facts/kb/x/1.md/incoming", body.Links["self"].Href)
	requireRefs(t, body, c, zuluBranch,
		wantRef{"kb/x/2.md", "Cites it", "principle", "w2", false},
		wantRef{"kb/x/3.md", "Also cites it", "hypothesis", "w3", false})
}

// A kb://<id12>/… path's /outgoing resolves to THAT mount at its own pinned
// branch, not to the write repo LensMiddleware leaves in the request context,
// and the store is queried with the MOUNT-RELATIVE path. Sabotage: read the
// write repo (6), beta's agent branch (7), or keep the kb://<id12>/ qualifier
// on the path (404).
func TestLensFactOutgoing_QualifiedPathHitsMount(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	alpha, beta := m.Get("alpha"), m.Get("beta")
	withMountStore(t, m, "beta", func(svc *store.Service) {
		require.NoError(t, svc.CreateBranch(t.Context(), betaPinnedBranch, beta.AgentBranch()))
	})
	c := map[string]string{}
	c["b1"] = writeLensStep(t, beta, betaPinnedBranch, plainFact("kb/y/9.md", "Derived from", typed(knomitfact.Principle)))
	c["b2"] = writeLensStep(t, beta, betaPinnedBranch, plainFact("kb/y/8.md", "Also derived", typed(knomitfact.Hypothesis)))
	writeLensStep(t, beta, betaPinnedBranch, plainFact("kb/y/2.md", "Read only", refsTo(knomitfact.Observation, "kb/y/9.md", "kb/y/8.md")))
	writeLensStep(t, beta, beta.AgentBranch(),
		plainFact("kb/y/7.md", "Beta agent target", nil),
		plainFact("kb/y/2.md", "Beta agent copy", refsTo(knomitfact.Observation, "kb/y/7.md")))
	writeLensStep(t, alpha, alpha.AgentBranch(),
		plainFact("kb/y/6.md", "Write target", nil),
		plainFact("kb/y/2.md", "Write copy", refsTo(knomitfact.Observation, "kb/y/6.md")))

	s := &Server{Manager: m}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta","branch":"`+betaPinnedBranch+`"}]}`)

	id := federate.ID12(beta.ID())
	body := decodeRefs(t, getLensFacts(t, r, "/lenses/eng/facts/"+url.PathEscape("kb://"+id+"/kb/y/2.md")+"/outgoing"))
	betaBranch := APIBase + "/repos/beta/branches/" + hal.EncodeBranch(betaPinnedBranch)
	require.Equal(t, betaBranch+"/facts/kb/y/2.md/outgoing", body.Links["self"].Href)
	requireRefs(t, body, c, betaBranch,
		wantRef{"kb/y/9.md", "Derived from", "principle", "b1", false},
		wantRef{"kb/y/8.md", "Also derived", "hypothesis", "b2", false})
}

// An unmounted id12 must 404 through the sub-resource route too, with the same
// body as a missing fact — the dispatch may not become a mount-topology oracle.
func TestLensFactSub_UnknownID404(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	s := &Server{Manager: m}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	rec := getLensFacts(t, r, "/lenses/eng/facts/"+url.PathEscape("kb://0123456789ab/kb/y/2.md")+"/incoming")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	var pb struct {
		Title string `json:"title"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &pb)
	if pb.Title != "Fact not found" {
		t.Errorf("title: got %q, want %q", pb.Title, "Fact not found")
	}
}

// A sub-resource on a MOUNTED id whose fact does not exist must answer exactly
// like an unmounted id: 404, naming nothing about the mount.
//
// Registering the dispatch reopened the topology oracle the read path closes.
// The sub-resource handlers report failure through writeStoreError, not
// lensFactNotFound, and handleFactCommits does not fail at all for a path with
// no commits — it returns an empty 200 whose _links.self carries the mount's
// real repo name and branch. So a caller could iterate candidate id12s and read
// "mounted" off a 200 versus a 404, which is precisely what lensFactNotFound's
// doc and openapi.yaml both promise cannot happen.
func TestLensFactSub_AbsentFactNeverNamesTheMount(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	s := &Server{Manager: m} // no facts anywhere
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	beta := m.Get("beta")
	id := federate.ID12(beta.ID())
	mounted := url.PathEscape("kb://" + id + "/kb/y/2.md")
	unmounted := url.PathEscape("kb://0123456789ab/kb/y/2.md")

	for _, sub := range []string{"commits", "incoming", "outgoing"} {
		recMounted := getLensFacts(t, r, "/lenses/eng/facts/"+mounted+"/"+sub)
		recUnmounted := getLensFacts(t, r, "/lenses/eng/facts/"+unmounted+"/"+sub)

		if recMounted.Code != http.StatusNotFound {
			t.Errorf("/%s on a mounted id with no such fact: got %d, want 404; body=%s",
				sub, recMounted.Code, recMounted.Body.String())
		}
		if recUnmounted.Code != recMounted.Code {
			t.Errorf("/%s: mounted=%d unmounted=%d — the status alone reveals the mount",
				sub, recMounted.Code, recUnmounted.Code)
		}
		// The body may echo the requested path (it differs by id), but it must
		// never name the mount's repo or its branch.
		body := recMounted.Body.String()
		if strings.Contains(body, `"beta"`) || strings.Contains(body, "/repos/beta/") {
			t.Errorf("/%s response names the mount repo: %s", sub, body)
		}
		if strings.Contains(body, beta.AgentBranch()) {
			t.Errorf("/%s response names the mount branch %q: %s", sub, beta.AgentBranch(), body)
		}
	}
}

// The gate reads the fact at the RESOLVED mount and branch, not at any other
// candidate: kb/x/only.md exists on the write repo's agent branch and on the
// read mount, but not at the write repo's pinned branch, so all three
// sub-resources are the lens 404, and only the resolved mount was read.
// Sabotage: gate on the mount's agent branch or on another mount (/commits
// answers an empty 200), or skip the gate.
func TestLensFactSub_GateReadsTheResolvedMount(t *testing.T) {
	m, _ := newTestLensManager(t, "zulu", "alpha")
	zulu, alpha := m.Get("zulu"), m.Get("alpha")
	withMountStore(t, m, "zulu", func(svc *store.Service) {
		require.NoError(t, svc.CreateBranch(t.Context(), betaPinnedBranch, zulu.AgentBranch()))
	})
	writeLensStep(t, zulu, betaPinnedBranch, plainFact("kb/x/1.md", "Write copy", nil))
	writeLensStep(t, zulu, zulu.AgentBranch(), plainFact("kb/x/only.md", "Agent only", nil))
	writeLensStep(t, alpha, alpha.AgentBranch(), plainFact("kb/x/only.md", "Alpha only", nil))
	s := &Server{Manager: m}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"zulu","reads":[{"repo":"zulu","branch":"`+betaPinnedBranch+`"},{"repo":"alpha"}]}`)
	zuluReads := hookMountReadFact(t, m, "zulu", "", nil)
	alphaReads := hookMountReadFact(t, m, "alpha", "", nil)

	for _, sub := range []string{"commits", "incoming", "outgoing"} {
		requireLensProblem(t, getLensFacts(t, r, "/lenses/eng/facts/kb/x/only.md/"+sub), http.StatusNotFound,
			"Fact not found", `no fact at path "kb/x/only.md/`+sub+`"`)
	}
	require.Equal(t, []string{"head kb/x/only.md", "head kb/x/only.md", "head kb/x/only.md"}, zuluReads.all())
	require.Empty(t, alphaReads.all())
}

// A gate read that fails with anything but "not found" is a 500 carrying the
// store's error, and the sub-resource is not served. Sabotage: ignore the
// gate's error, or map it to the lens 404.
func TestLensFactSub_GateErrorIs500(t *testing.T) {
	m, _ := newTestLensManager(t, "zulu", "alpha")
	zulu := m.Get("zulu")
	writeLensStep(t, zulu, zulu.AgentBranch(), plainFact("kb/x/1.md", "Write copy", nil))
	s := &Server{Manager: m}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"zulu","reads":[{"repo":"alpha"}]}`)
	reads := hookMountReadFact(t, m, "zulu", "head kb/x/1.md", errors.New("gate on fire"))
	counts := countMountReads(t, m, []string{"zulu"}, "ExplainFact")

	requireLensProblem(t, getLensFacts(t, r, "/lenses/eng/facts/kb/x/1.md/incoming"), http.StatusInternalServerError,
		"Failed to read fact", "gate on fire")
	require.Equal(t, []string{"head kb/x/1.md"}, reads.all())
	require.EqualValues(t, 0, counts["zulu"]["ExplainFact"].Load(), "the sub-resource must not be served past a failed gate")
}
