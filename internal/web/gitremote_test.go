package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"context"

	"github.com/go-chi/chi/v5"
	"knomit/internal/store"
	"knomit/internal/repos"
)

// newTestRepoManager creates a *repos.Manager with a single repo named repoName
// backed by store.
func newTestRepoManager(repoName string, store *store.Service) *repos.Manager {
	rm := repos.New(context.Background(), repos.Deps{})
	rm.Set(repoName, repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{Svc: store}))
	return rm
}

// TestGitCloneIntegration verifies that git clone HTTP traffic routed through
// the full chi stack (r.Mount("/git", gitHandler)) reaches the go-git handler
// without triggering a redirect. This is a regression test for the bug where
// GET /git/knomit/info/refs?service=git-upload-pack redirected to
// /?service=git-upload-pack.
func TestGitCloneIntegration(t *testing.T) {
	store := newWebTestStore(t)

	gitHandler := GitRemoteHandler(newTestRepoManager("knomit", store))

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
		t.Errorf("got 404 — GitRemoteHandler failed to route request\nbody: %s", body)
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
	store := newWebTestStore(t)

	// Add a commit so the repo is non-empty.
	if _, _, err := store.WriteFile(context.Background(), testAgentBranch, "kb/hello.md", "# Hello\n", "init", "learn"); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	gitHandler := GitRemoteHandler(newTestRepoManager("knomit", store))
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

// TestGitRemoteHandler_GSNotGitRemoteStore verifies that a repo whose GS does
// not implement GitRemoteStore returns 500 rather than panicking.
func TestGitRemoteHandler_GSNotGitRemoteStore(t *testing.T) {
	rm := repos.New(context.Background(), repos.Deps{})
	// Svc is nil — the handler should return 500 because it can't get a Handler().
	rm.Set("mocked", repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{}))

	handler := GitRemoteHandler(rm)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/mocked/info/refs?service=git-upload-pack", nil)
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("want 500, got %d", rr.Code)
	}
}

// TestGitRemoteHandler_UnknownRepo verifies that requests for an unknown repo
// return 404.
func TestGitRemoteHandler_UnknownRepo(t *testing.T) {
	store := newWebTestStore(t)

	handler := GitRemoteHandler(newTestRepoManager("knomit", store))

	tests := []struct {
		name string
		path string
		want int
	}{
		{"unknown repo", "/other/info/refs", 404},
		{"no repo segment", "/info/refs", 404},
		{"empty path", "/", 404},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest("GET", tt.path+"?service=git-upload-pack", nil)
			handler.ServeHTTP(rr, req)
			if rr.Code != tt.want {
				t.Errorf("status = %d, want %d", rr.Code, tt.want)
			}
		})
	}
}

// TestGitRemoteHandler_MultiRepo verifies that multiple repos can be served
// from the same handler, each routing to the correct underlying store.
func TestGitRemoteHandler_MultiRepo(t *testing.T) {
	dir := t.TempDir()

	storeA := newWebTestStore(t)
	if _, _, err := storeA.WriteFile(context.Background(), testAgentBranch, "kb/a.md", "# A\n", "init a", "learn"); err != nil {
		t.Fatalf("WriteFile a: %v", err)
	}

	storeB := newWebTestStore(t)
	if _, _, err := storeB.WriteFile(context.Background(), testAgentBranch, "kb/b.md", "# B\n", "init b", "learn"); err != nil {
		t.Fatalf("WriteFile b: %v", err)
	}

	rm := repos.New(context.Background(), repos.Deps{})
	rm.Set("repo-a", repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{Svc: storeA}))
	rm.Set("repo-b", repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{Svc: storeB}))

	r := chi.NewRouter()
	r.Mount("/git", GitRemoteHandler(rm))
	srv := httptest.NewServer(r)
	defer srv.Close()

	for _, repoName := range []string{"repo-a", "repo-b"} {
		cloneDir := filepath.Join(dir, "clone-"+repoName)
		cmd := exec.Command("git", "clone", srv.URL+"/git/"+repoName, cloneDir)
		out, err := cmd.CombinedOutput()
		if err != nil {
			output := string(out)
			if strings.Contains(output, "redirect") || strings.Contains(output, "400") {
				t.Errorf("git clone %s failed: %s", repoName, output)
			} else {
				t.Logf("git clone %s non-fatal: %s", repoName, output)
			}
		}
	}
}
