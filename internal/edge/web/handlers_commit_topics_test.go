package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleCommitAnchoredTopicNode_Returns501(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{"root topics", "/repos/alpha/branches/main/commits/abc123/topics"},
		{"topic node", "/repos/alpha/branches/main/commits/abc123/topics/ai"},
		{"topic facts", "/repos/alpha/branches/main/commits/abc123/topics/ai/facts"},
		{"topic stats", "/repos/alpha/branches/main/commits/abc123/topics/ai/stats"},
		{"nested", "/repos/alpha/branches/main/commits/abc123/topics/ai/ml/facts"},
	}

	s := &Server{Manager: newTestManagerWithRepos(t, "alpha")}
	r := s.NewAPIRouter()

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			r.ServeHTTP(rec, req)

			if rec.Code != http.StatusNotImplemented {
				t.Errorf("status: got %d, want 501, body=%s", rec.Code, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
				t.Errorf("Content-Type: got %q, want application/problem+json", ct)
			}
		})
	}
}

func TestHandleCommitAnchoredTopicNode_UnknownRepo_Returns404(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t)}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/missing/branches/main/commits/abc/topics/ai", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: %d, want 404", rec.Code)
	}
}
