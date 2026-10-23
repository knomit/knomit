package web

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/engine/store/index"
)

// The lens facts text read fails ONCE, up front, when the shared query vector
// cannot be computed: one inference (not one per mount), a 500, and no mount is
// read. The counters are the positive control: once the embedder works, the
// same request reads every mount.
func TestLensFacts_EmbedFailureFailsOnceUpFront(t *testing.T) {
	for name, set := range map[string]func(*cosEmbedder){
		"error":        func(e *cosEmbedder) { e.failQuery.Store(true) },
		"empty vector": func(e *cosEmbedder) { e.emptyQuery.Store(true) },
	} {
		t.Run(name, func(t *testing.T) {
			mounts := []string{"alpha", "beta", "gamma"}
			env := searchLens(t, storeProviders{}, "alpha", "beta", "gamma")
			for _, n := range mounts {
				writeCosFact(t, env.m.Get(n), "kb/"+n+".md", n+" fact", 0.9, nil)
			}
			reads := countMountReads(t, env.m, mounts, "RecentFacts")
			set(env.emb)
			before := env.emb.queries.Load()

			rec := env.get(t, "/lenses/eng/facts?q=x")

			require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
			require.EqualValues(t, 1, env.emb.queries.Load()-before, "EmbedQuery across 3 mounts")
			for _, n := range mounts {
				require.EqualValues(t, 0, reads[n]["RecentFacts"].Load(), "mount %s was read despite the failed embed", n)
			}

			env.emb.failQuery.Store(false)
			env.emb.emptyQuery.Store(false)
			rec = env.get(t, "/lenses/eng/facts?q=x")
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			for _, n := range mounts {
				require.EqualValues(t, 1, reads[n]["RecentFacts"].Load(), "mount %s", n)
			}
		})
	}
}

// searchRepo hands an embedder's EMPTY vector (with a nil error) back as an
// error, exactly like an embed error: leaving QueryVec empty would make the
// store embed the same text again.
func TestSearchRepo_EmptyVectorIsAnError(t *testing.T) {
	emb := &cosEmbedder{}
	emb.emptyQuery.Store(true)
	m, _ := newRealStoreManagerWith(t, emb, "alpha")

	res, err := searchRepo(context.Background(), m.Get("alpha"), emb, "agent/test", index.SearchOptions{Text: "x"})

	require.Error(t, err, "got results=%v", res)
	require.Contains(t, err.Error(), "empty vector")
	require.EqualValues(t, 1, emb.queries.Load(), "no store-side retry of an unusable embed")
}

// On the lens facts text read, an empty result caused by the caller's cutoff
// carries the notice (cutoff, best cosine); an empty result with another cause,
// or no caller cutoff, does not. The diagnosis is the real stores' own.
func TestLensFacts_CutoffNotice(t *testing.T) {
	get := func(cos float64, query string) map[string]any {
		env := searchLens(t, storeProviders{}, "alpha", "beta")
		for _, n := range []string{"alpha", "beta"} {
			writeCosFact(t, env.m.Get(n), "kb/"+n+"/one.md", "A fact on "+n, cos, nil)
		}
		rec := env.get(t, "/lenses/eng/facts?q=x"+query)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var out map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		return out
	}

	// The caller's 0.7 removed every candidate (best cosine 0.54).
	out := get(0.54, "&min_similarity=0.7")
	require.Empty(t, out["facts"])
	notice, _ := out["notice"].(string)
	require.Contains(t, notice, "0.70")
	require.Contains(t, notice, "0.54")

	// No caller cutoff: the model's own floor (0.40) emptied it, which is not
	// the caller's doing.
	out = get(0.3, "")
	require.Empty(t, out["facts"])
	require.NotContains(t, out, "notice")

	// The cutoff kept both candidates; a later filter (an entity no fact has)
	// emptied the result, so the cutoff is not to blame.
	out = get(0.54, "&min_similarity=0.3&entities=ghost")
	require.Empty(t, out["facts"])
	require.NotContains(t, out, "notice")
}
