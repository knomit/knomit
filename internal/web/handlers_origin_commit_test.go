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

// newDisjointSession builds a manager + in-memory repo instance + an applied
// disjoint-history session whose RemoteStore is a real cloned git repo at
// <TempDir>/clone.db — the exact shape handleCommit's swap path consumes.
// keyPath (may be "") becomes the manager's agent key, controlling whether the
// registry gets a Crypt — and thus whether the commit can record the session's
// token in control.db at all.
func newDisjointSession(t *testing.T, keyPath string) (*Server, *repos.RepoInstance, *SessionManager, *OriginSession, string) {
	t.Helper()
	const remoteURL = "https://example.com/repo.git"

	localSvc, err := store.Open(filepath.Join(t.TempDir(), "local.db"))
	if err != nil {
		t.Fatalf("open local svc: %v", err)
	}
	t.Cleanup(func() { _ = localSvc.Close() })

	var activateURL string
	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:        "alpha",
		AgentBranch: "machine/test",
		Svc:         localSvc,
		StartSync:   func(url string) error { activateURL = url; return nil },
	})
	_ = activateURL

	// A STARTED manager: the credential now goes to control.db, so the commit
	// step needs a real registry to write into.
	m := repos.New(context.Background(), repos.Deps{
		Cfg:                   config.Config{Home: t.TempDir()},
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

	sess, err := sm.Create("alpha", remoteURL, AuthConfig{Method: "token", Token: "tok"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// The cloned remote the swap will consume.
	remoteSvc, err := store.Open(filepath.Join(sess.TempDir, "clone.db"))
	if err != nil {
		t.Fatalf("open remote svc: %v", err)
	}
	if err := remoteSvc.InitRepo(map[string]string{"seed.md": "seed"}, "machine/test"); err != nil {
		t.Fatalf("init remote git: %v", err)
	}
	// handleCommit closes remoteSvc; don't double-close it here.

	sess.mu.Lock()
	sess.State = StateApplied
	sess.RemoteStore = remoteSvc
	sess.TestResult = connectivityResult{History: "disjoint", DefaultBranch: "main"}
	sess.RemoteBranch = "main"
	sess.AppliedBranch = "machine/test"
	sess.mu.Unlock()

	s := &Server{Manager: m, SessionManager: sm, AgentBranch: "machine/test"}
	return s, ri, sm, sess, remoteURL
}

func postCommit(t *testing.T, s *Server, sessID string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/repos/alpha/origin-sessions/"+sessID+"/commit", nil)
	s.NewAPIRouter().ServeHTTP(rec, req)
	return rec
}

// TestHandleCommit_Disjoint_PostSwapConfigFailureStillCompletes pins Change 2:
// once the swap has happened the local store IS the merged result (point of no
// return), so a failure to persist remote config must NOT abort into a
// retryable half-done state. With no agent key the REGISTRY has no Crypt, so
// recording the session's token in control.db is refused (never plaintext) and
// SetOrigin fails — the commit must still reach "done" (carrying a non-fatal
// warning) and delete the session, not error out.
func TestHandleCommit_Disjoint_PostSwapConfigFailureStillCompletes(t *testing.T) {
	s, _, sm, sess, _ := newDisjointSession(t, "" /* no key → no Crypt → SetOrigin fails */)

	rec := postCommit(t, s, sess.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"phase":"done"`) {
		t.Errorf("commit must complete despite config failure; body=%s", body)
	}
	if !strings.Contains(body, "warning") || !strings.Contains(body, "save remote config") {
		t.Errorf("expected a non-fatal config warning in the stream; body=%s", body)
	}
	if strings.Contains(body, `"phase":"error"`) {
		t.Errorf("post-swap config failure must not emit an error phase; body=%s", body)
	}

	// Session was committed and removed — a retry gets a clean 404, never a
	// re-entry into the swap path on a closed store (Change 1).
	if _, ok := sm.Get("alpha", sess.ID); ok {
		t.Error("session must be deleted after a completed commit")
	}
	if rec2 := postCommit(t, s, sess.ID); rec2.Code != http.StatusNotFound {
		t.Errorf("retry after commit: got %d, want 404", rec2.Code)
	}
}

// TestHandleCommit_Disjoint_HappyPathSavesOrigin verifies the normal case: with
// an agent key present, SetOrigin records the credential in control.db, wires
// the origin into the swapped-in store WITHOUT the credential, and emits no
// warning.
func TestHandleCommit_Disjoint_HappyPathSavesOrigin(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "agent.key")
	if err := os.WriteFile(keyPath, []byte("agent-key-material-for-hkdf"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	s, ri, sm, sess, remoteURL := newDisjointSession(t, keyPath)
	m := s.Manager

	rec := postCommit(t, s, sess.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"phase":"done"`) {
		t.Errorf("expected done; body=%s", body)
	}
	if strings.Contains(body, "warning") {
		t.Errorf("happy path must not emit a config warning; body=%s", body)
	}

	// Origin persisted on the swapped-in store.
	var origin *store.Remote
	var err error
	ri.WithRead(func(c *store.Service) { origin, err = c.Remote().GetRemote("origin") })
	if err != nil {
		t.Fatalf("GetRemote: %v", err)
	}
	if origin == nil || origin.URL != remoteURL {
		t.Fatalf("origin not persisted correctly: %+v", origin)
	}
	if origin.AuthMethod != "" {
		t.Errorf("swapped store kept auth method %q; the credential belongs only in control.db", origin.AuthMethod)
	}

	// The credential itself is in control.db, which is the copy that survives
	// losing this repo's database — the whole reason the swap path routes
	// through the funnel rather than writing the store directly.
	if method, token, cerr := m.RepoRegistry().OriginCredential("alpha"); cerr != nil {
		t.Errorf("OriginCredential: %v", cerr)
	} else if method != "token" || token != "tok" {
		t.Errorf("control.db credential = (%q, %q), want (token, tok)", method, token)
	}

	if _, ok := sm.Get("alpha", sess.ID); ok {
		t.Error("session must be deleted after a completed commit")
	}
}
