package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knomit/internal/config"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// TestHandleCommit_SharedHistory_DoesNotSwapLocalStore pins the contract that
// when the test-connectivity step detects shared history with the remote, the
// commit step MUST NOT swap the local store with the cloned temp DB.
//
// The earlier implementation always swapped, which silently discarded any
// local-only facts (e.g. 209 local-only / 8 remote-only → after swap, the
// local store became the 69-fact remote clone, losing 201 facts). For shared
// history the local DB and remote already have a common ancestor, so the
// normal sync loop's pull/push primitives can reconcile them — no swap or
// rebuild is needed. The commit step must:
//
//   - keep the existing *store.Service (same pointer)
//   - write the "origin" row into the local store (through Manager.SetOrigin)
//   - call ActivateSync so the sync loop picks up the freshly-configured remote
func TestHandleCommit_SharedHistory_DoesNotSwapLocalStore(t *testing.T) {
	// Local store — a fresh DB that simulates the operator's existing repo.
	localDir := t.TempDir()
	localSvc, err := store.Open(filepath.Join(localDir, "local.db"))
	if err != nil {
		t.Fatalf("open local svc: %v", err)
	}
	t.Cleanup(func() { _ = localSvc.Close() })

	var activateCalled bool
	var activateURL string

	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:        "alpha",
		AgentBranch: "machine/test",
		Svc:         localSvc,
		StartSync: func(url string) error {
			activateCalled = true
			activateURL = url
			return nil
		},
	})

	// A STARTED manager: the credential now goes to control.db, so the commit
	// step needs a real registry (and the agent key that lets it encrypt).
	home := t.TempDir()
	keyPath := filepath.Join(home, "id_ed25519")
	if err := os.WriteFile(keyPath, []byte("test-key-material-for-hkdf"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	m := repos.New(context.Background(), repos.Deps{
		Cfg:                   config.Config{Home: home},
		AgentBranch:           "machine/test",
		KeyPath:               keyPath,
		DisableBackgroundSync: true,
	})
	if err := m.Start(); err != nil {
		t.Fatalf("manager start: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	m.Set("alpha", ri)

	sm := NewSessionManager()
	t.Cleanup(sm.Shutdown)

	const remoteURL = "https://example.com/repo.git"
	sess, err := sm.Create("alpha", remoteURL, AuthConfig{Method: "token", Token: "tok"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Cloned-remote store at <sess.TempDir>/clone.db with a real git repo
	// initialised. This shape is what /test produces and is exactly what the
	// buggy commit handler would happily swap on top of the local store. By
	// making OpenRepo succeed on the clone, we ensure SwapStore's in-memory
	// fallback would actually replace ri.svc rather than no-op — so the
	// pointer-equality check below is a real signal.
	remoteDBPath := filepath.Join(sess.TempDir, "clone.db")
	remoteSvc, err := store.Open(remoteDBPath)
	if err != nil {
		t.Fatalf("open remote svc: %v", err)
	}
	if err := remoteSvc.InitRepo(map[string]string{"seed.md": "seed"}, "machine/test"); err != nil {
		t.Fatalf("init remote git: %v", err)
	}

	sess.mu.Lock()
	sess.State = StateApplied
	sess.RemoteStore = remoteSvc
	sess.TestResult = connectivityResult{
		History:       "shared",
		DefaultBranch: "main",
		AgentBranches: []string{"machine/test"},
	}
	sess.RemoteBranch = "main"
	sess.AppliedBranch = "machine/test"
	sess.mu.Unlock()

	// Capture the local svc pointer before commit — if the handler swaps,
	// the after-pointer will differ.
	var svcBefore *store.Service
	ri.WithRead(func(s *store.Service) { svcBefore = s })

	s := &Server{Manager: m, SessionManager: sm, AgentBranch: "machine/test"}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/repos/alpha/origin-sessions/"+sess.ID+"/commit", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"phase":"done"`) {
		t.Errorf("expected a done phase in the SSE stream; body=%s", rec.Body.String())
	}

	// 1. The local svc must be the same instance — no swap.
	var svcAfter *store.Service
	ri.WithRead(func(s *store.Service) { svcAfter = s })
	if svcBefore != svcAfter {
		t.Error("local *store.Service was replaced — shared history must not swap")
	}

	// 2. Origin must be persisted on the local svc.
	var origin *store.Remote
	ri.WithRead(func(s *store.Service) {
		origin, err = s.Remote().GetRemote("origin")
	})
	if err != nil {
		t.Fatalf("GetRemote: %v", err)
	}
	if origin == nil {
		t.Fatal("origin row was not written to the local store")
	}
	if origin.URL != remoteURL {
		t.Errorf("origin URL: got %q, want %q", origin.URL, remoteURL)
	}
	// The store gets the wiring, never the credential: that lives only in
	// control.db, which is the copy that survives losing this database.
	if origin.AuthMethod != "" {
		t.Errorf("origin auth method in the store: got %q, want it empty", origin.AuthMethod)
	}
	if method, token, cerr := m.RepoRegistry().OriginCredential("alpha"); cerr != nil {
		t.Errorf("OriginCredential: %v", cerr)
	} else if method != "token" || token != "tok" {
		t.Errorf("control.db credential = (%q, %q), want (token, tok)", method, token)
	}
	if origin.Branch != "main" {
		t.Errorf("origin branch: got %q, want %q", origin.Branch, "main")
	}

	// 3. ActivateSync was called with the configured URL.
	if !activateCalled {
		t.Error("ActivateSync was not called")
	}
	if activateURL != remoteURL {
		t.Errorf("ActivateSync URL: got %q, want %q", activateURL, remoteURL)
	}
}
