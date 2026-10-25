package origins

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// A /test whose clone failed leaves the half-made clone.db in the session
// folder (go-git writes HEAD into it before cloning), and the session in
// stateError. A second /test used to open that leftover and fail with the
// misleading "repository already exists"; it is now refused with a 409 that
// says to create a new session, before it touches anything. The wizard opens
// a new session for every test, so no client retests in place.
//
// SABOTAGE: drop the stateError arm of the refusal at the top of
// handleTestConnectivity -> red (the second /test streams instead of 409).
func TestHandleTestConnectivity_RetestAfterFailedCloneIsRefused(t *testing.T) {
	f := newCommitFixture(t, "", nil)
	missing := filepath.Join(f.root, "does-not-exist.git")
	sess, err := f.sm.Create("alpha", missing, authConfig{})
	if err != nil {
		t.Fatal(err)
	}
	runTest := func() *streamRecorder {
		rec := newStreamRecorder()
		f.s.ServeHTTP(rec, httptest.NewRequest("GET", "/repos/alpha/origin-sessions/"+sess.ID+"/test", nil))
		return rec
	}

	first := runTest()
	if !strings.Contains(first.body(), "clone failed") {
		t.Fatalf("first /test must fail in the clone; got %s", first.body())
	}
	sess.mu.Lock()
	state := sess.State
	sess.mu.Unlock()
	if state != stateError {
		t.Fatalf("state after a failed clone = %q, want %q", state, stateError)
	}
	if !exists(t, filepath.Join(sess.TempDir, "clone.db")) {
		t.Fatal("precondition: the failed clone is expected to leave clone.db behind")
	}

	second := runTest()
	if second.Code != 409 || !strings.Contains(second.Header().Get("Content-Type"), "problem+json") {
		t.Fatalf("second /test: status %d, content type %q, want a 409 problem document; body %s",
			second.Code, second.Header().Get("Content-Type"), second.body())
	}
	if !strings.Contains(second.body(), "create a new session") {
		t.Errorf("the 409 must tell the client to create a new session; body %s", second.body())
	}
	if strings.Contains(second.body(), "repository already exists") {
		t.Errorf("the re-test must not reach the clone; body %s", second.body())
	}
}
