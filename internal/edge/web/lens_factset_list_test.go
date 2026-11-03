package web

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/federate"
	"knomit/internal/corpus/materialize"
	"knomit/internal/corpus/repos"
	"knomit/internal/engine/store"
	"knomit/internal/foundation/auth"
)

// lensFactSetRoots builds the engine's lens FactSet for search on lens over
// manager m (Depth 0, every result) and returns its unit roots, in order, as
// wire paths, and each root's repo_name.
func lensFactSetRoots(t *testing.T, m *repos.Manager, lens string, search materialize.FactSetSearch, embed func(context.Context, string) ([]float32, error)) (roots, names []string, units int) {
	t.Helper()
	ctx, err := repos.ResolveLensBinding(auth.WithPrincipal(context.Background(), auth.Principal{Kind: auth.KindHost, ID: "t"}), m, lens)
	require.NoError(t, err)
	b := repos.BindingFromContext(ctx)
	require.True(t, b.IsLens())
	var sc materialize.CursorScope
	require.NoError(t, b.Write().WithRead(func(svc *store.Service) {
		sc = materialize.CursorScope{Sessions: svc.ToolSession(), Branch: b.WriteMountBranch(), Pin: b.PinID(),
			ReadSet: federate.ReadSetFingerprint(b), Owner: "o"}
	}))
	p, _, err := materialize.FactSetFirstPage(ctx, sc, materialize.FactSetRequest{
		Binding: b, Manager: m, Search: search,
		Facts: materialize.DefaultFactSetFacts, Options: materialize.Options{Depth: 0, MaxNodes: 10},
		Limit: materialize.HumanMaxLimit, Embed: embed,
	})
	require.NoError(t, err)
	units = p.Units
	for {
		for _, n := range p.Facts {
			if n.Number == strconv.Itoa(n.Unit) {
				roots = append(roots, n.Path)
				names = append(names, n.RepoName)
			}
		}
		if !p.HasMore {
			return roots, names, units
		}
		p, err = materialize.FactSetNextPage(ctx, sc, b, m, nil, *p.Cursor, 0)
		require.NoError(t, err)
	}
}

// A lens FactSet's roots ARE the REST lens list (design section 2, item 1 of
// section 6). Three mounts of 25 text-searchable facts each (75 results, so
// the 50-row list is a strict prefix): the FactSet built by the engine's lens
// facade on the lens binding, for the same query, has 75 units, and its
// first 50 unit roots are /lenses/{lens}/search's 50 results, path for path,
// in order (both read every mount at depth 500 and fuse with the same
// federate function); the whole root list is fused, so no mount's results
// come in one block.
//
// Sabotage (the facade searches the write mount only: planLensSearch keeps
// plan.Targets[:1]): 25 units, and the comparison fails at the second root.
func TestLensFactSet_FirstFiftyRootsAreTheRESTLensList(t *testing.T) {
	emb := &cosEmbedder{}
	mounts := []string{"alpha", "beta", "gamma"}
	m, _ := newTestLensManagerWith(t, emb, mounts...)
	for k, name := range mounts {
		for i := 0; i < 25; i++ {
			cos := 0.95 - 0.02*float64(i) - 0.003*float64(k)
			writeCosFact(t, m.Get(name), fmt.Sprintf("kb/%s/f%02d.md", name[:1], i), name+" "+strconv.Itoa(i), cos, nil)
		}
	}
	r := (&Server{Manager: m, Embedder: emb}).NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"},{"repo":"gamma"}]}`)
	rest := decodeLensSearch(t, getLensFacts(t, r, "/lenses/eng/search?q=x"))
	require.Len(t, rest.Results, materialize.UIListLimit)

	roots, names, units := lensFactSetRoots(t, m, "eng", materialize.FactSetSearch{Text: "x", Sort: materialize.SortRelevance}, emb.EmbedQuery)
	require.Equal(t, 75, units, "every mount's 25 results")
	require.Len(t, roots, 75)
	want := make([]string, len(rest.Results))
	for i, res := range rest.Results {
		want[i] = res.Path
	}
	require.Equal(t, want, roots[:materialize.UIListLimit], "the first 50 roots are the REST lens list, in order")
	require.Equal(t, []string{"alpha", "beta", "gamma", "alpha", "beta", "gamma"}, names[:6], "fused, not one mount after another")
}

// sort=recent with no text (review M2): the lens FactSet's first 50 roots are
// REST /lenses/{lens}/facts' first page, including when a re-rooted fork
// beside its upstream spends a mount's whole depth on duplicates. beta is a
// fork of alpha: alpha commits 510 facts; then beta commits 5 facts of its
// own; then beta re-commits all 510 of alpha's paths. beta's copies are its
// 510 newest rows and all lose the write-first dedupe, so a single round at
// depth 500 sees none of beta's own 5 facts, which are the 5 newest rows of
// the deduped union. Both surfaces share federate.FusedRecentDeep's
// merge-horizon deepening, so both put beta's 5 facts first.
//
// Sabotage (FusedRecentDeep never deepens: always returns the first round):
// the FactSet's first roots are alpha's facts and the comparison fails (REST
// too would fail TestLensFacts_DedupedDuplicatesDoNotEatTheDepth).
func TestLensFactSet_RecentFirstFiftyAreTheRESTFactsList(t *testing.T) {
	t.Parallel() // waits on commit stamps (awaitNextSecond)
	env := searchLens(t, "alpha", "beta")
	shared := make([]factSpec, 510)
	for i := range shared {
		shared[i] = factSpec{path: fmt.Sprintf("kb/s/%03d.md", i), title: "shared " + strconv.Itoa(i)}
	}
	writeFactsInOneCommit(t, env.m.Get("alpha"), shared...)
	awaitNextSecond()
	var own []factSpec
	for i := 0; i < 5; i++ {
		own = append(own, factSpec{path: fmt.Sprintf("kb/own/%d.md", i), title: "own " + strconv.Itoa(i)})
	}
	writeFactsInOneCommit(t, env.m.Get("beta"), own...)
	awaitNextSecond()
	copies := make([]factSpec, len(shared))
	for i, s := range shared {
		copies[i] = factSpec{path: s.path, title: "fork " + strconv.Itoa(i)}
	}
	writeFactsInOneCommit(t, env.m.Get("beta"), copies...)

	rest := decodeLensFacts(t, env.get(t, "/lenses/eng/facts?limit=50"))
	require.Len(t, rest.Facts, 50)
	want := make([]string, len(rest.Facts))
	for i, f := range rest.Facts {
		want[i] = f.Path
	}
	roots, names, units := lensFactSetRoots(t, env.m, "eng", materialize.FactSetSearch{Sort: materialize.SortRecent}, nil)
	require.Equal(t, materialize.MaxFactSetFacts, units)
	require.Equal(t, want, roots[:50], "the first 50 roots are the REST /facts list, in order")
	for i := 0; i < 5; i++ {
		require.Equal(t, "beta", names[i], "the fork's own facts are the newest rows of the union: root %d", i+1)
	}
	require.Equal(t, "alpha", names[5], "then the upstream's copies win the shared paths")
}

// The lens relevance list pages by offset like a repo's, and ENDS WHERE THE
// LENS FACTSET ENDS (user report 2026-11-02, the "all" lens: "Matches 50",
// no scrolling past it, a 500-unit factset). Two mounts of 300 text hits
// each fuse to 600 rows, more than a factset holds: /lenses/{lens}/search
// pages of 50 at offset 0, 50, ... are contiguous and disjoint, their union
// is the one-call list at limit 500 (the fused list, in order), `total` is
// that list's length (500) on every page, the page at offset 500 is empty,
// and the lens factset of the same search has 500 units whose roots are the
// list, page after page.
//
// Sabotage: drop the offset (every page is page 1) and the pages overlap;
// drop the cut to materialize.MaxFactSetFacts and the list runs to 600 rows
// while the factset stops at 500.
func TestLensSearch_OffsetPagesEndWhereTheFactSetEnds(t *testing.T) {
	emb := &cosEmbedder{}
	mounts := []string{"alpha", "beta"}
	m, _ := newTestLensManagerWith(t, emb, mounts...)
	for k, name := range mounts {
		specs := make([]factSpec, 300)
		for i := range specs {
			specs[i] = factSpec{
				path:  fmt.Sprintf("kb/%s/f%03d.md", name[:1], i),
				title: name + " " + strconv.Itoa(i),
				cos:   0.95 - 0.001*float64(i) - 0.0003*float64(k),
			}
		}
		writeFactsInOneCommit(t, m.Get(name), specs...)
	}
	r := (&Server{Manager: m, Embedder: emb}).NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	paths := func(b lensSearchBody) []string {
		out := make([]string, len(b.Results))
		for i, res := range b.Results {
			out[i] = res.Path
		}
		return out
	}
	full := decodeLensSearch(t, getLensFacts(t, r, "/lenses/eng/search?q=x&limit=500"))
	require.Equal(t, materialize.MaxFactSetFacts, full.Total, "the fused 600 are cut to the factset's ceiling")
	require.Len(t, full.Results, materialize.MaxFactSetFacts)

	var list []string
	seen := map[string]bool{}
	pages := 0
	for {
		pages++
		page := decodeLensSearch(t, getLensFacts(t, r, fmt.Sprintf("/lenses/eng/search?q=x&limit=%d&offset=%d", materialize.UIListLimit, len(list))))
		require.Equal(t, materialize.MaxFactSetFacts, page.Total, "total is the list's length on every page")
		for _, p := range paths(page) {
			require.False(t, seen[p], "page %d repeats %s: pages overlap", pages, p)
			seen[p] = true
		}
		list = append(list, paths(page)...)
		if len(page.Results) < materialize.UIListLimit {
			break
		}
		require.Less(t, pages, 100, "paging does not end")
	}
	require.Equal(t, 11, pages, "ten full pages, then an empty last one")
	require.Equal(t, paths(full), list, "the pages are the fused list, contiguous")

	roots, _, units := lensFactSetRoots(t, m, "eng", materialize.FactSetSearch{Text: "x", Sort: materialize.SortRelevance}, emb.EmbedQuery)
	require.Equal(t, materialize.MaxFactSetFacts, units)
	require.Equal(t, list, roots, "the list's last page ends at the factset's last unit")

	rec := getLensFacts(t, r, "/lenses/eng/search?q=x&offset=-1")
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}
