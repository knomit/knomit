package web

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/federate"
)

// ?path= is a prefix filter on a search, and it reaches the store. The corpus
// puts the BEST match outside the prefix, so a search that dropped the path
// would lead with it, and a nested prefix narrows further still.
func TestHandleSearch_PathNarrowsTheResults(t *testing.T) {
	s, _ := searchRepoServer(t)
	ri := s.Manager.Get("alpha")
	writeCosFact(t, ri, "kb/x/a.md", "Under x", 0.9, nil)
	writeCosFact(t, ri, "kb/x/sub/b.md", "Deeper under x", 0.8, nil)
	writeCosFact(t, ri, "kb/y/c.md", "Under y", 0.95, nil)

	require.Equal(t, []string{"kb/y/c.md", "kb/x/a.md", "kb/x/sub/b.md"},
		decodeSearch(t, getAPI(t, s, alphaSearch+"?q=x")).paths(), "control: unfiltered, best first")
	require.Equal(t, []string{"kb/x/a.md", "kb/x/sub/b.md"},
		decodeSearch(t, getAPI(t, s, alphaSearch+"?q=x&path=kb/x/")).paths())
	require.Equal(t, []string{"kb/x/sub/b.md"},
		decodeSearch(t, getAPI(t, s, alphaSearch+"?q=x&path=kb/x/sub/")).paths())
}

// A lens search applies an UNQUALIFIED ?path= to every mount in that mount's
// own terms, and a kb://<id12>/-qualified one to the single mount it names,
// made repo-relative. Each mount holds a best match outside the prefix. The
// topics are ones the preset ontology carries: an unqualified path under a topic
// a mount's ontology lacks skips that mount.
func TestLensSearch_PathNarrowsEveryMount(t *testing.T) {
	env := searchLens(t, storeProviders{}, "alpha", "beta")
	writeCosFact(t, env.m.Get("alpha"), "kb/technology/a.md", "alpha x", 0.8, nil)
	writeCosFact(t, env.m.Get("alpha"), "kb/science/a.md", "alpha y", 0.95, nil)
	writeCosFact(t, env.m.Get("beta"), "kb/technology/b.md", "beta x", 0.7, nil)
	writeCosFact(t, env.m.Get("beta"), "kb/science/b.md", "beta y", 0.9, nil)
	titles := func(url string) []string {
		var out []string
		for _, r := range decodeLensSearch(t, env.get(t, url)).Results {
			out = append(out, r.Title)
		}
		sort.Strings(out)
		return out
	}

	require.Equal(t, []string{"alpha x", "alpha y", "beta x", "beta y"}, titles("/lenses/eng/search?q=x"), "control: unfiltered")
	require.Equal(t, []string{"alpha x", "beta x"}, titles("/lenses/eng/search?q=x&path=kb/technology/"))
	// Qualified: only beta answers, under its own repo-relative prefix.
	qualified := federate.QualifyPath(federate.ID12(env.m.Get("beta").ID()), "kb/technology/")
	require.Equal(t, []string{"beta x"}, titles("/lenses/eng/search?q=x&path="+qualified))
}
