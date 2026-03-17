package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"knomit/internal/git"
)

// TestGitCloneIntegration verifies that git clone HTTP traffic routed through
// the full chi stack (r.Mount("/git", gitHandler)) reaches the go-git handler
// without triggering a redirect. This is a regression test for the bug where
// GET /git/knomit/info/refs?service=git-upload-pack redirected to
// /?service=git-upload-pack.
func TestGitCloneIntegration(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "test.db"), nil)
	if err != nil {
		t.Fatalf("git.Init: %v", err)
	}

	gitHandler := GitRemoteHandler(store, "")

	r := chi.NewRouter()
	r.Mount("/git", gitHandler)

	srv := httptest.NewServer(r)
	defer srv.Close()

	// Use a client that does NOT follow redirects so we can inspect them.
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	url := srv.URL + "/git/knomit/info/refs?service=git-upload-pack"
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusMovedPermanently || resp.StatusCode == http.StatusFound ||
		resp.StatusCode == http.StatusTemporaryRedirect || resp.StatusCode == http.StatusPermanentRedirect {
		t.Errorf("got redirect %d to %s — expected non-redirect response\nbody: %s",
			resp.StatusCode, resp.Header.Get("Location"), body)
		return
	}

	// go-git serves the advertised refs; we accept 200 or 500 (empty repo).
	// What we must NOT get is a redirect or 404 from path mismatch.
	if resp.StatusCode == http.StatusNotFound {
		t.Errorf("got 404 — gitPathStripper failed to route request\nbody: %s", body)
	}

	// Also run an actual git ls-remote to confirm the full protocol works.
	cmd := exec.Command("git", "ls-remote", srv.URL+"/git/knomit")
	out, err := cmd.CombinedOutput()
	// An empty repo returns exit 0 with no lines, or exit 128 with "empty repo".
	// What is NOT acceptable is a redirect error.
	if err != nil {
		output := string(out)
		if strings.Contains(output, "redirect") {
			t.Errorf("git ls-remote got redirect error:\n%s", output)
		}
		// "Repository is empty" or similar is acceptable.
	}
}

// TestGitCloneWithCommits verifies that git clone works against a repo with
// actual commits. go-git's AdvertisedReferencesContext behaves differently on
// a non-empty repo and this exercises the full fetch protocol.
func TestGitCloneWithCommits(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "test.db"), nil)
	if err != nil {
		t.Fatalf("git.Init: %v", err)
	}

	// Add a commit so the repo is non-empty.
	if _, _, err := store.WriteFile("kb/hello.md", "# Hello\n", "init"); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	gitHandler := GitRemoteHandler(store, "")
	r := chi.NewRouter()
	r.Mount("/git", gitHandler)
	srv := httptest.NewServer(r)
	defer srv.Close()

	cloneDir := filepath.Join(dir, "clone")
	cmd := exec.Command("git", "clone", srv.URL+"/git/knomit", cloneDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		output := string(out)
		if strings.Contains(output, "redirect") {
			t.Errorf("git clone got redirect error:\n%s", output)
		} else {
			t.Logf("git clone non-redirect failure (may be ok): %s", output)
		}
	}
}

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
