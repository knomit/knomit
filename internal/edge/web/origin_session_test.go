package web

import (
	"testing"
)

// TestOriginSession_RemoteBranchSurvivesGet regression-tests PR #61 review
// finding #1: the user's chosen upstream branch (set by /apply) was lost
// before /commit could read it, because OriginSession did not carry a
// dedicated field. Both handlers now read/write OriginSession.RemoteBranch.
//
// This test pins the data-model contract: writing RemoteBranch on the
// session and looking it back up via the manager must round-trip the value.
func TestOriginSession_RemoteBranchSurvivesGet(t *testing.T) {
	sm := NewSessionManager()
	t.Cleanup(sm.Shutdown)

	sess, err := sm.Create("alpha", "https://example.com/repo.git", AuthConfig{})
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
