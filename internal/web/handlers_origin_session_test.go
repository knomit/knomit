package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
//   - persist the origin to control.db and inject it into the local store
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
		UID:         "alpha-uid",
		AgentBranch: "machine/test",
		Svc:         localSvc,
		StartSync: func(url string) error {
			activateCalled = true
			activateURL = url
			return nil
		},
	})

	// Mirror production: connection identity is written to control.db, keyed by
	// the repo's registry uid, and the agent key there is what encrypts the
	// session's auth token. Without a started manager (and its key) the commit
	// step would refuse to persist the token and fail at "configuring".
	keyPath := filepath.Join(t.TempDir(), "agent.key")
	if err := os.WriteFile(keyPath, []byte("test-key-material-for-hkdf"), 0o600); err != nil {
		t.Fatalf("write agent key: %v", err)
	}
	m := newRegisteredManager(t, keyPath, "alpha", "alpha-uid")
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

	// Shared SSE recorder: /commit streams, so the double needs
	// SetWriteDeadline as well as Flush.
	rec := newStreamRecorder()
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
	if origin.AuthMethod != "token" {
		t.Errorf("origin auth method: got %q, want %q", origin.AuthMethod, "token")
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

// A subscription cannot be re-pointed through a connect session: the flow ends
// in a store swap, which for a repo that owns no content of its own would
// replace the thing it follows rather than reconcile it.
func TestHandleCreateSession_SubscriptionIs409(t *testing.T) {
	m := repos.New(context.Background(), repos.Deps{})
	m.Set("sub", repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "sub", UID: "sub-uid", Subscribed: true, ReadBranch: "main",
	}))
	sm := NewSessionManager()
	s := &Server{Manager: m, SessionManager: sm, AgentBranch: "machine/test"}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/repos/sub/origin-sessions",
		strings.NewReader(`{"url":"https://example.com/x.git"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want 409, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Subscription requires its origin") {
		t.Errorf("body does not name the refusal: %s", rec.Body.String())
	}

	// And nothing was created.
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/repos/sub/origin-sessions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"id"`) {
		t.Errorf("a session was created despite the refusal: %s", rec.Body.String())
	}
}

// persistSessionOrigin writes the durable origin record, and Origins.Set is a
// full replacement: an empty Mode DELETES the subscription row. A connect
// session cannot reach a subscription today (handleCreateSession 409s first),
// so this pins the rule at the WRITE rather than at that guard — the guard is
// three calls away and a future caller need not go through it.
func TestPersistSessionOrigin_PreservesSubscriptionMode(t *testing.T) {
	originsRoot := t.TempDir()
	_, m, _ := newControlDBTestServer(t, originsRoot)

	url := seedBareRemoteKBForTest(t, filepath.Join(originsRoot, "kb.git"))
	sub, err := m.Create(context.Background(), repos.CreateSpec{
		Name: "sub", Mode: "subscribe", Origin: &repos.OriginSpec{URL: url},
	}, nil)
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	before, err := m.Origins().Get(sub.UID())
	if err != nil || before == nil || before.Mode != repos.OriginModeSubscribe {
		t.Fatalf("precondition: want a subscribe-mode origin, got %+v (err %v)", before, err)
	}

	var perr error
	if werr := sub.WithRead(func(svc *store.Service) {
		perr = persistSessionOrigin(m, sub, svc, url, "main", "", "token", "s3cret")
	}); werr != nil {
		t.Fatalf("WithRead: %v", werr)
	}
	if perr != nil {
		t.Fatalf("persistSessionOrigin: %v", perr)
	}

	after, err := m.Origins().Get(sub.UID())
	if err != nil || after == nil {
		t.Fatalf("origin after: %v %+v", err, after)
	}
	if after.Mode != repos.OriginModeSubscribe {
		t.Errorf("Mode: got %q, want %q — the session write demoted the subscription",
			after.Mode, repos.OriginModeSubscribe)
	}
	if after.AuthToken != "s3cret" {
		t.Errorf("AuthToken: got %q, want the value just written — the write did not land", after.AuthToken)
	}
}
