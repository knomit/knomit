package testenv

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
// Origins.Set upsert semantics.
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
	require.Empty(t, string(result.Main.Mode), "Sync on unconnected repo should not advance main")
	require.Empty(t, string(result.Agent.Mode), "Sync on unconnected repo should not touch agent")
}

// TestBareRemote_PushSyncRoundTrip is the core happy path: repo A writes a
// fact on its agent branch, pushes, the agent branch is promoted to main
// on the remote, repo B syncs, sees the fact.
//
// Under the post-rework model agents push to agent/<host>, never to main.
// "Sync" pulls origin/main and replays the agent on top, so the fact must
// reach origin/main before B can see it — RemoteHandle.MergeIntoMain
// simulates the remote-side merge-to-main step that the blueprint
// describes:
//
//  1. A.Branch("agent/test").Write(fact).Push()   — A pushes its agent branch
//  2. remote.MergeIntoMain("agent/test", ...)     — promote agent → main on origin
//  3. B.Branch("agent/test").Sync()               — B pulls origin/main, replays agent
//  4. B.Branch("agent/test").Head().Fact(...)...  — B sees the promoted fact
func TestBareRemote_PushSyncRoundTrip(t *testing.T) {
	t.Log("Scenario: A writes fact on agent/test, pushes; remote promotes to main; B connects and syncs; B sees the fact")
	sb := NewStoryboard(t)
	remote := sb.BareRemote("origin")

	a := sb.Repo("a").Connect(remote)
	aAgent := a.Branch("agent/test")
	aAgent.Write("kb/shared.md", Fact("shared"), "A writes shared fact")
	aAgent.Push()

	remote.MergeIntoMain("agent/test", "promote A's agent to main")

	b := sb.Repo("b").Connect(remote)
	bAgent := b.Branch("agent/test")
	bAgent.Sync()

	bAgent.Head().Fact("kb/shared.md").MustExist()
}

// #651: a push into a Storyboard remote starts no detached git
// auto-maintenance in it. The push goes through the product (Connect's
// first round, then Push), i.e. go-git's file:// transport, which execs the
// real git-receive-pack. The oracle is git's own trace2 event log, not a
// timing: receive-pack either starts `git maintenance` or it does not. A
// started child takes objects/maintenance.lock before it detaches and drops
// it later, which is what TestMultiRepo_WritesAndRemotesIndependent's byte
// snapshot of the remote and t.TempDir's cleanup raced.
// SABOTAGE: drop gitfixture.NoAutoMaintenance from gitfixture.InitBare -> the
// trace names a maintenance child -> red on every run.
func TestBareRemote_PushStartsNoAutoMaintenance(t *testing.T) {
	trace := filepath.Join(t.TempDir(), "trace2.json")
	t.Setenv("GIT_TRACE2_EVENT", trace)
	sb := NewStoryboard(t)
	remote := sb.BareRemote("origin")
	a := sb.Repo("a").Connect(remote)
	agent := a.Branch("agent/test")
	agent.Write("kb/one.md", Fact("one"), "a fact to push")
	require.True(t, agent.Push().Pushed, "fixture: the push reached the remote")

	raw, err := os.ReadFile(trace)
	require.NoError(t, err)
	var receivePacks, maintenance int
	for _, line := range strings.Split(string(raw), "\n") {
		var ev struct {
			Event string   `json:"event"`
			Argv  []string `json:"argv"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil || (ev.Event != "start" && ev.Event != "child_start") {
			continue
		}
		joined := strings.Join(ev.Argv, " ")
		if ev.Event == "start" && strings.Contains(joined, "receive-pack") {
			receivePacks++
		}
		if strings.Contains(joined, "maintenance run") || strings.Contains(joined, "gc --auto") {
			maintenance++
			t.Logf("auto-maintenance started: %s", joined)
		}
	}
	require.NotZero(t, receivePacks, "positive control: the pushes ran a traced git-receive-pack")
	require.Zero(t, maintenance, "a push into the remote started a detached git auto-maintenance")
}
