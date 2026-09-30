package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/platform/fileuri"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// seedBareKBOn builds a bare knowledge-base remote (the default ontology, one
// commit) whose only branch, and HEAD, is branch.
func seedBareKBOn(t *testing.T, bare, branch string) string {
	t.Helper()
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitForTest(t, "", "init", "--bare", "--initial-branch="+branch, bare)
	work := t.TempDir()
	runGitForTest(t, "", "clone", bare, work)
	runGitForTest(t, work, "checkout", "-B", branch)
	ont, err := fact.DefaultOntology().Serialize()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(work, filepath.Dir(repos.OntologyPath)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, repos.OntologyPath), ont, 0o644); err != nil {
		t.Fatal(err)
	}
	runGitForTest(t, work, "add", "-A")
	runGitForTest(t, work, "commit", "-m", "seed kb")
	runGitForTest(t, work, "push", "origin", branch)
	runGitForTest(t, bare, "symbolic-ref", "HEAD", "refs/heads/"+branch)
	return fileuri.New(bare)
}

// newSyncingServer is a real Manager WITH background sync (no
// DisableBackgroundSync: startSyncLoops returns early on it, which would make
// a loop test vacuous) and a short local reconcile interval, plus a repo
// "mission" cloned from a knowledge base whose only branch is trunk.
func newSyncingServer(t *testing.T, interval time.Duration) (*Server, *repos.Manager, *repos.RepoInstance, string) {
	t.Helper()
	originsRoot := t.TempDir()
	url := seedBareKBOn(t, filepath.Join(originsRoot, "mission.git"), "trunk")
	home := t.TempDir()
	keyPath := filepath.Join(home, "agent.key")
	if err := os.WriteFile(keyPath, []byte("agent-key-material-for-hkdf"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Home = home
	cfg.OntologyRoot = "kb"
	cfg.LocalOriginRoot = originsRoot
	cfg.Git.LocalReconcileInterval = interval
	m := repos.New(context.Background(), repos.Deps{Cfg: cfg, AgentBranch: "agent/test", KeyPath: keyPath})
	t.Cleanup(func() { _ = m.Close() })
	if err := m.Start(); err != nil {
		t.Fatalf("start manager: %v", err)
	}
	ri, err := m.Create(context.Background(), repos.CreateSpec{Name: "mission", Mode: "clone",
		Origin: &repos.OriginSpec{URL: url}}, nil)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	return &Server{Manager: m}, m, ri, url
}

func tipOf(t *testing.T, ri *repos.RepoInstance, branch string) string {
	t.Helper()
	var tip string
	if err := ri.WithRead(func(svc *store.Service) {
		tip, _ = svc.Branches().HeadCommit(context.Background(), branch)
	}); err != nil {
		t.Fatal(err)
	}
	return tip
}

func deleteOrigin(t *testing.T, s *Server, repo string) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.NewAPIRouter().ServeHTTP(rec, fromLoopback(httptest.NewRequest(http.MethodDelete, "/repos/"+repo+"/origin", nil)))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE origin: %d %s", rec.Code, rec.Body.String())
	}
}

// Removing the origin starts the origin-less reconcile loop AT ONCE: a fact
// written after the DELETE reaches the consensus branch (trunk) within a few
// local intervals, with no reopen and no second manager
// (kb/gotchas/repos/origin/detach-starts-no-local-loop).
//
// SABOTAGE: DeleteOrigin calls ri.DeactivateSync() instead of
// ri.StartLocalSync() → no loop runs, trunk never moves → red.
func TestDeleteOrigin_StartsTheLocalReconcileLoopAtOnce(t *testing.T) {
	const interval = 250 * time.Millisecond
	s, _, ri, _ := newSyncingServer(t, interval)
	deleteOrigin(t, s, "mission")

	var consensus string
	if err := ri.WithRead(func(svc *store.Service) { consensus = svc.UpstreamBranch() }); err != nil {
		t.Fatal(err)
	}
	if consensus != "trunk" {
		t.Fatalf("consensus branch after detach = %q, want trunk", consensus)
	}

	var written string
	if err := ri.WithRead(func(svc *store.Service) {
		_, err := svc.Facts().WriteFact(context.Background(), ri.AgentBranch(), "kb/notes/after-detach.md",
			"---\ntype: observation\nconfidence: 0.9\n---\n# Written after the detach\n\nbody\n", "after detach", "")
		if err != nil {
			t.Errorf("write: %v", err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	written = tipOf(t, ri, ri.AgentBranch())
	if written == "" || written == tipOf(t, ri, "trunk") {
		t.Fatalf("precondition: the agent branch must be ahead of trunk after the write (agent %q)", written)
	}

	// A few intervals, not one: the bound only has to be finite, because
	// without the loop trunk never moves at all.
	deadline := time.Now().Add(8 * interval)
	for time.Now().Before(deadline) {
		if tipOf(t, ri, "trunk") == written {
			return
		}
		time.Sleep(interval / 5)
	}
	t.Fatalf("trunk did not advance to the agent tip %s within %s of the detach (tip %s): no local loop is running",
		written, 8*interval, tipOf(t, ri, "trunk"))
}

// PUT /origin with no branch, on a repo with no origin, tracks the repo's OWN
// consensus branch (recorded at clone time, kept across the detach), not a
// name the handler supplies.
//
// SABOTAGE: restore `upstreamMain = "main"` in SetOrigin → the reconcile
// against origin/main fails (502) and control.db would say main → red.
func TestPutOrigin_NoBranchTracksTheRecordedConsensusBranch(t *testing.T) {
	s, m, ri, url := newSyncingServer(t, time.Hour)
	deleteOrigin(t, s, "mission")

	rec := httptest.NewRecorder()
	req := fromLoopback(httptest.NewRequest(http.MethodPut, "/repos/mission/origin",
		strings.NewReader(`{"url":"`+url+`","auth_method":"none"}`)))
	req.Header.Set("Content-Type", "application/json")
	s.NewAPIRouter().ServeHTTP(rec, req)
	if rec.Code < 200 || rec.Code >= 300 {
		t.Fatalf("PUT origin with no branch: %d %s", rec.Code, rec.Body.String())
	}
	o, err := m.Origins().Get(ri.UID())
	if err != nil || o == nil {
		t.Fatalf("origin not stored: %v %v", o, err)
	}
	if o.Branch != "trunk" {
		t.Fatalf("stored branch = %q, want trunk", o.Branch)
	}
}

// The session commit, with neither a chosen branch nor a remote default,
// records NO origin (the config warning says why) rather than one tracking a
// branch called main. Both commit arms: disjoint and shared history.
//
// SABOTAGE: restore the `upstreamMain = "main"` fallback at either arm → an
// origin tracking main is stored → red.
func TestHandleCommit_NoBranchKnownStoresNoInventedBranch(t *testing.T) {
	for _, history := range []string{"disjoint", "shared"} {
		t.Run(history, func(t *testing.T) {
			keyPath := filepath.Join(t.TempDir(), "agent.key")
			if err := os.WriteFile(keyPath, []byte("agent-key-material-for-hkdf"), 0o600); err != nil {
				t.Fatal(err)
			}
			s, ri, _, sess, _ := newDisjointSession(t, keyPath)
			sess.mu.Lock()
			sess.TestResult = connectivityResult{History: history, DefaultBranch: ""}
			sess.RemoteBranch = ""
			sess.mu.Unlock()

			rec := postCommit(t, s, sess.ID)
			body := rec.Body.String()
			if !strings.Contains(body, `"phase":"done"`) || !strings.Contains(body, "save remote config") {
				t.Fatalf("want done with a config warning; body=%s", body)
			}
			o, err := s.Manager.Origins().Get(ri.UID())
			if err != nil {
				t.Fatal(err)
			}
			if o != nil {
				t.Fatalf("an origin was stored with an invented branch: %+v", o)
			}
		})
	}
}
