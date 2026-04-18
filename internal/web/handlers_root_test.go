package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"knomit/internal/web/hal"
)

func TestHandleAPIRoot_LinksToReposAndSpec(t *testing.T) {
	s := &Server{}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != hal.ContentType {
		t.Errorf("content-type: got %q, want %q", got, hal.ContentType)
	}

	var body struct {
		Links hal.LinkMap `json:"_links"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for _, rel := range []string{"self", "repos", "openapi"} {
		if _, ok := body.Links[rel]; !ok {
			t.Errorf("missing link %q: %+v", rel, body.Links)
		}
	}
	if got := body.Links["repos"].Href; got != APIBase+"/repos" {
		t.Errorf("repos link: got %q", got)
	}
}
