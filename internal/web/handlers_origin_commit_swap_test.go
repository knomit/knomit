package web

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"knomit/internal/repos"
)

// writeAgentKey writes an agent key file and returns its path: with it,
// control.db has a Crypt and Origins.Set can store a credential.
func writeAgentKey(t *testing.T) string {
	t.Helper()
	keyPath := t.TempDir() + "/agent.key"
	if err := os.WriteFile(keyPath, []byte("agent-key-material-for-hkdf"), 0o600); err != nil {
		t.Fatal(err)
	}
	return keyPath
}

// indexHold holds the machines' index job at its hook once armed, until
// released. Like every hook it watches the life's ctx, so a held job still
// unmounts.
type indexHold struct {
	armed       atomic.Bool
	arrived     chan struct{}
	release     chan struct{}
	arriveOnce  sync.Once
	releaseOnce sync.Once
}

func newIndexHold() *indexHold {
	return &indexHold{arrived: make(chan struct{}), release: make(chan struct{})}
}

func (h *indexHold) hook(_ repos.StageID, point string, ctx context.Context) {
	if point != "index-job" || !h.armed.Load() {
		return
	}
	h.arriveOnce.Do(func() { close(h.arrived) })
	select {
	case <-h.release:
	case <-ctx.Done():
	}
}

func (h *indexHold) open() { h.releaseOnce.Do(func() { close(h.release) }) }

func (h *indexHold) waitArrived(t *testing.T) {
	t.Helper()
	select {
	case <-h.arrived:
	case <-time.After(30 * time.Second):
		t.Fatal("the index job never reached the hook")
	}
}

// waitIndexSettledWeb waits, through Watch, until ri's index has left
// "indexing", and returns the state it settled in.
func waitIndexSettledWeb(t *testing.T, ri *repos.RepoInstance) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for tr := range ri.Watch(ctx) {
		if tr.Status.Index.State != repos.IndexStateIndexing {
			return tr.Status.Index.State
		}
	}
	t.Fatalf("index of %s did not settle", ri.Name())
	return ""
}

// A swap is exclusive with indexing. A commit while the repo's index job runs
// is refused with 409 — before the stream opens and before the session's clone
// is touched — and its body links the way out (index:cancel). Retried with
// {"cancel_indexing": true} it cancels the job, swaps, and narrates the new
// store's index to ready.
func TestHandleCommit_Disjoint_SwapDuringIndexingIs409_CancelAndContinueReachesReady(t *testing.T) {
	hold := newIndexHold()
	// An agent key, so control.db can store the session's credential: the
	// swap persists its origin before installing the store, and a refused
	// credential would abort the swap.
	f := newCommitFixture(t, writeAgentKey(t), hold.hook)
	t.Cleanup(hold.open)
	hold.armed.Store(true)
	if _, err := f.m.Send(context.Background(), f.ri, repos.Rebuild(f.ri.AgentBranch())); err != nil {
		t.Fatalf("start a rebuild: %v", err)
	}
	hold.waitArrived(t)

	rec := postCommit(t, f.s, f.sess.ID)
	if rec.Code != http.StatusConflict {
		t.Fatalf("commit while indexing: got %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	var problem struct {
		Title string `json:"title"`
		Links map[string]struct {
			Href   string `json:"href"`
			Method string `json:"method"`
		} `json:"_links"`
	}
	if err := json.Unmarshal([]byte(rec.Body.String()), &problem); err != nil {
		t.Fatalf("409 body: %v (%s)", err, rec.Body.String())
	}
	if problem.Title != "Repo is indexing" || problem.Links["cancel-index"].Href != "/api/v1/repos/alpha/index:cancel" ||
		problem.Links["cancel-index"].Method != http.MethodPost {
		t.Fatalf("409 must name the state and link index:cancel; got %+v", problem)
	}

	// Cancel and continue. The swapped-in store's own index job is not held.
	hold.armed.Store(false)
	rec = postCommitBody(t, f.s, f.sess.ID, `{"cancel_indexing": true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("commit with cancel_indexing: got %d, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"phase":"cancelling-index"`) || !strings.Contains(body, `"phase":"done"`) {
		t.Fatalf("expected cancelling-index … done; body=%s", body)
	}
	if !strings.Contains(body, `"index_state":"ready"`) {
		t.Fatalf("the commit's done must report the new store's index ready; body=%s", body)
	}
	if got := f.ri.Status().Index.State; got != repos.IndexStateReady {
		t.Fatalf("index state after the swap: got %q, want ready", got)
	}
}

// A failed swap replies with the failure, and the repo comes back on its
// previous store running the previous sync mode: the walk forward always runs
// after the exits, and Open reopens the restored store through its one wiring
// function, origin included. The install is made to fail at its first step: a
// directory sits where the backup would be written.
func TestHandleCommit_Disjoint_FailedSwapRestartsThePreviousSyncMode(t *testing.T) {
	for _, tc := range []struct {
		name       string
		withOrigin bool
	}{
		{"origin: the previous origin's loop", true},
		{"no origin: the local loop", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCommitFixture(t, "", nil)
			previous := ""
			if tc.withOrigin {
				previous = seedBareKBOn(t, f.root+"/previous.git", "main")
				if _, err := f.m.Send(context.Background(), f.ri, repos.AttachOrigin(repos.OriginSpec{URL: previous, Branch: "main"})); err != nil {
					t.Fatalf("attach the previous origin: %v", err)
				}
			}
			idBefore := f.ri.ID()
			if err := os.Mkdir(f.m.RepoPath(f.ri.UID())+".bak", 0o755); err != nil {
				t.Fatal(err)
			}

			rec := postCommit(t, f.s, f.sess.ID)
			body := rec.Body.String()
			if !strings.Contains(body, `"phase":"error"`) || !strings.Contains(body, "swap failed") {
				t.Fatalf("the swap must fail and say so; body=%s", body)
			}
			st := f.ri.Status()
			if st.Stage != "ready" {
				t.Fatalf("the repo must come back: stage %q", st.Stage)
			}
			if st.Sync.Origin != previous {
				t.Fatalf("Sync must run on the previous origin %q, got %q", previous, st.Sync.Origin)
			}
			if f.ri.ID() != idBefore {
				t.Fatalf("the previous store must be back")
			}
			if got := waitIndexSettledWeb(t, f.ri); got != repos.IndexStateReady {
				t.Fatalf("index state after a failed swap: %q, want ready (the restored store is healed)", got)
			}
		})
	}
}
