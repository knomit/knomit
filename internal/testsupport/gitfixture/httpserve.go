package gitfixture

import (
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os/exec"
	"sync"
	"testing"
)

// HTTPRemote serves the bare repositories under a root directory over git's
// smart HTTP protocol (`git http-backend`), requiring HTTP Basic auth with
// one user and password. A request with any other credential, or none, is
// 401 and is recorded, so a test can prove which credential a client sent.
type HTTPRemote struct {
	// URL is the server's base URL (http://127.0.0.1:<port>); a repository
	// at <root>/<name> is URL + "/" + name.
	URL string

	mu       sync.Mutex
	user     string
	password string
	ok       int
	refused  []string // the passwords of refused requests ("" for none)
}

// ServeHTTP starts an HTTPRemote over root that accepts user:password and
// closes it when the test ends. Pushes are enabled.
func ServeHTTP(t testing.TB, root, user, password string) *HTTPRemote {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git not found: %v", err)
	}
	backend := &cgi.Handler{
		Path: gitPath,
		Args: []string{"http-backend"},
		Env: append(isolatedEnv(),
			"GIT_PROJECT_ROOT="+root,
			"GIT_HTTP_EXPORT_ALL=1",
			// receive-pack is enabled for an authenticated REMOTE_USER.
			"REMOTE_USER="+user,
		),
	}
	r := &HTTPRemote{user: user, password: password}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		u, p, ok := req.BasicAuth()
		r.mu.Lock()
		good := ok && u == r.user && p == r.password
		if good {
			r.ok++
		} else {
			r.refused = append(r.refused, p)
		}
		r.mu.Unlock()
		if !good {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, req)
	}))
	t.Cleanup(srv.Close)
	r.URL = srv.URL
	return r
}

// SetPassword changes the password the server accepts from now on.
func (r *HTTPRemote) SetPassword(p string) {
	r.mu.Lock()
	r.password = p
	r.mu.Unlock()
}

// Accepted is how many requests carried the accepted credential.
func (r *HTTPRemote) Accepted() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ok
}

// Refused returns the passwords of the refused requests ("" for a request
// with no credential), oldest first.
func (r *HTTPRemote) Refused() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.refused...)
}
