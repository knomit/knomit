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
// rebuild is needed. The commit is an AttachOrigin, which must:
//
//   - keep the existing *store.Service (same pointer)
//   - persist the origin to control.db and inject it into the local store
//   - restart the Sync stage on the freshly-configured remote
func TestHandleCommit_SharedHistory_DoesNotSwapLocalStore(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "agent.key")
	if err := os.WriteFile(keyPath, []byte("test-key-material-for-hkdf"), 0o600); err != nil {
		t.Fatalf("write agent key: %v", err)
	}
	f := newCommitFixture(t, keyPath, nil)
	f.sess.mu.Lock()
	f.sess.TestResult = connectivityResult{History: "shared", DefaultBranch: "main", AgentBranches: []string{"machine/test"}}
	f.sess.mu.Unlock()

	// Capture the local svc pointer before commit — if the handler swaps,
	// the after-pointer will differ.
	var svcBefore *store.Service
	f.ri.WithRead(func(s *store.Service) { svcBefore = s })
	syncGenBefore := f.ri.Status()

	rec := newStreamRecorder()
	req := fromLoopback(httptest.NewRequest(http.MethodPost,
		"/repos/alpha/origin-sessions/"+f.sess.ID+"/commit", nil))
	f.s.NewAPIRouter().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"phase":"configuring"`) || !strings.Contains(body, `"phase":"done"`) {
		t.Errorf("expected configuring … done in the SSE stream; body=%s", body)
	}
	if strings.Contains(body, `"phase":"swapping"`) {
		t.Errorf("shared history must not swap; body=%s", body)
	}

	// 1. The local svc must be the same instance — no swap.
	var svcAfter *store.Service
	f.ri.WithRead(func(s *store.Service) { svcAfter = s })
	if svcBefore != svcAfter {
		t.Error("local *store.Service was replaced — shared history must not swap")
	}

	// 2. Origin must be persisted and injected.
	var origin *store.Remote
	var err error
	f.ri.WithRead(func(s *store.Service) {
		origin, err = s.Remote().GetRemote("origin")
	})
	if err != nil {
		t.Fatalf("GetRemote: %v", err)
	}
	if origin == nil || origin.URL != f.remoteURL || origin.AuthMethod != "token" || origin.Branch != "main" {
		t.Fatalf("origin not persisted correctly: %+v", origin)
	}

	// 3. The Sync stage restarted on the configured URL.
	st := f.ri.Status()
	if st.Sync.Origin != f.remoteURL {
		t.Errorf("Sync follows %q, want %q", st.Sync.Origin, f.remoteURL)
	}
	_ = syncGenBefore
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
	req := fromLoopback(httptest.NewRequest(http.MethodPost, "/repos/sub/origin-sessions",
		strings.NewReader(`{"url":"https://example.com/x.git"}`)))
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

// An attach writes the durable origin record, and Origins.Set is a full
// replacement: an empty Mode DELETES the subscription row. A connect session
// cannot reach a subscription today (handleCreateSession 409s first), but PUT
// /origin can — a credential refresh — so the rule is pinned at the write the
// AttachOrigin event's apply makes, not at that guard.
func TestAttachOrigin_PreservesSubscriptionMode(t *testing.T) {
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

	if _, err := m.Send(context.Background(), sub, repos.AttachOrigin(repos.OriginSpec{
		URL: url, Branch: "main", AuthMethod: "token", AuthToken: "s3cret",
	})); err != nil {
		t.Fatalf("attach: %v", err)
	}

	after, err := m.Origins().Get(sub.UID())
	if err != nil || after == nil {
		t.Fatalf("origin after: %v %+v", err, after)
	}
	if after.Mode != repos.OriginModeSubscribe {
		t.Errorf("Mode: got %q, want %q — the attach demoted the subscription",
			after.Mode, repos.OriginModeSubscribe)
	}
	if after.AuthToken != "s3cret" {
		t.Errorf("AuthToken: got %q, want the value just written — the write did not land", after.AuthToken)
	}
}
