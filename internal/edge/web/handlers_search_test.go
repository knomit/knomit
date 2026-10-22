package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/domain/fact"
	"knomit/internal/edge/web/hal"
)

// searchWire mirrors the repo /search wire shape.
type searchWire struct {
	Count    int         `json:"count"`
	Notice   string      `json:"notice"`
	Links    hal.LinkMap `json:"_links"`
	Embedded struct {
		Results []struct {
			Path       string      `json:"path"`
			Title      string      `json:"title"`
			Score      float64     `json:"score"`
			Kind       string      `json:"kind"`
			Type       string      `json:"type"`
			Domain     []string    `json:"domain"`
			Entities   []string    `json:"entities"`
			Motifs     []string    `json:"motifs"`
			Confidence float64     `json:"confidence"`
			Body       *string     `json:"body,omitempty"`
			Links      hal.LinkMap `json:"_links"`
		} `json:"results"`
	} `json:"_embedded"`
}

// paths is the result paths, in served order.
func (w searchWire) paths() []string {
	out := make([]string, 0, len(w.Embedded.Results))
	for _, r := range w.Embedded.Results {
		out = append(out, r.Path)
	}
	return out
}

// searchRepoServer is a Server over one real repo, "alpha", whose store embeds
// with a fresh cosEmbedder (also the Server's embedder, as in production).
func searchRepoServer(t *testing.T) (*Server, *cosEmbedder) {
	t.Helper()
	emb := &cosEmbedder{}
	m, _ := newRealStoreManagerWith(t, emb, "alpha")
	return &Server{Manager: m, Embedder: emb}, emb
}

func decodeSearch(t *testing.T, rec *httptest.ResponseRecorder) searchWire {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body searchWire
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	return body
}

const alphaSearch = "/repos/alpha/branches/agent:test/search"

func TestHandleSearch_ReturnsHALCollection(t *testing.T) {
	s, emb := searchRepoServer(t)
	ri := s.Manager.Get("alpha")
	writeCosFact(t, ri, "kb/ai/ml/abc123.md", "Attention Is All You Need", 0.9, func(f *fact.Fact) {
		f.Domain = []string{"ai", "ml"}
		f.Entities = []string{"transformer"}
	})
	writeCosFact(t, ri, "kb/ai/nlp/def456.md", "BERT Overview", 0.8, func(f *fact.Fact) {
		f.Domain = []string{"ai"}
		f.Entities = []string{"bert"}
		f.Confidence = 0.8
	})
	// Below the model's recall floor (0.40): a search must not serve it.
	writeCosFact(t, ri, "kb/ai/other/zzz999.md", "Unrelated", 0.2, nil)

	rec := getAPI(t, s, alphaSearch+"?q=attention&limit=10")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	if got := rec.Header().Get("Content-Type"); got != hal.ContentType {
		t.Errorf("content-type: got %q, want %q", got, hal.ContentType)
	}
	var body searchWire
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))

	require.Equal(t, 2, body.Count)
	require.Contains(t, body.Links, "self")
	require.Equal(t, []string{"kb/ai/ml/abc123.md", "kb/ai/nlp/def456.md"}, body.paths())

	item := body.Embedded.Results[0]
	require.Equal(t, "Attention Is All You Need", item.Title)
	require.InDelta(t, 90, item.Score, 0.01)
	require.InDelta(t, 80, body.Embedded.Results[1].Score, 0.01)
	require.Equal(t, "observation", item.Type)
	require.Equal(t, []string{"ai", "ml"}, item.Domain)
	require.Equal(t, []string{"transformer"}, item.Entities)
	require.Nil(t, item.Body, "body must be omitted from collection items")
	wantSelf := APIBase + "/repos/alpha/branches/agent:test/facts/kb/ai/ml/abc123.md"
	require.Equal(t, wantSelf, item.Links["self"].Href)

	// The query text reached the embedder, and limit reached the store.
	require.Equal(t, "attention", emb.lastQueryText())
	one := decodeSearch(t, getAPI(t, s, alphaSearch+"?q=attention&limit=1"))
	require.Equal(t, []string{"kb/ai/ml/abc123.md"}, one.paths())
}

func TestHandleSearch_EmptyResults(t *testing.T) {
	s, _ := searchRepoServer(t)
	// One fact, but below the recall floor: nothing matches.
	writeCosFact(t, s.Manager.Get("alpha"), "kb/a/one.md", "Nothing like the query", 0.1, nil)

	rec := getAPI(t, s, alphaSearch+"?q=nothing")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	if got := rec.Header().Get("Content-Type"); got != hal.ContentType {
		t.Errorf("content-type: got %q, want %q", got, hal.ContentType)
	}
	var body struct {
		Count    int `json:"count"`
		Embedded struct {
			Results []any `json:"results"`
		} `json:"_embedded"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, 0, body.Count)
	require.NotNil(t, body.Embedded.Results, "results must be [] not null")
	require.Empty(t, body.Embedded.Results)
}

// ?kind= / ?exclude_kind= on /search narrow the real result set. Regression:
// the filter was silently dropped, so a UI chip like "kind:pragmatic" returned
// every fact instead of pragmatic ones. Each param is driven on its own, so
// neither can pass on the other's strength.
func TestHandleSearch_KindFilterNarrowsTheResults(t *testing.T) {
	s, _ := searchRepoServer(t)
	ri := s.Manager.Get("alpha")
	writeCosFact(t, ri, "kb/e.md", "An observation", 0.9, nil)
	writeCosFact(t, ri, "kb/p.md", "A policy", 0.9, func(f *fact.Fact) {
		f.Kind = fact.Pragmatic
		f.Type = fact.Policy
	})

	all := decodeSearch(t, getAPI(t, s, alphaSearch+"?q=x"))
	got := all.paths()
	sort.Strings(got)
	require.Equal(t, []string{"kb/e.md", "kb/p.md"}, got, "unfiltered, both kinds answer")

	require.Equal(t, []string{"kb/p.md"}, decodeSearch(t, getAPI(t, s, alphaSearch+"?q=x&kind=pragmatic")).paths())
	require.Equal(t, []string{"kb/p.md"}, decodeSearch(t, getAPI(t, s, alphaSearch+"?q=x&exclude_kind=epistemic")).paths())
	require.Equal(t, []string{"kb/e.md"}, decodeSearch(t, getAPI(t, s, alphaSearch+"?q=x&exclude_kind=pragmatic")).paths())
	require.Empty(t, decodeSearch(t, getAPI(t, s, alphaSearch+"?q=x&kind=pragmatic&exclude_kind=pragmatic")).paths())
}

func TestHandleSearch_UnknownRepo_Returns404(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t)}

	rec := getAPI(t, s, "/repos/missing/branches/main/search?q=x")

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: got %q, want application/problem+json", got)
	}
}

// min_similarity and domain_exact on /search narrow the real result set: REST
// and MCP parity.
func TestHandleSearch_MinSimilarityAndDomainExactNarrowTheResults(t *testing.T) {
	s, _ := searchRepoServer(t)
	ri := s.Manager.Get("alpha")
	writeCosFact(t, ri, "kb/a.md", "Exactly ai, close", 0.9, func(f *fact.Fact) { f.Domain = []string{"ai"} })
	writeCosFact(t, ri, "kb/b.md", "Exactly ai, far", 0.5, func(f *fact.Fact) { f.Domain = []string{"ai"} })
	writeCosFact(t, ri, "kb/c.md", "ai safety, close", 0.9, func(f *fact.Fact) { f.Domain = []string{"ai safety"} })

	sorted := func(url string) []string {
		p := decodeSearch(t, getAPI(t, s, url)).paths()
		sort.Strings(p)
		return p
	}
	// The model's own floor (0.40) lets all three in.
	require.Equal(t, []string{"kb/a.md", "kb/b.md", "kb/c.md"}, sorted(alphaSearch+"?q=x"))
	// The caller's cutoff (0.55) removes the 0.5 one.
	require.Equal(t, []string{"kb/a.md", "kb/c.md"}, sorted(alphaSearch+"?q=x&min_similarity=0.55"))
	// domain=ai alone also matches "ai safety"; domain_exact=true does not.
	require.Equal(t, []string{"kb/a.md", "kb/b.md", "kb/c.md"}, sorted(alphaSearch+"?q=x&domain=ai"))
	require.Equal(t, []string{"kb/a.md", "kb/b.md"}, sorted(alphaSearch+"?q=x&domain=ai&domain_exact=true"))
	// Both together.
	require.Equal(t, []string{"kb/a.md"}, sorted(alphaSearch+"?q=x&min_similarity=0.55&domain=ai&domain_exact=true"))
}

// ?motifs= / ?motif_match= on /search narrow the real result set: the motif
// axis is only queryable if the params survive the handler.
func TestHandleSearch_MotifFilterNarrowsTheResults(t *testing.T) {
	s, _ := searchRepoServer(t)
	ri := s.Manager.Get("alpha")
	motifs := func(m ...string) func(*fact.Fact) { return func(f *fact.Fact) { f.Motifs = m } }
	writeCosFact(t, ri, "kb/a.md", "zero value", 0.9, motifs("zero-value-as-valid"))
	writeCosFact(t, ri, "kb/b.md", "fallback", 0.9, motifs("silent-fallback"))
	writeCosFact(t, ri, "kb/c.md", "plural fallback", 0.9, motifs("silent-fallbacks"))
	writeCosFact(t, ri, "kb/d.md", "unrelated", 0.9, motifs("something-else"))

	sorted := func(url string) []string {
		p := decodeSearch(t, getAPI(t, s, url)).paths()
		sort.Strings(p)
		return p
	}
	q := alphaSearch + "?q=x&motifs=zero-value-as-valid,silent-fallback"
	require.Equal(t, []string{"kb/a.md", "kb/b.md"}, sorted(q+"&motif_match=exact"))
	// The stem tier also reaches the plural spelling, so the param is read.
	require.Equal(t, []string{"kb/a.md", "kb/b.md", "kb/c.md"}, sorted(q+"&motif_match=stem"))
}

// The tiers this surface refuses are refused at the edge, before any store
// call (C3/MN6). The Search read counter proves no store call happened, and a
// permitted tier on the same corpus proves the counter would have moved.
func TestHandleSearch_LooseMotifTierRejected(t *testing.T) {
	s, _ := searchRepoServer(t)
	counts := countMountReads(t, s.Manager, []string{"alpha"}, "Search")["alpha"]

	rec := getAPI(t, s, alphaSearch+"?motifs=a-b&motif_match=token-1")
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.EqualValues(t, 0, counts["Search"].Load(), "the store was searched despite the rejection")

	rec = getAPI(t, s, alphaSearch+"?motifs=a-b&motif_match=stem")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.EqualValues(t, 1, counts["Search"].Load(), "a permitted tier reaches the store exactly once")
}

// Rows carry their motifs, and a motif-free row serializes with no motifs key
// at all (byte-compat for corpora that never ran a motif pass).
func TestHandleSearch_MotifsOnTheWire(t *testing.T) {
	s, _ := searchRepoServer(t)
	ri := s.Manager.Get("alpha")
	writeCosFact(t, ri, "kb/a.md", "Carries a motif", 0.9, func(f *fact.Fact) { f.Motifs = []string{"a-b"} })
	writeCosFact(t, ri, "kb/b.md", "Carries none", 0.8, nil)

	rec := getAPI(t, s, alphaSearch+"?q=x")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var view struct {
		Embedded struct {
			Results []map[string]any `json:"results"`
		} `json:"_embedded"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &view))
	require.Len(t, view.Embedded.Results, 2)
	require.Equal(t, "kb/a.md", view.Embedded.Results[0]["path"])
	require.Equal(t, []any{"a-b"}, view.Embedded.Results[0]["motifs"])
	_, present := view.Embedded.Results[1]["motifs"]
	require.False(t, present, "motif-free row must omit the key entirely, body=%s", rec.Body.String())
}
