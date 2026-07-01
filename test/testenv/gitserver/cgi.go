// Package gitserver provides an in-process, fault-injecting smart-HTTP git
// server backed by the real `git http-backend` CGI. Test-support only.
package gitserver

import (
	"net/http"
	"net/http/cgi"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitHTTPBackendPath resolves the absolute path to git-http-backend.
func gitHTTPBackendPath(t testing.TB) string {
	t.Helper()
	out, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Fatalf("git --exec-path: %v", err)
	}
	return filepath.Join(strings.TrimSpace(string(out)), "git-http-backend")
}

// newCGIHandler serves bare repos under projectRoot via git http-backend.
func newCGIHandler(t testing.TB, projectRoot string) http.Handler {
	t.Helper()
	return &cgi.Handler{
		Path: gitHTTPBackendPath(t),
		Dir:  projectRoot,
		Env: []string{
			"GIT_PROJECT_ROOT=" + projectRoot,
			"GIT_HTTP_EXPORT_ALL=1",
		},
	}
}
