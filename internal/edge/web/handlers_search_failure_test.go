package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/engine/store/index"
)

// A text search whose query cannot be embedded is a 500, never a 200 with an
// empty list. Driven through a real store whose embedder fails: once with the
// server embedder set (the handler embeds, as in production) and once without
// it, where the failure happens inside store.Search, which used to turn it into
// nil, nil.
func TestHandleSearch_EmbedderFailureIsServerError(t *testing.T) {
	for name, set := range map[string]func(*cosEmbedder){
		"error":        func(e *cosEmbedder) { e.failQuery.Store(true) },
		"empty vector": func(e *cosEmbedder) { e.emptyQuery.Store(true) },
	} {
		for _, serverEmbeds := range []bool{true, false} {
			label := name + " store-side embed"
			if serverEmbeds {
				label = name + " handler embed"
			}
			t.Run(label, func(t *testing.T) {
				s, emb := searchRepoServer(t)
				writeCosFact(t, s.Manager.Get("alpha"), "kb/a.md", "A fact", 0.9, nil)
				set(emb)
				if !serverEmbeds {
					s.Embedder = nil
				}
				rec := getAPI(t, s, alphaSearch+"?q=x")
				require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
				if name == "error" {
					// A failed embed is not retried on the other side. (An EMPTY
					// vector is not an error to searchRepo, so the store embeds
					// again and fails on its own: that case only pins the 500.)
					require.EqualValues(t, 1, emb.queries.Load(), "no retry of a failed embed")
				}
			})
		}
	}
}

// searchRepo hands the embedder's failure back as an error. Before, it logged
// and let the store re-embed (and the store then swallowed it).
func TestSearchRepo_EmbedErrorPropagates(t *testing.T) {
	emb := &cosEmbedder{}
	emb.failQuery.Store(true)
	m, _ := newRealStoreManagerWith(t, emb, "alpha")

	res, err := searchRepo(context.Background(), m.Get("alpha"), emb, "agent/test", index.SearchOptions{Text: "x"})

	require.Error(t, err, "want the embedder's error, got results=%v", res)
	require.ErrorIs(t, err, errInferenceDown)
	require.EqualValues(t, 1, emb.queries.Load(), "no store-side retry of a failed embed")
}

// A failing store read is a 500 that carries the cause. The injected error is
// distinct and only the Search read is failed, so the 500 cannot come from
// another read, and the hit counter proves the read was reached once.
func TestHandleSearch_StoreError_Returns500(t *testing.T) {
	s, _ := searchRepoServer(t)
	writeCosFact(t, s.Manager.Get("alpha"), "kb/a.md", "A fact", 0.9, nil)
	var hits atomic.Int64
	sentinel := errors.New("search read exploded: alpha")
	failMountRead(t, s.Manager, "alpha", index.ReadSearch, sentinel, &hits)

	rec := getAPI(t, s, alphaSearch+"?q=x")

	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), sentinel.Error())
	require.EqualValues(t, 1, hits.Load())
}

// The lens search fails ONCE, up front, when the shared query vector cannot be
// computed: one inference (not one per mount), a 500, and no mount is searched.
func TestLensSearch_EmbedFailureFailsOnceUpFront(t *testing.T) {
	for name, set := range map[string]func(*cosEmbedder){
		"error":        func(e *cosEmbedder) { e.failQuery.Store(true) },
		"empty vector": func(e *cosEmbedder) { e.emptyQuery.Store(true) },
	} {
		t.Run(name, func(t *testing.T) {
			emb := &cosEmbedder{}
			set(emb)
			m, _ := newTestLensManagerWith(t, emb, "alpha", "beta", "gamma")
			mounts := []string{"alpha", "beta", "gamma"}
			reads := countMountReads(t, m, mounts, "Search")
			s := &Server{Manager: m, Embedder: emb}
			r := s.NewAPIRouter()
			createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"},{"repo":"gamma"}]}`)

			rec := getLensFacts(t, r, "/lenses/eng/search?q=x")

			require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
			require.EqualValues(t, 1, emb.queries.Load(), "EmbedQuery across 3 mounts")
			for _, n := range mounts {
				require.EqualValues(t, 0, reads[n]["Search"].Load(), "mount %s was searched despite the failed embed", n)
			}

			// Control: once the embedder works, the same request searches every
			// mount, so the counters above could have moved.
			emb.failQuery.Store(false)
			emb.emptyQuery.Store(false)
			rec = getLensFacts(t, r, "/lenses/eng/search?q=x")
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			for _, n := range mounts {
				require.EqualValues(t, 1, reads[n]["Search"].Load(), "mount %s", n)
			}
		})
	}
}

// The facts twin of the test above; its mounts are still answered by the facts
// collection stub, so it keeps that wiring until the facts provider goes.
func TestLensFacts_EmbedFailureFailsOnceUpFront(t *testing.T) {
	for name, mode := range map[string]int32{"error": 1, "empty vector": 2} {
		t.Run(name, func(t *testing.T) {
			m, _ := newTestLensManager(t, "alpha", "beta", "gamma")
			emb := &cosEmbedder{}
			if mode == 1 {
				emb.failQuery.Store(true)
			} else {
				emb.emptyQuery.Store(true)
			}
			s := &Server{Manager: m, Embedder: emb, providers: storeProviders{factsCollection: &lensFactsStub{}}}
			r := s.NewAPIRouter()
			createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"},{"repo":"gamma"}]}`)

			rec := getLensFacts(t, r, "/lenses/eng/facts?q=x")
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status: %d, want 500; body=%s", rec.Code, rec.Body.String())
			}
			if got := emb.queries.Load(); got != 1 {
				t.Errorf("EmbedQuery called %d times across 3 mounts, want 1", got)
			}
		})
	}
}

// diagFactsProvider reports a fixed vector-step result through
// SearchOptions.Diag and returns no rows: a mount whose text query came back
// empty. It stands in for the facts collection only; search runs on real stores.
type diagFactsProvider struct{ d index.SearchDiag }

func (p diagFactsProvider) RecentFacts(_ context.Context, _ *repos.RepoInstance, _ string, q index.SearchOptions) ([]index.RecentFactEntry, int, error) {
	if q.Diag != nil {
		*q.Diag = p.d
	}
	return nil, 0, nil
}

// On the lens facts text read, an empty result caused by the caller's cutoff
// carries the notice (cutoff, best cosine); an empty result with another cause,
// or no caller cutoff, does not.
func TestLensFacts_CutoffNotice(t *testing.T) {
	emptied := index.SearchDiag{Text: true, VecHits: 9, BestCosine: 0.54, Cutoff: 0.7}
	keptThenFiltered := index.SearchDiag{Text: true, VecHits: 9, BestCosine: 0.54, Cutoff: 0.3, Candidates: 4}

	get := func(d index.SearchDiag, query string) map[string]any {
		m, _ := newTestLensManager(t, "alpha", "beta")
		s := &Server{Manager: m, providers: storeProviders{factsCollection: diagFactsProvider{d}}}
		r := s.NewAPIRouter()
		createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)
		rec := getLensFacts(t, r, "/lenses/eng/facts?q=x"+query)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d body=%s", rec.Code, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	notice, _ := get(emptied, "&min_similarity=0.7")["notice"].(string)
	if !strings.Contains(notice, "0.70") || !strings.Contains(notice, "0.54") {
		t.Errorf("notice should carry cutoff and best cosine, got %q", notice)
	}
	if _, ok := get(emptied, "")["notice"]; ok {
		t.Errorf("no caller cutoff, so no notice")
	}
	if _, ok := get(keptThenFiltered, "&min_similarity=0.3")["notice"]; ok {
		t.Errorf("the cutoff kept candidates (a later filter emptied it), so no notice")
	}
}

// On the lens search, an empty result caused by the caller's cutoff carries the
// notice (cutoff, best cosine); an empty result with another cause, or no
// caller cutoff, does not. The diagnosis is the real stores' own.
func TestLensSearch_CutoffNotice(t *testing.T) {
	get := func(cos float64, query string) map[string]any {
		emb := &cosEmbedder{}
		m, _ := newTestLensManagerWith(t, emb, "alpha", "beta")
		for _, n := range []string{"alpha", "beta"} {
			writeCosFact(t, m.Get(n), "kb/"+n+"/one.md", "A fact on "+n, cos, nil)
		}
		s := &Server{Manager: m, Embedder: emb}
		r := s.NewAPIRouter()
		createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)
		rec := getLensFacts(t, r, "/lenses/eng/search?q=x"+query)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var out map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		return out
	}

	// The caller's 0.7 removed every candidate (best cosine 0.54).
	out := get(0.54, "&min_similarity=0.7")
	require.Empty(t, out["results"])
	notice, _ := out["notice"].(string)
	require.Contains(t, notice, "0.70")
	require.Contains(t, notice, "0.54")

	// No caller cutoff: the model's own floor (0.40) emptied it, which is not
	// the caller's doing.
	out = get(0.3, "")
	require.Empty(t, out["results"])
	require.NotContains(t, out, "notice")

	// The cutoff kept both candidates; a later filter (an entity no fact has)
	// emptied the result, so the cutoff is not to blame.
	out = get(0.54, "&min_similarity=0.3&entities=ghost")
	require.Empty(t, out["results"])
	require.NotContains(t, out, "notice")
}
