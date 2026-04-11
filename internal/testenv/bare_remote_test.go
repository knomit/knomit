package testenv

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestBareRemote_InitCreatesEmptyRepo asserts that BareRemote creates a
// valid bare git repo on disk that `git` can list via ls-remote.
func TestBareRemote_InitCreatesEmptyRepo(t *testing.T) {
	t.Log("Scenario: sb.BareRemote(\"origin\") creates a file:// bare repo that ls-remote can query")
	sb := NewStoryboard(t)
	remote := sb.BareRemote("origin")

	require.NotEmpty(t, remote.URL())
	require.NotEmpty(t, remote.Dir())
	require.Equal(t, "origin", remote.Name())

	// ls-remote on an empty bare repo returns exit 0 with empty stdout.
	out, err := exec.Command("git", "ls-remote", remote.URL()).CombinedOutput()
	require.NoError(t, err, "ls-remote failed: %s", out)
}

// TestBareRemote_ConnectIsIdempotent asserts that calling Connect twice with
// the same remote does not break the repo — this matches the production
// SetRemote INSERT OR REPLACE semantics.
func TestBareRemote_ConnectIsIdempotent(t *testing.T) {
	t.Log("Scenario: Connect(remote) twice is harmless")
	sb := NewStoryboard(t)
	remote := sb.BareRemote("origin")

	a := sb.Repo("a").Connect(remote).Connect(remote)
	a.MustVerify()
}

// TestBareRemote_PushWithNoOriginNoOps asserts that calling Push on a repo
// that has NOT been Connect()ed is a clean no-op (matches production
// behavior: Push returns PushResult{} when no origin remote is configured).
func TestBareRemote_PushWithNoOriginNoOps(t *testing.T) {
	t.Log("Scenario: Push on unconnected repo is a no-op, not an error")
	sb := NewStoryboard(t)
	a := sb.Repo("a").Branch("agent/test")
	result := a.Push()
	require.False(t, result.Pushed, "Push on unconnected repo should not report Pushed")
}

// TestBareRemote_SyncWithNoOriginNoOps mirrors the Push no-op case for Sync.
func TestBareRemote_SyncWithNoOriginNoOps(t *testing.T) {
	t.Log("Scenario: Sync on unconnected repo is a no-op, not an error")
	sb := NewStoryboard(t)
	a := sb.Repo("a").Branch("agent/test")
	result := a.Sync()
	require.False(t, result.Synced, "Sync on unconnected repo should not report Synced")
}

// TestBareRemote_PushSyncRoundTrip is the core happy path: repo A writes a
// fact, pushes, repo B syncs, sees the fact.
//
// Note: "sync" in knomit's production model pulls origin/main into the
// local branch. So the test has to write on a branch that the bare remote
// will accept AND that repo B's Sync will pull. The simplest shape is:
//
//  1. A.Branch("main").Write(fact).Push()   — A pushes main to origin
//  2. B.Branch("main").Sync()               — B pulls origin/main into local main
//  3. B.Branch("main").Head().Fact(...)...  — B sees the fact
//
// TODO(Phase3-E2): If this hits "no common ancestor" (disjoint histories
// between A's initial main commit and B's initial main commit), the full
// scenario test lives in Phase 3 Category E which will add
// Storyboard.RepoFromRemote (clone-from-remote semantics).
func TestBareRemote_PushSyncRoundTrip(t *testing.T) {
	t.Log("Scenario: A writes fact on main, pushes; B connects and syncs; B sees the fact")
	sb := NewStoryboard(t)
	remote := sb.BareRemote("origin")

	a := sb.Repo("a").Connect(remote)
	aMain := a.Branch("main")
	aMain.Write("kb/shared.md", Fact("shared"), "A writes shared fact")
	aMain.Push()

	b := sb.Repo("b").Connect(remote)
	bMain := b.Branch("main")
	bMain.Sync()

	bMain.Head().Fact("kb/shared.md").MustExist()
}
