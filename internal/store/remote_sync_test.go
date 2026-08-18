package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A push that a server REFUSES explains itself on the sideband, prefixed
// "remote: ". Dropping those lines left a reader with go-git's summary alone —
// "pre-receive hook declined" — which names the mechanism and never the
// reason: protected branch, push rule, unsigned commit, oversized file.
func TestRemoteSays(t *testing.T) {
	cases := []struct {
		name     string
		sideband string
		want     string
	}{
		{
			// The real answer from GitLab for a refused initial commit: prose
			// plus a list of remedies, wrapped. Folding it onto one line is
			// what turned guidance into a run-on.
			name:     "multi-line guidance keeps its shape",
			sideband: "remote: GitLab:\nremote: You cannot push the initial commit because the default branch is\nremote: protected and your role does not allow it.\nremote: To resolve this, either:\nremote: - Ask a Maintainer to push the initial commit.\n",
			want:     "\nGitLab:\nYou cannot push the initial commit because the default branch is\nprotected and your role does not allow it.\nTo resolve this, either:\n- Ask a Maintainer to push the initial commit.",
		},
		{
			name:     "gitlab frames its reason with banner lines",
			sideband: "remote: \nremote: GitLab: You are not allowed to push code to protected branches on this project.\nremote: \n",
			want:     "\nGitLab: You are not allowed to push code to protected branches on this project.",
		},
		{
			name:     "progress uses carriage returns, not newlines",
			sideband: "remote: Resolving deltas:   0%\rremote: Resolving deltas: 100%\rremote: GitLab: Commit message does not match\n",
			want:     "\nResolving deltas:   0%\nResolving deltas: 100%\nGitLab: Commit message does not match",
		},
		{
			name:     "a repeated line is said once",
			sideband: "remote: same\nremote: same\nremote: same\n",
			want:     "\nsame",
		},
		{
			name:     "silence adds nothing to the error",
			sideband: "",
			want:     "",
		},
		{
			name:     "blank sideband adds nothing either",
			sideband: "remote: \nremote:\n\n",
			want:     "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := remoteSays(c.sideband); got != c.want {
				t.Fatalf("remoteSays()\n got: %q\nwant: %q", got, c.want)
			}
		})
	}
}

// A sync that WE cancelled established nothing about the remote, so it must not
// overwrite what the last completed sync established.
//
// The reported case: creating a repo against a remote runs ActivateSync, which
// cancels the reconcile loop to restart it. A tick whose fetch was in flight
// died with `context canceled`, and that message was written to the remotes
// table and broadcast — so a create that fully succeeded (agent branch pushed,
// repo open) showed a red "sync failed — Sync: fetch: Get ...: context
// canceled" on the repo screen underneath a green last-push line.
//
// The distinction is the CALLER's context, not the error's text: a per-fetch
// netTimeout firing is a real failure of a real attempt and still gets
// recorded. Only ctx.Err() != nil means "we stopped this ourselves".
func TestSync_CallerCancellationLeavesStatusAlone(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	svc.SetOrigin(&Origin{URL: "https://example.invalid/repo.git", Branch: "main"})
	require.NoError(t, svc.ConfigureRemote("https://example.invalid/repo.git", "main", "agent/test"))

	ri := svc.Remote().(*remoteIndex)
	// What the last completed sync established.
	require.NoError(t, ri.updateRemoteStatus("origin", "ok", nil))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, syncErr := ri.Sync(ctx, "agent/test", nil)
	require.Error(t, syncErr, "a cancelled sync still reports the cancellation to its caller")

	rem, err := ri.GetRemote("origin")
	require.NoError(t, err)
	require.NotNil(t, rem.LastStatus)
	require.Equal(t, "ok", *rem.LastStatus, "the cancelled sync overwrote the last established status")
	require.Nil(t, rem.LastError)
}

// Push writes its own status on the same rule, and is cancelled by the same
// restart: ActivateSync cancels the loop between the Sync and the Push.
func TestPush_CallerCancellationLeavesStatusAlone(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	svc.SetOrigin(&Origin{URL: "https://example.invalid/repo.git", Branch: "main"})
	require.NoError(t, svc.ConfigureRemote("https://example.invalid/repo.git", "main", "agent/test"))

	ri := svc.Remote().(*remoteIndex)
	require.NoError(t, ri.updateRemotePushStatus("origin", "ok", nil))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, pushErr := ri.Push(ctx, "agent/test", nil)
	require.Error(t, pushErr)

	rem, err := ri.GetRemote("origin")
	require.NoError(t, err)
	require.NotNil(t, rem.LastPushStatus)
	require.Equal(t, "ok", *rem.LastPushStatus, "the cancelled push overwrote the last established status")
	require.Nil(t, rem.LastPushError)
}

// A caller DEADLINE is not a cancellation: recoverFromOrigin gives the startup
// reconcile a 15s budget (repos/builder.go), and a remote that does not answer
// inside it has really failed. That failure must be persisted — it is the only
// record a user gets that their remote was unreachable at boot.
//
// This is the line abandonedByCaller has to hold: "we stopped this ourselves"
// (ActivateSync cancelling the loop) versus "the attempt ran out of time".
// Testing ctx.Err() != nil alone conflates them and loses the second.
func TestSync_CallerDeadlineIsRecorded(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	svc.SetOrigin(&Origin{URL: "https://example.invalid/repo.git", Branch: "main"})
	require.NoError(t, svc.ConfigureRemote("https://example.invalid/repo.git", "main", "agent/test"))

	ri := svc.Remote().(*remoteIndex)
	require.NoError(t, ri.updateRemoteStatus("origin", "ok", nil))

	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	_, syncErr := ri.Sync(ctx, "agent/test", nil)
	require.Error(t, syncErr)

	rem, err := ri.GetRemote("origin")
	require.NoError(t, err)
	require.NotNil(t, rem.LastStatus)
	require.Equal(t, "error", *rem.LastStatus, "a remote that ran out of time still failed, and the user must be told")
}
