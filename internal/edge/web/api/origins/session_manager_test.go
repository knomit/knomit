package origins

import (
	"testing"
)

// TestOriginSession_RemoteBranchSurvivesGet regression-tests PR #61 review
// finding #1: the user's chosen upstream branch (set by /apply) was lost
// before /commit could read it, because originSession did not carry a
// dedicated field. Both handlers now read/write originSession.RemoteBranch.
//
// This test pins the data-model contract: writing RemoteBranch on the
// session and looking it back up via the manager must round-trip the value.
func TestOriginSession_RemoteBranchSurvivesGet(t *testing.T) {
	sm := newSessionManager()
	t.Cleanup(sm.drain)

	sess, err := sm.Create("alpha", "https://example.com/repo.git", authConfig{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	sess.mu.Lock()
	sess.RemoteBranch = "master"
	sess.mu.Unlock()

	got, ok := sm.Get("alpha", sess.ID)
	if !ok {
		t.Fatalf("session not retrievable by id")
	}
	if got.RemoteBranch != "master" {
		t.Errorf("RemoteBranch: got %q, want %q", got.RemoteBranch, "master")
	}
}

// The idle sweeper runs only while the manager holds a session (the LIFETIME
// note on sessionManager): a new manager owns no goroutine, Create starts one,
// the sweeper's exit check keeps it while a session is held and stops it once
// none is, and the next Create starts it again.
//
// SABOTAGE: drop sm.ensureSweeper() from Create, or make stopIfEmpty report
// true with a session held -> red.
func TestSessionManager_SweeperRunsOnlyWhileSessionsExist(t *testing.T) {
	sweeping := func(sm *sessionManager) bool {
		sm.sweepMu.Lock()
		defer sm.sweepMu.Unlock()
		return sm.sweeping
	}
	sm := newSessionManager()
	t.Cleanup(sm.drain)
	if sweeping(sm) {
		t.Fatal("a new manager is sweeping: it owns a goroutine before any session exists")
	}
	sess, err := sm.Create("alpha", "https://example.com/repo.git", authConfig{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !sweeping(sm) {
		t.Fatal("Create did not start the sweeper")
	}
	if sm.stopIfEmpty() || !sweeping(sm) {
		t.Fatal("the sweeper would stop with a session held")
	}
	sm.Delete("alpha", sess.ID)
	if !sm.stopIfEmpty() || sweeping(sm) {
		t.Fatal("the sweeper would keep running with no session held")
	}
	if _, err := sm.Create("alpha", "https://example.com/repo.git", authConfig{}); err != nil {
		t.Fatalf("create again: %v", err)
	}
	if !sweeping(sm) {
		t.Fatal("Create after the sweeper stopped did not start it again")
	}
}
