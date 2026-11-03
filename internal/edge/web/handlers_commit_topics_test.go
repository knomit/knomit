package web

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// The commit-anchored topic routes are not implemented: 501 at a commit the
// repo holds. (An unknown commit is 404 commit_not_in_repo first; see
// TestCommitAnchoredRoutes_UnknownCommitIs404CommitNotInRepo.)
func TestHandleCommitAnchoredTopicNode_Returns501(t *testing.T) {
	m, svcs := newRealStoreManager(t, "alpha")
	res, err := svcs["alpha"].Facts().WriteFact(t.Context(), "agent/test", "kb/ai/a.md",
		commitsFactText("A"), "add a", "learn")
	require.NoError(t, err)
	base := "/repos/alpha/branches/agent:test/commits/" + res.CommitHash

	tests := []struct {
		name string
		path string
	}{
		{"root topics", base + "/topics"},
		{"topic node", base + "/topics/ai"},
		{"topic facts", base + "/topics/ai/facts"},
		{"topic stats", base + "/topics/ai/stats"},
		{"nested", base + "/topics/ai/ml/facts"},
	}

	s := &Server{Manager: m}
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
