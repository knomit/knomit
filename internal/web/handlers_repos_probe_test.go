package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initBareRepoForWebTest creates an empty bare git repo under parent and
// returns a file:// URL to it.
func initBareRepoForWebTest(t *testing.T, parent string) string {
	t.Helper()
	dir := filepath.Join(parent, "remote.git")
	if err := exec.Command("git", "init", "--bare", dir).Run(); err != nil {
		t.Fatalf("git init --bare: %v", err)
	}
	return "file://" + dir
}

func TestProbeOrigin_ReportsEmptyRemote(t *testing.T) {
	root := t.TempDir()
	s := &Server{Manager: newRealManagerWithLocalOriginRoot(t, root)}
	r := s.NewAPIRouter()
	dir := initBareRepoForWebTest(t, root)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/repos:probe-origin",
		strings.NewReader(`{"url":"`+dir+`"}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Reachable bool `json:"reachable"`
		Empty     bool `json:"empty"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if !got.Reachable || !got.Empty {
		t.Fatalf("got %+v, want reachable+empty", got)
	}
}

func TestProbeOrigin_MalformedBodyIs400(t *testing.T) {
	s := &Server{Manager: newRealManager(t)}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/repos:probe-origin",
		strings.NewReader(`not json`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
