package web

import (
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/engine/store/index"
)

// A lens text search embeds the query ONCE no matter how many mounts it fans
// out to. This is the regression that made /lenses/{lens}/search 88% embedding
// on a 5-mount lens: searchRepo embeds whenever QueryVec is empty, so the
// handler paid the same ~81 ms inference once per mount.
//
// The count is asserted against the MOUNT COUNT, not a literal 1, so the test
// still fails the day someone re-introduces a per-mount embed on a lens of a
// different size.
func TestLensSearch_EmbedsQueryOncePerRequest_NotPerMount(t *testing.T) {
	emb := &cosEmbedder{}
	mounts := []string{"alpha", "beta", "gamma"}
	m, _ := newTestLensManagerWith(t, emb, mounts...)
	writeCosFact(t, m.Get("alpha"), "kb/a/1.md", "Write fact", 0.9, nil)
	writeCosFact(t, m.Get("beta"), "kb/b/2.md", "Read fact", 0.8, nil)
	writeCosFact(t, m.Get("gamma"), "kb/g/3.md", "Other fact", 0.7, nil)
	reads := countMountReads(t, m, mounts, "Search")
	s := &Server{Manager: m, Embedder: emb}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"},{"repo":"gamma"}]}`)
	before := emb.queries.Load()

	body := decodeLensSearch(t, getLensFacts(t, r, "/lenses/eng/search?q=x"))

	// The count below is only meaningful against the full fan-out.
	for _, n := range mounts {
		require.EqualValues(t, 1, reads[n]["Search"].Load(), "mount %s must be searched exactly once", n)
	}
	require.EqualValues(t, 1, emb.queries.Load()-before,
		"EmbedQuery called more than once across %d mounts", len(mounts))
	require.Len(t, body.Results, len(mounts))
}

// Every mount is handed the SAME vector, the one the handler embedded. This is
// what makes the hoist result-preserving rather than merely faster: if a future
// change made the vector mount-dependent (per-mount embedder, per-mount query
// rewriting), the mounts would no longer be ranking against a common query and
// RRF would be fusing incomparable lists.
func TestLensSearch_EveryMountGetsTheSameQueryVector(t *testing.T) {
	emb := &cosEmbedder{}
	// Every inference answers a different vector, so equal vectors on the
	// mounts can only mean ONE inference was shared.
	emb.distinctQueries.Store(true)
	mounts := []string{"alpha", "beta", "gamma"}
	m, _ := newTestLensManagerWith(t, emb, mounts...)
	seen := observeMountSearch(t, m, mounts...)
	s := &Server{Manager: m, Embedder: emb}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"},{"repo":"gamma"}]}`)

	rec := getLensFacts(t, r, "/lenses/eng/search?q=x")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	first := seen.of("alpha")
	require.Len(t, first, 1, "mount alpha was not searched exactly once")
	require.NotEmpty(t, first[0].QueryVec)
	for _, n := range mounts {
		got := seen.of(n)
		require.Len(t, got, 1, "mount %s was not searched exactly once", n)
		require.Equal(t, first[0].QueryVec, got[0].QueryVec, "mount %s got a different QueryVec than the hoisted one", n)
	}
}

// Hoisting the embed does not perturb the fusion, the dedupe or the totals.
//
// Both arms run over the same REAL mounts, so this is evidence that a hoisted
// vector retrieves what N per-mount vectors would have: without a server
// embedder the handler cannot hoist and every mount embeds for itself (the
// pre-hoist shape); with one, the vector is computed once, up front. The wire
// body must not be able to tell the difference, while the inference count can.
func TestLensSearch_HoistedEmbedMatchesPerMountEmbed(t *testing.T) {
	emb := &cosEmbedder{}
	// ONE manager for both runs. Two managers would mint two sets of repos with
	// different root commits, and the id12 in every qualified path and source
	// would differ for reasons that have nothing to do with embedding.
	m, _ := newTestLensManagerWith(t, emb, "alpha", "beta")
	writeCosFact(t, m.Get("alpha"), "kb/a/1.md", "A one", 0.6, nil)
	writeCosFact(t, m.Get("alpha"), "kb/shared.md", "Shadowed", 0.9, nil)
	writeCosFact(t, m.Get("beta"), "kb/shared.md", "Winner-shadowed copy", 0.95, nil)
	writeCosFact(t, m.Get("beta"), "kb/b/2.md", "B two", 0.7, nil)

	perMount := &Server{Manager: m}
	r := perMount.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)
	before := emb.queries.Load()
	withoutHoist := decodeLensSearch(t, getLensFacts(t, r, "/lenses/eng/search?q=x"))
	perMountQueries := emb.queries.Load() - before

	hoisted := &Server{Manager: m, Embedder: emb}
	before = emb.queries.Load()
	withHoist := decodeLensSearch(t, getLensFacts(t, hoisted.NewAPIRouter(), "/lenses/eng/search?q=x"))
	hoistedQueries := emb.queries.Load() - before

	// The arms really differ in how they embed, and the corpus really answers.
	require.EqualValues(t, 2, perMountQueries, "without a server embedder each of the 2 mounts embeds")
	require.EqualValues(t, 1, hoistedQueries)
	require.Len(t, withoutHoist.Results, 3, "one shared path is deduped to the write mount's copy")
	require.Equal(t, withoutHoist.Total, withHoist.Total)
	require.Equal(t, withoutHoist.Results, withHoist.Results)
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
			env := searchLens(t, storeProviders{}, "alpha", "beta", "gamma")
			for _, n := range []string{"alpha", "beta", "gamma"} {
				writeCosFact(t, env.m.Get(n), "kb/"+n+".md", n+" fact", 0.9, nil)
			}
			reads := countMountReads(t, env.m, []string{"alpha", "beta", "gamma"}, "RecentFacts")
			before := env.emb.queries.Load()

			body := decodeLensFacts(t, env.get(t, tc.query))

			// Every mount was read and answered, so the inference count is
			// against the full fan-out.
			require.Len(t, body.Facts, 3)
			for _, n := range []string{"alpha", "beta", "gamma"} {
				require.EqualValues(t, 1, reads[n]["RecentFacts"].Load(), "mount %s", n)
			}
			require.Equal(t, tc.want, env.emb.queries.Load()-before, "EmbedQuery across 3 mounts")
		})
	}
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
	emb := &cosEmbedder{}
	m, _ := newTestLensManagerWith(t, emb, "alpha", "beta")
	writeCosFact(t, m.Get("alpha"), "kb/shared.md", "write copy", 0.9, nil)
	writeCosFact(t, m.Get("alpha"), "kb/a.md", "alpha only", 0.8, nil)
	writeCosFact(t, m.Get("beta"), "kb/shared.md", "read copy", 0.95, nil)
	writeCosFact(t, m.Get("beta"), "kb/b.md", "beta only", 0.85, nil)
	// alpha (the write mount, index 0) finishes last.
	slowMountRead(t, m, "alpha", "Search", 50*time.Millisecond)
	s := &Server{Manager: m, Embedder: emb}
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
	// RRF over per-mount ranks (alpha: shared, a; beta: shared, b): the two
	// rank-0 rows lead (shared resolved to the write copy), then the rank-1 rows
	// in mount order.
	var titles []string
	for _, row := range body.Results {
		titles = append(titles, row.Title)
	}
	require.Equal(t, []string{"write copy", "alpha only", "beta only"}, titles)
}

// §9.1 under concurrency on the search fan-out: one failing mount fails the
// whole request, even when the healthy mounts are still in flight.
func TestLensSearch_MountErrorFailsWholeRequestUnderConcurrency(t *testing.T) {
	emb := &cosEmbedder{}
	m, _ := newTestLensManagerWith(t, emb, "alpha", "beta")
	writeCosFact(t, m.Get("alpha"), "kb/a.md", "alpha", 0.9, nil)
	var hits atomic.Int64
	sentinel := errors.New("db on fire: beta")
	failMountRead(t, m, "beta", index.ReadSearch, sentinel, &hits)
	// The healthy mount is still in flight when beta fails.
	slowMountRead(t, m, "alpha", "Search", 40*time.Millisecond)
	s := &Server{Manager: m, Embedder: emb}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	rec := getLensFacts(t, r, "/lenses/eng/search?q=x")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500 — a lens must never silently shrink its read "+
			"set (RFC §9.1); body=%s", rec.Code, rec.Body.String())
	}
	require.Contains(t, rec.Body.String(), sentinel.Error())
	require.EqualValues(t, 1, hits.Load())
}
