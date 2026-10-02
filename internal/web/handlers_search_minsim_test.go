package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"knomit/internal/store"
)

// min_similarity is a raw cosine in [0, 1]. A value outside it (typically the
// displayed score, cosine×100, passed back) is rejected with a 400 that names the
// parameter and the range — never rescaled, never silently an empty list.
func TestHandleSearch_MinSimilarityOutOfRangeRejected(t *testing.T) {
	for _, v := range []string{"45", "1.5", "-0.1", "NaN"} {
		t.Run(v, func(t *testing.T) {
			provider := &stubSearchProvider{}
			s := &Server{Manager: newTestManagerWithRepos(t, "alpha"), providers: storeProviders{search: provider}}
			rec := httptest.NewRecorder()
			s.NewAPIRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
				"/repos/alpha/branches/agent:test/search?q=x&min_similarity="+v, nil))
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
		provider := &stubSearchProvider{}
		s := &Server{Manager: newTestManagerWithRepos(t, "alpha"), providers: storeProviders{search: provider}}
		rec := httptest.NewRecorder()
		s.NewAPIRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
			"/repos/alpha/branches/agent:test/search?q=x&min_similarity="+v, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("min_similarity=%s: status %d, want 200", v, rec.Code)
		}
	}
}

// The lens twins validate the same way.
func TestLensSearchAndFacts_MinSimilarityOutOfRange400(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha")
	s := &Server{Manager: m, providers: storeProviders{factsCollection: &lensFactsStub{}}}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[]}`)
	for _, path := range []string{"/lenses/eng/facts?q=x&min_similarity=45", "/lenses/eng/search?q=x&min_similarity=45"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "between 0 and 1") {
			t.Errorf("%s: got %d, want 400 naming the range; body=%s", path, rec.Code, rec.Body.String())
		}
	}
}

// When the caller's own cutoff removed every candidate, the empty response says
// so (best cosine, cutoff) instead of looking like "nothing matched". Without a
// caller cutoff, or with a different cause for emptiness, there is no notice.
func TestHandleSearch_CutoffExplainsEmptyResult(t *testing.T) {
	run := func(url string, d *store.SearchDiag) map[string]any {
		provider := &stubSearchProvider{diag: d}
		s := &Server{Manager: newTestManagerWithRepos(t, "alpha"), providers: storeProviders{search: provider}}
		rec := httptest.NewRecorder()
		s.NewAPIRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status: %d, body=%s", rec.Code, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	emptied := &store.SearchDiag{Text: true, VecHits: 12, BestCosine: 0.54, Cutoff: 0.7, Candidates: 0}

	out := run("/repos/alpha/branches/agent:test/search?q=x&min_similarity=0.7", emptied)
	notice, _ := out["notice"].(string)
	if !strings.Contains(notice, "0.70") || !strings.Contains(notice, "0.54") {
		t.Errorf("notice should carry the cutoff and best cosine, got %q", notice)
	}
	if _, ok := run("/repos/alpha/branches/agent:test/search?q=x", emptied)["notice"]; ok {
		t.Error("no caller cutoff: the model's own floor must not produce a notice")
	}
	none := &store.SearchDiag{Text: true, VecHits: 0, Cutoff: 0.7}
	if _, ok := run("/repos/alpha/branches/agent:test/search?q=x&min_similarity=0.7", none)["notice"]; ok {
		t.Error("nothing matched at all: no cutoff notice")
	}
}
