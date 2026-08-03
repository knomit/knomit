package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"knomit/internal/config"
	"knomit/internal/repos"
)

// The origin HAL handlers are the only way a user changes a repo's origin after
// creation, and control.db is the only record of that origin once the repo's
// own database is gone. These tests drive the REAL provider — not the stub the
// sibling tests use — because the thing under test is precisely that the store
// mutation and the registry write happen together.
//
// They exist as wiring guards. The write-through's semantics are pinned in
// internal/repos (origin_writethrough_test.go); what can rot HERE is someone
// deleting the one-line m.RecordOrigin call from a handler, which no test that
// stubs the provider could ever notice.

// newRegistryBackedServer returns a Server over a real started Manager holding
// one real repo, so the default origin provider has an actual store to write to
// and the manager has an actual control.db to write through into.
func newRegistryBackedServer(t *testing.T, repo string) (*Server, *repos.Manager) {
	t.Helper()
	home := t.TempDir()
	m := repos.New(context.Background(), repos.Deps{
		Cfg:                   config.Config{Home: home},
		AgentBranch:           "machine/test",
		DisableBackgroundSync: true,
	})
	if err := m.Start(); err != nil {
		t.Fatalf("manager start: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })

	initRepoFile(t, home, repo)
	if _, err := m.Rescan(); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	if m.Get(repo) == nil {
		t.Fatalf("repo %q did not open", repo)
	}
	return &Server{Manager: m, AgentBranch: "machine/test"}, m
}

func activeOrigin(t *testing.T, m *repos.Manager, repo string) repos.RepoRecord {
	t.Helper()
	rec, ok, err := m.RepoRegistry().ActiveRecord(repo)
	if err != nil {
		t.Fatalf("ActiveRecord(%q): %v", repo, err)
	}
	if !ok {
		t.Fatalf("no active registry row for %q", repo)
	}
	return rec
}

// TestSetOriginReachesTheRegistry covers PUT /repos/{repo}/origin.
//
// The 502 is the point, not an accident of the fixture. ActivateSync runs an
// immediate reconcile against the URL, which fails here because the URL is not
// a real remote — and the handler correctly reports that. The origin IS
// persisted by then, so the registry write has to happen BEFORE the activation
// that can bail out. Placing it after would skip the write-through for exactly
// the origins whose first sync failed, which are the ones most likely to still
// be sitting there unreachable when the repo later needs rebuilding.
func TestSetOriginReachesTheRegistry(t *testing.T) {
	s, m := newRegistryBackedServer(t, "work")
	r := s.NewAPIRouter()

	if got := activeOrigin(t, m, "work").OriginURL; got != "" {
		t.Fatalf("precondition: origin already recorded as %q", got)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/repos/work/origin",
		strings.NewReader(`{"url":"https://example.invalid/acme/kb.git","branch":"master"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK && rec.Code != http.StatusBadGateway {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}

	got := activeOrigin(t, m, "work")
	if got.OriginURL != "https://example.invalid/acme/kb.git" {
		t.Errorf("control.db origin = %q, want the URL just configured", got.OriginURL)
	}
	if got.OriginBranch != "master" {
		t.Errorf("control.db upstream = %q, want master", got.OriginBranch)
	}
}

// TestSetOriginUpstreamReachesTheRegistry covers PATCH
// /repos/{repo}/origin/upstream. The branch is half of what the registry
// records: a repo re-pinned to a release branch and later rebuilt from a row
// still naming "main" fetches a refspec the remote may not have.
func TestSetOriginUpstreamReachesTheRegistry(t *testing.T) {
	s, m := newRegistryBackedServer(t, "work")
	r := s.NewAPIRouter()

	put := httptest.NewRecorder()
	r.ServeHTTP(put, httptest.NewRequest(http.MethodPut, "/repos/work/origin",
		strings.NewReader(`{"url":"https://example.invalid/acme/kb.git","branch":"main"}`)))
	if got := activeOrigin(t, m, "work").OriginBranch; got != "main" {
		t.Fatalf("precondition: upstream = %q, want main", got)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/repos/work/origin/upstream",
		strings.NewReader(`{"branch":"release-2"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}

	if got := activeOrigin(t, m, "work").OriginBranch; got != "release-2" {
		t.Errorf("control.db upstream = %q, want release-2", got)
	}
}

// TestDeleteOriginClearsTheRegistry covers DELETE /repos/{repo}/origin, which
// is the direction a "never overwrite with a blank" rule silently gets wrong.
//
// A registry that kept the URL the user just disconnected would have the next
// boot re-clone this repo from a remote they deliberately detached — using
// credentials they may well have revoked in the same breath.
func TestDeleteOriginClearsTheRegistry(t *testing.T) {
	s, m := newRegistryBackedServer(t, "work")
	r := s.NewAPIRouter()

	put := httptest.NewRecorder()
	r.ServeHTTP(put, httptest.NewRequest(http.MethodPut, "/repos/work/origin",
		strings.NewReader(`{"url":"https://example.invalid/acme/kb.git","branch":"main"}`)))
	if got := activeOrigin(t, m, "work").OriginURL; got == "" {
		t.Fatal("precondition: origin was never recorded, so clearing it proves nothing")
	}

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/repos/work/origin", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}

	got := activeOrigin(t, m, "work")
	if got.OriginURL != "" {
		t.Errorf("control.db still holds origin %q after a disconnect", got.OriginURL)
	}
	if got.OriginBranch != "" {
		t.Errorf("control.db still holds upstream %q after a disconnect", got.OriginBranch)
	}
}
