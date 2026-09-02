package web

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"knomit/internal/embeddings/params"
	"knomit/internal/store"
)

// countingEmbedder answers every query with the same fixed vector and counts
// the inferences. The vector's VALUE is irrelevant to these tests; the COUNT is
// the whole point, and a fixed vector is what makes "hoisted" and "per-mount"
// results comparable at all.
type countingEmbedder struct {
	queries atomic.Int64
	vec     []float32
}

func (e *countingEmbedder) EmbedQuery(_ context.Context, _ string) ([]float32, error) {
	e.queries.Add(1)
	return e.vec, nil
}

func (e *countingEmbedder) EmbedDocument(context.Context, string, string) ([]float32, error) {
	return e.vec, nil
}

func (e *countingEmbedder) EmbedDocuments(_ context.Context, titles, _ []string) ([][]float32, error) {
	out := make([][]float32, len(titles))
	for i := range out {
		out[i] = e.vec
	}
	return out, nil
}

func (e *countingEmbedder) EmbedShortStrings(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = e.vec
	}
	return out, nil
}

func (e *countingEmbedder) Dim() int                      { return len(e.vec) }
func (e *countingEmbedder) ID() string                    { return "counting-stub" }
func (e *countingEmbedder) Thresholds() params.Thresholds { return params.Defaults() }

func newCountingEmbedder() *countingEmbedder {
	return &countingEmbedder{vec: []float32{0.25, -0.5, 0.75}}
}

// A lens text search embeds the query ONCE no matter how many mounts it fans
// out to. This is the regression that made /lenses/{lens}/search 88% embedding
// on a 5-mount lens: the per-mount searchProvider embeds whenever QueryVec is
// empty, so the handler paid the same ~81 ms inference once per mount.
//
// The count is asserted against the MOUNT COUNT, not a literal 1, so the test
// still fails the day someone re-introduces a per-mount embed on a lens of a
// different size.
func TestLensSearch_EmbedsQueryOncePerRequest_NotPerMount(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta", "gamma")
	stub := &lensSearchStub{byRepo: map[string][]store.SearchResult{
		"alpha": {sr("kb/a/1.md", "Write fact", 9)},
		"beta":  {sr("kb/b/2.md", "Read fact", 8)},
		"gamma": {sr("kb/g/3.md", "Other fact", 7)},
	}}
	emb := newCountingEmbedder()
	s := &Server{Manager: m, Embedder: emb, providers: storeProviders{search: stub}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"},{"repo":"gamma"}]}`)

	body := decodeLensSearch(t, getLensFacts(t, r, "/lenses/eng/search?q=x"))

	const mounts = 3
	if len(stub.lastOpts) != mounts {
		t.Fatalf("fan-out reached %d mounts, want %d — the count below is only "+
			"meaningful against the full fan-out", len(stub.lastOpts), mounts)
	}
	if got := emb.queries.Load(); got != 1 {
		t.Errorf("EmbedQuery called %d times across %d mounts, want 1", got, mounts)
	}
	if len(body.Results) != mounts {
		t.Errorf("results: got %d, want %d", len(body.Results), mounts)
	}
}

// Every mount is handed the SAME vector — the one the handler embedded. This is
// what makes the hoist result-preserving rather than merely faster: if a future
// change made the vector mount-dependent (per-mount embedder, per-mount query
// rewriting), the mounts would no longer be ranking against a common query and
// RRF would be fusing incomparable lists.
func TestLensSearch_EveryMountGetsTheSameQueryVector(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta", "gamma")
	stub := &lensSearchStub{}
	emb := newCountingEmbedder()
	s := &Server{Manager: m, Embedder: emb, providers: storeProviders{search: stub}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"},{"repo":"gamma"}]}`)

	getLensFacts(t, r, "/lenses/eng/search?q=x")

	for _, repo := range []string{"alpha", "beta", "gamma"} {
		opts, ok := stub.lastOpts[repo]
		if !ok {
			t.Fatalf("mount %q never reached", repo)
		}
		if !float32SliceEqual(opts.QueryVec, emb.vec) {
			t.Errorf("mount %q got QueryVec %v, want the hoisted %v", repo, opts.QueryVec, emb.vec)
		}
	}
}

// The results a hoisted embed produces are the ones per-mount embedding
// produced. Driven by passing the SAME stub the same rows and comparing the
// wire bodies: the hoist is a pure move of where the vector is computed, so the
// fused order, the dedupe and the totals must all be untouched.
func TestLensSearch_HoistedEmbedMatchesPerMountEmbed(t *testing.T) {
	rows := map[string][]store.SearchResult{
		"alpha": {sr("kb/a/1.md", "A one", 3), sr("kb/shared.md", "Shadowed", 99)},
		"beta":  {sr("kb/shared.md", "Winner-shadowed copy", 50), sr("kb/b/2.md", "B two", 1)},
	}

	// ONE manager for both runs. Two managers would mint two sets of repos with
	// different root commits, and the id12 in every qualified path and source
	// would differ for reasons that have nothing to do with embedding.
	m, _ := newTestLensManager(t, "alpha", "beta")

	// Without a server embedder the handler cannot hoist and every mount is left
	// to embed for itself — the pre-hoist shape. With one, the vector is computed
	// once, up front. The wire body must not be able to tell the difference.
	perMount := &Server{Manager: m, providers: storeProviders{search: &lensSearchStub{byRepo: rows}}}
	r := perMount.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)
	withoutHoist := decodeLensSearch(t, getLensFacts(t, r, "/lenses/eng/search?q=x"))

	hoisted := &Server{Manager: m, Embedder: newCountingEmbedder(),
		providers: storeProviders{search: &lensSearchStub{byRepo: rows}}}
	withHoist := decodeLensSearch(t,
		getLensFacts(t, hoisted.NewAPIRouter(), "/lenses/eng/search?q=x"))

	if withHoist.Total != withoutHoist.Total {
		t.Errorf("total: hoisted %d, per-mount %d", withHoist.Total, withoutHoist.Total)
	}
	if !reflect.DeepEqual(withHoist.Results, withoutHoist.Results) {
		t.Errorf("result rows differ:\n hoisted   %+v\n per-mount %+v",
			withHoist.Results, withoutHoist.Results)
	}
}

// /lenses/{lens}/facts?q= is the SAME fan-out with the same defect — the
// Library search box, and the one that measured worst (474 ms on 5 mounts). A
// text-LESS browse must not reach an embedder at all.
func TestLensFacts_EmbedsQueryOncePerRequest_AndNotAtAllWithoutText(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
		want  int64
	}{
		{"with text", "/lenses/eng/facts?q=x", 1},
		{"no text", "/lenses/eng/facts", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newTestLensManager(t, "alpha", "beta", "gamma")
			stub := &lensFactsStub{}
			emb := newCountingEmbedder()
			s := &Server{Manager: m, Embedder: emb, providers: storeProviders{factsCollection: stub}}
			r := s.NewAPIRouter()
			createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"},{"repo":"gamma"}]}`)

			getLensFacts(t, r, tc.query)

			if got := emb.queries.Load(); got != tc.want {
				t.Errorf("EmbedQuery called %d times across 3 mounts, want %d", got, tc.want)
			}
		})
	}
}

func float32SliceEqual(a, b []float32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ── concurrent search fan-out: fusion order and §9.1 ────────────────────────

// A mount's INDEX is its identity to FuseRRF and WriteFirstWinners: rank fusion
// orders by per-mount rank, and the dedupe resolves a shared path in favour of
// the write mount and then binding order. Both read the per-mount lists as an
// indexed slice, so a concurrent fan-out that collected results as they arrived
// would silently re-map every row's mount — changing the fused order AND which
// copy of a duplicated path wins.
//
// The write mount is made the SLOWEST so it finishes last, and it shares a path
// with a read mount that finishes first. Correct behaviour: the write mount
// still wins that path, and the fused order is unchanged.
func TestLensSearch_FusionIsBindingOrderNotCompletionOrder(t *testing.T) {
	stub := &lensSearchStub{
		byRepo: map[string][]store.SearchResult{
			"alpha": {sr("kb/shared.md", "write copy", 1), sr("kb/a.md", "alpha only", 2)},
			"beta":  {sr("kb/shared.md", "read copy", 99), sr("kb/b.md", "beta only", 98)},
		},
		// alpha (the write mount, index 0) finishes last.
		delayByRepo: map[string]time.Duration{"alpha": 50 * time.Millisecond},
	}
	m, _ := newTestLensManager(t, "alpha", "beta")
	s := &Server{Manager: m, providers: storeProviders{search: stub}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	body := decodeLensSearch(t, getLensFacts(t, r, "/lenses/eng/search?q=x"))

	if len(body.Results) != 3 {
		t.Fatalf("results: got %d, want 3 (one shared path deduped); body=%+v",
			len(body.Results), body.Results)
	}
	for _, row := range body.Results {
		if row.Path == "kb/shared.md" && row.Title != "write copy" {
			t.Errorf("shared path resolved to %q, want the write mount's copy — the dedupe "+
				"read a mount index that completion order had reshuffled", row.Title)
		}
	}
	// The read mount's copy must not ALSO be present under a qualified path.
	for _, row := range body.Results {
		if strings.HasSuffix(row.Path, "/kb/shared.md") {
			t.Errorf("shadowed copy survived as %q — both copies were emitted", row.Path)
		}
	}
}

// §9.1 under concurrency on the search fan-out: one failing mount fails the
// whole request, even when the healthy mounts are still in flight.
func TestLensSearch_MountErrorFailsWholeRequestUnderConcurrency(t *testing.T) {
	stub := &lensSearchStub{
		byRepo:      map[string][]store.SearchResult{"alpha": {sr("kb/a.md", "alpha", 1)}},
		errRepo:     "beta",
		delayByRepo: map[string]time.Duration{"alpha": 40 * time.Millisecond},
	}
	m, _ := newTestLensManager(t, "alpha", "beta")
	s := &Server{Manager: m, providers: storeProviders{search: stub}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	rec := getLensFacts(t, r, "/lenses/eng/search?q=x")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500 — a lens must never silently shrink its read "+
			"set (RFC §9.1); body=%s", rec.Code, rec.Body.String())
	}
}
