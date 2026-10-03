package web

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"knomit/internal/repos"
	"knomit/internal/store"
)

// Issue #400: a disjoint-history commit swaps the store, and SwapStore cancels
// the repo's initial index heal if it is still running. The heal's cancelled
// exit marks nothing, so the index state stayed 'indexing' for the life of the
// process — the handler's own rebuild built a correct index but never marked
// it, and the rebuild endpoint refuses with 409 while the state reads
// 'indexing', so nothing in the UI could clear it.
//
// The heal itself cannot be held from this package (the gate is unexported in
// repos, and repos' own test holds it — TestSwapStore_DuringInitialHeal_*).
// What a cancelled heal leaves behind is exactly a state of 'indexing' with no
// writer, so the test puts the instance in that state through the production
// mutator and asserts the commit clears it.
func TestHandleCommit_Disjoint_ClearsAnIndexingStateLeftByACancelledHeal(t *testing.T) {
	s, ri, _, sess, _ := newDisjointSession(t, "")
	ri.TestMarkIndexing()

	rec := postCommit(t, s, sess.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, `"phase":"done"`) {
		t.Fatalf("expected the commit to complete; body=%s", body)
	}

	state, _, _ := ri.IndexStatus()
	if state != repos.IndexStateReady {
		t.Fatalf("index state after the post-swap rebuild: got %q, want %q — "+
			"'indexing' here is pinned for the life of the process", state, repos.IndexStateReady)
	}
}

// failingSwapSession is newDisjointSession with two differences that reach
// SwapStore's FILE-BACKED failure path: the instance is file-backed (DBPath),
// and the session's clone path is a directory, so copying it over the live
// database fails and SwapStore restores the backup and reattaches the old
// store. Both sync restarts are recorded.
type syncCalls struct {
	mu          sync.Mutex
	activateURL []string
	local       int
}

func failingSwapSession(t *testing.T, withOrigin bool) (*Server, *repos.RepoInstance, *OriginSession, *syncCalls) {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "local.db")
	localSvc, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open local svc: %v", err)
	}
	if err := localSvc.InitRepo(context.Background(), map[string]string{"local.md": "local"}, "machine/test"); err != nil {
		t.Fatalf("init local git: %v", err)
	}
	// SwapStore closes this generation and reopens dbPath; closing it again
	// here is harmless if it is still the attached one.
	t.Cleanup(func() { _ = localSvc.Close() })

	calls := &syncCalls{}
	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:        "alpha",
		UID:         "alpha-uid",
		AgentBranch: "machine/test",
		Svc:         localSvc,
		DBPath:      dbPath,
		StartSync: func(url string) error {
			calls.mu.Lock()
			calls.activateURL = append(calls.activateURL, url)
			calls.mu.Unlock()
			return nil
		},
		StartLocalSync: func() error {
			calls.mu.Lock()
			calls.local++
			calls.mu.Unlock()
			return nil
		},
	})

	m := newRegisteredManager(t, "", "alpha", "alpha-uid")
	m.Set("alpha", ri)
	// A test instance has no closeFn, and a failed swap reattaches a store
	// reopened from dbPath that nothing else owns: close whichever generation
	// is attached so no handle outlives the TempDir.
	t.Cleanup(func() {
		if svc, release, err := ri.Acquire(); err == nil {
			release()
			_ = svc.Close()
		}
	})

	if withOrigin {
		// The injected origin is what the running sync loop reads.
		localSvc.SetOrigin(&store.Origin{URL: "https://previous.test/kb.git", Branch: "main"})
	}

	sm := NewSessionManager()
	t.Cleanup(sm.Shutdown)
	sess, err := sm.Create("alpha", "https://example.com/repo.git", AuthConfig{Method: "token", Token: "tok"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	// A real clone to satisfy the session's guards, at a path other than the
	// one the handler swaps from; that one is a directory, so the swap's copy
	// fails after the live database has been closed.
	remoteSvc, err := store.Open(filepath.Join(sess.TempDir, "elsewhere.db"))
	if err != nil {
		t.Fatalf("open remote svc: %v", err)
	}
	if err := remoteSvc.InitRepo(context.Background(), map[string]string{"seed.md": "seed"}, "machine/test"); err != nil {
		t.Fatalf("init remote git: %v", err)
	}
	if err := os.Mkdir(filepath.Join(sess.TempDir, "clone.db"), 0o755); err != nil {
		t.Fatalf("mkdir clone.db: %v", err)
	}

	sess.mu.Lock()
	sess.State = StateApplied
	sess.RemoteStore = remoteSvc
	sess.TestResult = connectivityResult{History: "disjoint", DefaultBranch: "main"}
	sess.RemoteBranch = "main"
	sess.AppliedBranch = "machine/test"
	sess.mu.Unlock()

	return &Server{Manager: m, SessionManager: sm, AgentBranch: "machine/test"}, ri, sess, calls
}

// A failed swap leaves the repo on its old store, and SwapStore stopped that
// store's sync loop on the way in — restarting sync is its caller's contract.
// Before the fix the handler returned the error without restarting anything,
// so the repo silently stopped syncing until the process restarted.
func TestHandleCommit_Disjoint_FailedSwapRestartsThePreviousSyncMode(t *testing.T) {
	for _, tc := range []struct {
		name       string
		withOrigin bool
	}{
		{"origin: ActivateSync with the previous URL", true},
		{"no origin: StartLocalSync", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, ri, sess, calls := failingSwapSession(t, tc.withOrigin)
			// A swap that cancels the initial heal leaves this behind; the
			// failure path must not keep it with no writer.
			ri.TestMarkIndexing()

			rec := postCommit(t, s, sess.ID)
			body := rec.Body.String()
			if !strings.Contains(body, `"phase":"error"`) || !strings.Contains(body, "swap failed") {
				t.Fatalf("fixture must make the swap fail; body=%s", body)
			}

			// The old store is back, so the restart below has a store to run on.
			if err := ri.WithRead(func(*store.Service) {}); err != nil {
				t.Fatalf("a failed swap must reattach the old store: %v", err)
			}

			calls.mu.Lock()
			activated, local := append([]string(nil), calls.activateURL...), calls.local
			calls.mu.Unlock()
			if tc.withOrigin {
				if len(activated) != 1 || activated[0] != "https://previous.test/kb.git" || local != 0 {
					t.Fatalf("want one ActivateSync with the PREVIOUS origin and no local sync; got activate=%v local=%d", activated, local)
				}
			} else {
				if len(activated) != 0 || local != 1 {
					t.Fatalf("want one StartLocalSync and no ActivateSync; got activate=%v local=%d", activated, local)
				}
			}

			if state, _, _ := ri.IndexStatus(); state == repos.IndexStateIndexing {
				t.Fatalf("index state after a failed swap: %q with no writer left", state)
			}
		})
	}
}
