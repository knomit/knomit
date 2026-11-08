package origins

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knomit/internal/corpus/repos"
	"knomit/internal/corpus/repos/events"
	"knomit/internal/engine/store"
)

// holdSessionKB makes the session's clone refusable: a second repo "beta"
// takes the clone's knowledge base (SwapStore of a checkpointed copy of
// clone.db), so the commit's swap guard refuses with
// ErrKnowledgeBaseAlreadyLocal before anything changes. The clone itself is
// only checkpointed and copied; it stays open on the session.
func holdSessionKB(t *testing.T, f *commitFixture) {
	t.Helper()
	ctx := context.Background()
	beta, err := f.m.Create(ctx, repos.CreateSpec{Name: "beta", Mode: "preset", OntologyPreset: "default"}, nil)
	if err != nil {
		t.Fatalf("create beta: %v", err)
	}
	f.sess.mu.Lock()
	svc := f.sess.RemoteStore
	f.sess.mu.Unlock()
	if err := svc.Checkpoint(); err != nil {
		t.Fatalf("checkpoint clone: %v", err)
	}
	copyPath := filepath.Join(t.TempDir(), "copy.db")
	if err := copyFileForTest(filepath.Join(f.sess.TempDir, "clone.db"), copyPath); err != nil {
		t.Fatalf("copy clone: %v", err)
	}
	if _, err := f.m.Send(ctx, beta, repos.SwapStore(repos.SwapSpec{TempDB: copyPath, IdentityBranch: "machine/test"})); err != nil {
		t.Fatalf("swap beta onto the clone's knowledge base: %v", err)
	}
}

func copyFileForTest(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o600)
}

func sessSnapshot(sess *originSession) (sessionState, *store.Service) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.State, sess.RemoteStore
}

// A swap the lifecycle machine refuses (here: the clone's knowledge base is
// already local) changed nothing, so the session must stay retryable: still
// applied, its clone reopened and attached, the repo on its previous store.
// Before #416 the clone was closed and detached before the send, and the retry
// died on "session has no remote store".
// SABOTAGE: drop the reopen after a refused swap -> RemoteStore nil -> red.
func TestHandleCommit_Disjoint_RefusedSwapLeavesSessionRetryable(t *testing.T) {
	f := newCommitFixture(t, writeAgentKey(t), nil)
	holdSessionKB(t, f)
	idBefore := f.ri.ID()

	rec := postCommit(t, f.s, f.sess.ID)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "already local") {
		t.Fatalf("a swap onto a knowledge base already local: got %d, body=%s", rec.Code, rec.Body.String())
	}
	state, svc := sessSnapshot(f.sess)
	if state != stateApplied {
		t.Errorf("state after a refused swap: got %v, want applied", state)
	}
	if svc == nil {
		t.Fatal("the session must keep a store to retry with")
	}
	if _, err := svc.RootCommit(context.Background(), "machine/test"); err != nil {
		t.Errorf("the reopened clone must be usable: %v", err)
	}
	if got := f.ri.ID(); got != idBefore {
		t.Errorf("a refused swap must leave the repo's store: id %q, want %q", got, idBefore)
	}
	if _, ok := f.sm.Get("alpha", f.sess.ID); !ok {
		t.Error("the session must survive a refused commit")
	}
}

// The retry the refusal exists for: once the cause is gone (beta archived) the
// same session commits, the swap completes, the origin is recorded and the
// session is dropped.
// SABOTAGE: as above -> the retry answers 409 "no remote store" -> red.
func TestHandleCommit_Disjoint_RetryAfterRefusalSucceeds(t *testing.T) {
	f := newCommitFixture(t, writeAgentKey(t), nil)
	holdSessionKB(t, f)

	if rec := postCommit(t, f.s, f.sess.ID); rec.Code != http.StatusConflict {
		t.Fatalf("first commit: got %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if _, err := f.m.Archive("beta"); err != nil {
		t.Fatalf("archive beta: %v", err)
	}

	rec := postCommit(t, f.s, f.sess.ID)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"phase":"done"`) {
		t.Fatalf("retry: got %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := waitIndexSettledWeb(t, f.ri); got != events.IndexStateReady {
		t.Errorf("index after the retried swap: got %q, want ready", got)
	}
	o, err := f.s.Manager.Origins().Get(f.ri.UID())
	if err != nil || o == nil || o.URL != f.remoteURL {
		t.Fatalf("origin row after the retry: %+v, %v", o, err)
	}
	if _, ok := f.sm.Get("alpha", f.sess.ID); ok {
		t.Error("session must be deleted after the completed retry")
	}
}

// If the clone cannot be reopened after a refusal the session cannot be
// retried: it moves to stateError (as after a failed /test), the original
// refusal is still what the client reads, and the next commit is refused
// outright.
func TestHandleCommit_Disjoint_ReopenFailureMarksSessionError(t *testing.T) {
	f := newCommitFixture(t, writeAgentKey(t), nil)
	holdSessionKB(t, f)
	prev := openSessionCloneFn
	openSessionCloneFn = func(*repos.Manager, *repos.RepoInstance, string) (*store.Service, error) {
		return nil, errors.New("disk on fire")
	}
	t.Cleanup(func() { openSessionCloneFn = prev })

	rec := postCommit(t, f.s, f.sess.ID)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "already local") {
		t.Fatalf("the original refusal must still be delivered: got %d, body=%s", rec.Code, rec.Body.String())
	}
	state, svc := sessSnapshot(f.sess)
	if state != stateError || svc != nil {
		t.Errorf("after a failed reopen: state %v, store %v; want stateError and no store", state, svc)
	}
	rec = postCommit(t, f.s, f.sess.ID)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "applied state") {
		t.Errorf("next commit: got %d, body=%s; want 409 applied state", rec.Code, rec.Body.String())
	}
}

// With cancel_indexing the stream is already open when the guard refuses, so
// the refusal arrives as an error phase, not an HTTP problem. It is a refusal
// all the same: the clone comes back.
// SABOTAGE: streamLifecycle returning nil for a narrated error -> no reopen -> red.
func TestHandleCommit_Disjoint_RefusalAfterCancelIndexingReopens(t *testing.T) {
	hold := newIndexHold()
	f := newCommitFixture(t, writeAgentKey(t), hold.hook)
	t.Cleanup(hold.open)
	holdSessionKB(t, f)
	hold.armed.Store(true)
	if _, err := f.m.Send(context.Background(), f.ri, repos.Rebuild(f.ri.AgentBranch())); err != nil {
		t.Fatalf("start a rebuild: %v", err)
	}
	hold.waitArrived(t)
	hold.armed.Store(false)

	rec := postCommitBody(t, f.s, f.sess.ID, `{"cancel_indexing": true}`)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `"phase":"cancelling-index"`) ||
		!strings.Contains(body, `"phase":"error"`) || !strings.Contains(body, "already") {
		t.Fatalf("expected cancelling-index then an error phase; got %d, body=%s", rec.Code, body)
	}
	state, svc := sessSnapshot(f.sess)
	if state != stateApplied || svc == nil {
		t.Fatalf("after the narrated refusal: state %v, store %v; want applied with a store", state, svc)
	}
}

// A shared-history commit is an attach, which never touches clone.db: a
// refused attach leaves the very same store handle on the session.
// SABOTAGE: restore the unconditional checkpoint/close/detach before the
// send -> RemoteStore nil -> red.
func TestHandleCommit_Shared_RefusedAttachKeepsClone(t *testing.T) {
	f := newCommitFixture(t, writeAgentKey(t), nil)
	// A local origin outside LocalOriginRoot: the attach guard refuses it
	// (ErrLocalOriginDenied) before anything changes.
	outside := seedBareKBOn(t, filepath.Join(t.TempDir(), "outside.git"), "main")
	f.sess.mu.Lock()
	f.sess.URL = outside
	f.sess.TestResult = connectivityResult{History: "shared", DefaultBranch: "main"}
	before := f.sess.RemoteStore
	f.sess.mu.Unlock()

	rec := postCommit(t, f.s, f.sess.ID)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("the attach must be refused as 400 Origin not allowed; got %d, body=%s", rec.Code, rec.Body.String())
	}
	state, after := sessSnapshot(f.sess)
	if state != stateApplied || after == nil || after != before {
		t.Errorf("a refused attach must keep the session's own clone: state %v, store %p, want %p", state, after, before)
	}
}
