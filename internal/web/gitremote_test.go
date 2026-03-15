package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestGitRemoteHandler_RepoPrefix verifies that requests with a repo-name
// prefix in the path (e.g. /knomit/info/refs) are correctly routed to the
// git smart HTTP endpoints. This is a regression test for a bug where
// git clone http://host/git/knomit failed because the repo-name segment
// was not stripped, causing a redirect loop.
func TestGitRemoteHandler_RepoPrefix(t *testing.T) {
	// Build a test mux with the same endpoints as the real handler.
	mux := http.NewServeMux()
	mux.HandleFunc("/info/refs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("refs"))
	})
	mux.HandleFunc("/git-upload-pack", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("upload"))
	})
	mux.HandleFunc("/git-receive-pack", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("receive"))
	})

	// Wrap with the same path-stripping logic used by GitRemoteHandler.
	handler := gitPathStripper(mux)

	tests := []struct {
		name   string
		path   string
		want   int
		body   string
	}{
		{"info/refs with repo prefix", "/knomit/info/refs", 200, "refs"},
		{"info/refs with nested repo prefix", "/org/repo/info/refs", 200, "refs"},
		{"git-upload-pack with repo prefix", "/myrepo/git-upload-pack", 200, "upload"},
		{"git-receive-pack with repo prefix", "/myrepo/git-receive-pack", 200, "receive"},
		{"no matching suffix", "/knomit/unknown", 404, ""},
		{"bare path no suffix", "/knomit", 404, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest("GET", tt.path, nil)
			handler.ServeHTTP(rr, req)
			if rr.Code != tt.want {
				t.Errorf("status = %d, want %d", rr.Code, tt.want)
			}
			if tt.body != "" && rr.Body.String() != tt.body {
				t.Errorf("body = %q, want %q", rr.Body.String(), tt.body)
			}
		})
	}
}
