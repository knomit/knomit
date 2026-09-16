package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/repos"
	"knomit/internal/store"
)

// The split itself, rule by rule, so each one is pinned independently of
// whether some HTTP path happens to exercise it.
func TestSplitGitRoute(t *testing.T) {
	for _, tc := range []struct{ in, repo, suffix string }{
		{"kb/info/refs", "kb", "info/refs"},
		{"kb/git-upload-pack", "kb", "git-upload-pack"},
		// The two tolerated forms.
		{"kb.git/info/refs", "kb", "info/refs"},
		{"kb//info/refs", "kb", "info/refs"},
		{"kb.git//info/refs", "kb", "info/refs"},
		// ...and where tolerance stops.
		{"kb.git.git/info/refs", "kb.git", "info/refs"},
		{"KB/info/refs", "KB", "info/refs"},
		{"kb///info/refs", "kb", "/info/refs"},
		{"kb.git.git.git/info/refs", "kb.git.git", "info/refs"},
		// ".git" alone names a repo called ".git", not an empty one: the
		// suffix is never stripped down to nothing.
		{".git/info/refs", ".git", "info/refs"},
		{"", "", ""},
		{"kb", "kb", ""},
	} {
		t.Run(tc.in, func(t *testing.T) {
			repo, suffix := splitGitRoute(tc.in)
			require.Equal(t, tc.repo, repo, "repo name")
			require.Equal(t, tc.suffix, suffix, "suffix")
		})
	}
}

// servedPeer is one knomit instance with a repo named "kb", mounted at the
// root of an httptest server the way /git mounts GitRemoteHandler.
func servedPeer(t *testing.T) *httptest.Server {
	t.Helper()
	ctx := context.Background()
	m := newPeerManager(t, "agent/a")
	ri, err := m.Create(ctx, repos.CreateSpec{Name: "kb", Mode: "preset"}, nil)
	require.NoError(t, err)
	require.NoError(t, ri.WithRead(func(s *store.Service) {
		_, werr := s.Facts().WriteFact(ctx, "agent/a", "kb/first.md",
			originFactBody("first"), "first", "")
		require.NoError(t, werr)
		_, rerr := s.AdvanceLocalUpstream(ctx, "agent/a", "main")
		require.NoError(t, rerr)
	}))
	srv := httptest.NewServer(GitRemoteHandler(m))
	t.Cleanup(srv.Close)
	return srv
}

// The two forms users type by habit resolve to the same repo. They FAIL FOR
// DIFFERENT REASONS, so each gets its own test rather than one "clone works"
// that could pass with only one path exercised:
//
//   - "<url>/kb.git" misses the OUTER lookup — the repo segment is read as
//     "kb.git" and rm.Get returns nil, so the request 404s before any store is
//     touched.
//   - "<url>/kb/" misses the INNER mux — the repo segment is "kb" and the
//     remainder is "/info/refs", which the handler rewrites to "//info/refs".
//
// Every git host accepts both. Tolerance stops exactly there: no general
// suffix stripping, no slash collapsing, no case folding. Those change which
// repository is named, and this endpoint is also how a subscriber identifies
// the knowledge base it follows.
func TestGitRemote_TolerantRepoForms(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	srv := servedPeer(t)

	for _, tc := range []struct{ name, path string }{
		{"plain", "/kb"},
		{"dot-git suffix", "/kb.git"},
		{"trailing slash", "/kb/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The advertisement, which is where each form fails today.
			resp, err := http.Get(srv.URL + tc.path + "/info/refs?service=git-upload-pack")
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, http.StatusOK, resp.StatusCode,
				"%s must resolve to the same repo", tc.path)

			// And a real clone end to end, so the whole exchange is exercised
			// and not just the first request.
			dst := filepath.Join(t.TempDir(), "c")
			out, cerr := exec.Command("git", "clone", "-q", srv.URL+tc.path, dst).CombinedOutput()
			require.NoError(t, cerr, "clone %s: %s", tc.path, out)
			require.FileExists(t, filepath.Join(dst, "kb/first.md"))
		})
	}
}

// Tolerance must not become a wildcard: an unknown repo still 404s in every
// form, and a name that only matches after some other mangling is not served.
func TestGitRemote_UnknownRepoStill404s(t *testing.T) {
	srv := servedPeer(t)

	for _, path := range []string{
		"/nope",
		"/nope.git",
		"/nope/",
		"/KB",         // no case folding
		"/kb.git.git", // only ONE suffix is tolerated
		"/kb//",       // not a general slash collapse
		"/",
	} {
		t.Run(path, func(t *testing.T) {
			resp, err := http.Get(srv.URL + path + "/info/refs?service=git-upload-pack")
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, http.StatusNotFound, resp.StatusCode,
				"%s must not resolve to a repo", path)
		})
	}
}
