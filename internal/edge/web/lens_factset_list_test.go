package web

import (
	"context"
	"fmt"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/federate"
	"knomit/internal/corpus/materialize"
	"knomit/internal/corpus/repos"
	"knomit/internal/engine/store"
	"knomit/internal/foundation/auth"
)

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

	ctx, err := repos.ResolveLensBinding(auth.WithPrincipal(context.Background(), auth.Principal{Kind: auth.KindHost, ID: "t"}), m, "eng")
	require.NoError(t, err)
	b := repos.BindingFromContext(ctx)
	require.True(t, b.IsLens())
	var sc materialize.CursorScope
	require.NoError(t, b.Write().WithRead(func(svc *store.Service) {
		sc = materialize.CursorScope{Sessions: svc.ToolSession(), Branch: b.WriteMountBranch(), Pin: b.PinID(),
			ReadSet: federate.ReadSetFingerprint(b), Owner: "o"}
	}))
	p, _, err := materialize.FactSetFirstPage(ctx, sc, materialize.FactSetRequest{
		Binding: b, Manager: m, Search: materialize.FactSetSearch{Text: "x", Sort: materialize.SortRelevance},
		Facts: materialize.DefaultFactSetFacts, Options: materialize.Options{Depth: 0, MaxNodes: 10},
		Limit: materialize.HumanMaxLimit, Embed: emb.EmbedQuery,
	})
	require.NoError(t, err)
	require.Equal(t, 75, p.Units, "every mount's 25 results")
	var roots, repos3 []string
	for {
		for _, n := range p.Facts {
			if n.Number == strconv.Itoa(n.Unit) {
				roots = append(roots, n.Path)
				repos3 = append(repos3, n.RepoName)
			}
		}
		if !p.HasMore {
			break
		}
		p, err = materialize.FactSetNextPage(ctx, sc, b, m, nil, *p.Cursor, 0)
		require.NoError(t, err)
	}
	require.Len(t, roots, 75)
	want := make([]string, len(rest.Results))
	for i, res := range rest.Results {
		want[i] = res.Path
	}
	require.Equal(t, want, roots[:materialize.UIListLimit], "the first 50 roots are the REST lens list, in order")
	require.Equal(t, []string{"alpha", "beta", "gamma", "alpha", "beta", "gamma"}, repos3[:6], "fused, not one mount after another")
}
