package web

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"knomit/internal/config"
	"knomit/internal/repos"
)

func newRealManager(t *testing.T) *repos.Manager {
	t.Helper()
	m := repos.New(context.Background(), repos.Deps{
		Cfg:         config.Config{Home: t.TempDir()},
		AgentBranch: "machine/test",
	})
	if err := m.Start(); err != nil {
		t.Fatalf("start manager: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// newRealManagerWithLocalOriginRoot is newRealManager with LocalOriginRoot set
// to root, so a file:// origin under it clears the local-origin policy gate.
// newRealManager itself must keep no root configured — other tests depend on
// filesystem origins being disabled by default — so this is a separate helper
// rather than a parameter added to it.
func newRealManagerWithLocalOriginRoot(t *testing.T, root string) *repos.Manager {
	t.Helper()
	m := repos.New(context.Background(), repos.Deps{
		Cfg:         config.Config{Home: t.TempDir(), LocalOriginRoot: root},
		AgentBranch: "machine/test",
	})
	if err := m.Start(); err != nil {
		t.Fatalf("start manager: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func TestPostRepos_StreamsNDJSONAndCreates(t *testing.T) {
	s := &Server{Manager: newRealManager(t)}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/repos",
		strings.NewReader(`{"name":"work","mode":"preset","ontology_preset":"default"}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/x-ndjson") {
		t.Fatalf("content-type = %q", ct)
	}
	var lastType string
	sc := bufio.NewScanner(strings.NewReader(rec.Body.String()))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("bad ndjson line %q: %v", line, err)
		}
		lastType, _ = m["type"].(string)
	}
	if lastType != "done" {
		t.Fatalf("last type = %q, want done", lastType)
	}
	if s.Manager.Get("work") == nil {
		t.Fatal("repo not registered")
	}
}

func TestPostRepos_ConflictOnExistingName(t *testing.T) {
	s := &Server{Manager: newRealManager(t)}
	r := s.NewAPIRouter()
	createViaAPI(t, r, "work")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/repos",
		strings.NewReader(`{"name":"work","mode":"preset","ontology_preset":"default"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
}

func TestPostRepos_BadNameReturns400(t *testing.T) {
	s := &Server{Manager: newRealManager(t)}
	r := s.NewAPIRouter()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/repos",
		strings.NewReader(`{"name":"Bad Name","mode":"preset"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
