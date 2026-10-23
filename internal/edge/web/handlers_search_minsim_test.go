package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// min_similarity is a raw cosine in [0, 1]. A value outside it (typically the
// displayed score, cosine×100, passed back) is rejected with a 400 that names the
// parameter and the range — never rescaled, never silently an empty list.
func TestHandleSearch_MinSimilarityOutOfRangeRejected(t *testing.T) {
	for _, v := range []string{"45", "1.5", "-0.1", "NaN"} {
		t.Run(v, func(t *testing.T) {
			s, _ := searchRepoServer(t)
			rec := getAPI(t, s, alphaSearch+"?q=x&min_similarity="+v)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status: %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			if !strings.Contains(body, "min_similarity") || !strings.Contains(body, "between 0 and 1") {
				t.Errorf("error should name the parameter and range: %s", body)
			}
		})
	}
	// The boundaries are valid.
	for _, v := range []string{"0", "1", "0.5"} {
		s, _ := searchRepoServer(t)
		rec := getAPI(t, s, alphaSearch+"?q=x&min_similarity="+v)
		if rec.Code != http.StatusOK {
			t.Errorf("min_similarity=%s: status %d, want 200", v, rec.Code)
		}
	}
}

// The lens twins validate the same way. Both run on real mounts.
func TestLensSearch_MinSimilarityOutOfRange400(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha")
	s := &Server{Manager: m}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[]}`)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/lenses/eng/search?q=x&min_similarity=45", nil))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "between 0 and 1") {
		t.Errorf("got %d, want 400 naming the range; body=%s", rec.Code, rec.Body.String())
	}
}

func TestLensFacts_MinSimilarityOutOfRange400(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha")
	s := &Server{Manager: m}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[]}`)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/lenses/eng/facts?q=x&min_similarity=45", nil))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "between 0 and 1") {
		t.Errorf("got %d, want 400 naming the range; body=%s", rec.Code, rec.Body.String())
	}
}

// When the caller's own cutoff removed every candidate, the empty response says
// so (best cosine, cutoff) instead of looking like "nothing matched". Without a
// caller cutoff, or with a different cause for emptiness, there is no notice.
// The diagnosis is the real store's own.
func TestHandleSearch_CutoffExplainsEmptyResult(t *testing.T) {
	// One fact whose cosine with the query is exactly cos, or none at all.
	run := func(cos float64, url string, writeFact bool) string {
		s, _ := searchRepoServer(t)
		if writeFact {
			writeCosFact(t, s.Manager.Get("alpha"), "kb/a.md", "A fact", cos, nil)
		}
		body := decodeSearch(t, getAPI(t, s, url))
		require.Empty(t, body.Embedded.Results, "the case is an EMPTY result")
		return body.Notice
	}

	// The caller's 0.7 removed the only candidate (best cosine 0.54).
	notice := run(0.54, alphaSearch+"?q=x&min_similarity=0.7", true)
	if !strings.Contains(notice, "0.70") || !strings.Contains(notice, "0.54") {
		t.Errorf("notice should carry the cutoff and best cosine, got %q", notice)
	}
	// No caller cutoff: the model's own floor emptied it, which must not
	// produce a notice.
	if got := run(0.3, alphaSearch+"?q=x", true); got != "" {
		t.Errorf("no caller cutoff: the model's own floor must not produce a notice, got %q", got)
	}
	// Nothing in the store at all (no vector hit): no cutoff notice.
	if got := run(0, alphaSearch+"?q=x&min_similarity=0.7", false); got != "" {
		t.Errorf("nothing matched at all: no cutoff notice, got %q", got)
	}
}
