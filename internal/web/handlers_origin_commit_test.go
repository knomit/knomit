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

// commitFixture is a real Manager with a mounted repo "alpha" and an applied
// disjoint-history origin session whose clone is an independent knowledge
// base at <TempDir>/clone.db — the exact shape handleCommit's swap consumes.
type commitFixture struct {
	s         *Server
	m         *repos.Manager
	ri        *repos.RepoInstance
	sm        *SessionManager
	sess      *OriginSession
	remoteURL string
	root      string
}

// newCommitFixture builds it. keyPath (may be "") becomes the manager's agent
// key, controlling whether control.db's Origins accessor gets a Crypt (and thus
// whether the swap can persist the session's token at all). hook (may be nil)
// is the machines' Options.Hook.
func newCommitFixture(t *testing.T, keyPath string, hook func(repos.StageID, string, context.Context)) *commitFixture {
	t.Helper()
	f := &commitFixture{root: t.TempDir()}
	f.remoteURL = seedBareKBOn(t, filepath.Join(f.root, "remote.git"), "main")

	f.m = repos.New(context.Background(), repos.Deps{
		Cfg:         config.Config{Home: t.TempDir(), LocalOriginRoot: f.root},
		AgentBranch: "machine/test",
		KeyPath:     keyPath,
		Machine:     repos.Options{Synchronous: true, Hook: hook},
	})
	t.Cleanup(func() { _ = f.m.Close() })
	if err := f.m.Start(); err != nil {
		t.Fatalf("start manager: %v", err)
	}
	ri, err := f.m.Create(context.Background(), repos.CreateSpec{Name: "alpha", Mode: "preset", OntologyPreset: "default"}, nil)
	if err != nil {
		t.Fatalf("create alpha: %v", err)
	}
	f.ri = ri

	f.sm = NewSessionManager()
	t.Cleanup(f.sm.Shutdown)
	f.sess, err = f.sm.Create("alpha", f.remoteURL, AuthConfig{Method: "token", Token: "tok"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// The cloned remote the swap will consume: an independent knowledge base.
	remoteSvc, err := store.Open(filepath.Join(f.sess.TempDir, "clone.db"))
	if err != nil {
		t.Fatalf("open remote svc: %v", err)
	}
	if err := remoteSvc.InitRepo(context.Background(), map[string]string{"seed.md": "seed"}, "machine/test"); err != nil {
		t.Fatalf("init remote git: %v", err)
	}
	// handleCommit closes remoteSvc; don't double-close it here.

	f.sess.mu.Lock()
	f.sess.State = StateApplied
	f.sess.RemoteStore = remoteSvc
	f.sess.TestResult = connectivityResult{History: "disjoint", DefaultBranch: "main"}
	f.sess.RemoteBranch = "main"
	f.sess.AppliedBranch = "machine/test"
	f.sess.mu.Unlock()

	f.s = &Server{Manager: f.m, SessionManager: f.sm, AgentBranch: "machine/test"}
	return f
}

// newDisjointSession is newCommitFixture's original shape, kept for the tests
// that only need the server, the instance, the session manager and the
// session.
func newDisjointSession(t *testing.T, keyPath string) (*Server, *repos.RepoInstance, *SessionManager, *OriginSession, string) {
	t.Helper()
	f := newCommitFixture(t, keyPath, nil)
	return f.s, f.ri, f.sm, f.sess, f.remoteURL
}

// newRegisteredManager starts a Manager against a fresh control.db and inserts
// one ACTIVE registry row for (name, uid) — the row a test's hand-built
// RepoInstance stands in for. keyPath (may be "") is the agent key: present
// means credential encryption is available and Origins.Set can store a token;
// absent means it refuses, which is the production shape of "config failed".
func newRegisteredManager(t *testing.T, keyPath, name, uid string) *repos.Manager {
	t.Helper()
	m := repos.New(context.Background(), repos.Deps{
		Cfg:         config.Config{Home: t.TempDir()},
		AgentBranch: "machine/test",
		KeyPath:     keyPath,
		Machine:     repos.Options{Synchronous: true},
	})
	t.Cleanup(func() { _ = m.Close() })
	if err := m.Start(); err != nil {
		t.Fatalf("start manager: %v", err)
	}
	if err := m.Repos().Insert(repos.RepoRecord{
		UID: uid, Name: name, State: repos.StateActive,
		Profile: repos.ProfileCode, CreatedAt: 1,
	}); err != nil {
		t.Fatalf("register %q: %v", name, err)
	}
	return m
}

// Returns the shared SSE recorder: /commit streams, and an SSE handler here
// needs SetWriteDeadline as well as Flush — a bare ResponseRecorder has only
// the latter, which sends the stream down its refuse-to-start path and reads
// as an empty body rather than as an incomplete double.
func postCommit(t *testing.T, s *Server, sessID string) *streamRecorder {
	t.Helper()
	return postCommitBody(t, s, sessID, "")
}

// postCommitBody is postCommit with a JSON body ("" for none).
func postCommitBody(t *testing.T, s *Server, sessID, body string) *streamRecorder {
	t.Helper()
	rec := newStreamRecorder()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(http.MethodPost, "/repos/alpha/origin-sessions/"+sessID+"/commit", nil)
	} else {
		req = newJSONRequest(http.MethodPost, "/repos/alpha/origin-sessions/"+sessID+"/commit", strings.NewReader(body))
	}
	s.NewAPIRouter().ServeHTTP(rec, fromLoopback(req))
	return rec
}

// TestHandleCommit_Disjoint_PostSwapConfigFailureStillCompletes: once the
// store is installed the swap is past its point of no return, so a failure to
// persist the origin must NOT abort into a retryable half-done state. With no
// agent key, control.db has no Crypt, so Origins.Set refuses to store the
// session's token — the commit must still reach "done" (carrying a non-fatal
// warning, the swap reply's) and delete the session, not error out.
func TestHandleCommit_Disjoint_PostSwapConfigFailureStillCompletes(t *testing.T) {
	s, _, sm, sess, _ := newDisjointSession(t, "" /* no key → no Crypt → Origins.Set refuses the token */)

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
	// re-entry into the swap path on a closed store.
	if _, ok := sm.Get("alpha", sess.ID); ok {
		t.Error("session must be deleted after a completed commit")
	}
	if rec2 := postCommit(t, s, sess.ID); rec2.Code != http.StatusNotFound {
		t.Errorf("retry after commit: got %d, want 404", rec2.Code)
	}
}

// TestHandleCommit_Disjoint_HappyPathSavesOrigin verifies the normal case: with
// an agent key present, control.db has a Crypt, the swap persists the origin,
// the reopened store has it injected, and no warning is emitted.
func TestHandleCommit_Disjoint_HappyPathSavesOrigin(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "agent.key")
	if err := os.WriteFile(keyPath, []byte("agent-key-material-for-hkdf"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	s, ri, sm, sess, remoteURL := newDisjointSession(t, keyPath)

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
	for _, phase := range []string{`"phase":"swapping"`, `"phase":"configuring"`} {
		if !strings.Contains(body, phase) {
			t.Errorf("the stream must narrate %s; body=%s", phase, body)
		}
	}

	// Origin persisted, and injected into the swapped-in store.
	var origin *store.Remote
	var err error
	if aerr := ri.WithRead(func(c *store.Service) { origin, err = c.Remote().GetRemote("origin") }); aerr != nil {
		t.Fatalf("acquire: %v", aerr)
	}
	if err != nil {
		t.Fatalf("GetRemote: %v", err)
	}
	if origin == nil || origin.URL != remoteURL {
		t.Fatalf("origin not persisted correctly: %+v", origin)
	}

	if _, ok := sm.Get("alpha", sess.ID); ok {
		t.Error("session must be deleted after a completed commit")
	}
}
