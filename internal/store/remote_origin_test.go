package store

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func openOriginTestService(t *testing.T) *Service {
	t.Helper()
	svc, err := Open(filepath.Join(t.TempDir(), "repo.db"))
	require.NoError(t, err)
	t.Cleanup(func() { svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{"README.md": "x"}, "agent/test"))
	return svc
}

// With no origin injected and no remotes row, GetRemote reports "no origin" —
// the contract every caller branches on.
func TestGetRemote_NoOriginIsNilNil(t *testing.T) {
	svc := openOriginTestService(t)
	got, err := svc.Remote().GetRemote("origin")
	require.NoError(t, err)
	require.Nil(t, got)
}

// An injected origin surfaces through GetRemote exactly as a stored row did.
func TestSetOrigin_SurfacesThroughGetRemote(t *testing.T) {
	svc := openOriginTestService(t)
	svc.SetOrigin(&Origin{URL: "https://x.test/kb.git", Branch: "master", AuthMethod: "token", AuthToken: "s3cret"})

	got, err := svc.Remote().GetRemote("origin")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "https://x.test/kb.git", got.URL)
	require.Equal(t, "master", got.Branch)
	require.Equal(t, "token", got.AuthMethod)
	require.Equal(t, "s3cret", got.AuthToken)
}

// Status stays in the repo and is assembled onto the injected origin, so a
// caller sees ONE record.
func TestGetRemote_MergesInjectedOriginWithLocalStatus(t *testing.T) {
	svc := openOriginTestService(t)
	svc.SetOrigin(&Origin{URL: "https://x.test/kb.git", Branch: "main"})
	require.NoError(t, svc.Remote().RecordSyncError("origin", "boom"))

	got, err := svc.Remote().GetRemote("origin")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "https://x.test/kb.git", got.URL)
	require.NotNil(t, got.LastStatus)
	require.Equal(t, "error", *got.LastStatus)
	require.NotNil(t, got.LastError)
	require.Equal(t, "boom", *got.LastError)
}

// Clearing the origin makes the repo originless again even though the status
// row survives — "has an origin" is control.db's answer, not the row's.
func TestSetOrigin_NilClears(t *testing.T) {
	svc := openOriginTestService(t)
	svc.SetOrigin(&Origin{URL: "https://x.test/kb.git", Branch: "main"})
	require.NoError(t, svc.Remote().RecordSyncError("origin", "boom"))

	svc.SetOrigin(nil)
	got, err := svc.Remote().GetRemote("origin")
	require.NoError(t, err)
	require.Nil(t, got)
}

// ConfigureRemote wires the git remote so go-git can fetch/push by name. It is
// idempotent: the second call is a no-op. Named distinctly from
// TestConfigureRemote_IsIdempotent in remote_reconcile_test.go, which covers
// the same idempotency at the unexported rh.configureRemote layer; this one
// exercises the new Service-level exported wrapper.
func TestServiceConfigureRemote_IsIdempotent(t *testing.T) {
	svc := openOriginTestService(t)
	require.NoError(t, svc.ConfigureRemote("https://x.test/kb.git", "main", "agent/test"))
	require.NoError(t, svc.ConfigureRemote("https://x.test/kb.git", "main", "agent/test"))
}

// ConfigureRemote must error, not panic, when the repo is not initialised
// (rh.repo is nil until InitRepo/OpenRepo — DB-only mode). Task 4 calls this
// at open time, exactly where DB-only mode is plausible. Regresses the
// unguarded delegate to rh.configureRemote, which dereferences rh.repo.Config()
// with no nil check.
func TestServiceConfigureRemote_ErrorsWhenRepoNotInitialised(t *testing.T) {
	svc, err := Open(filepath.Join(t.TempDir(), "repo.db"))
	require.NoError(t, err)
	t.Cleanup(func() { svc.Close() })

	err = svc.ConfigureRemote("https://x.test/kb.git", "main", "agent/test")
	require.Error(t, err, "ConfigureRemote must error rather than panic when the repo is not initialised")
}

// SetUpstreamBranch must treat a status-only row (empty url, written by
// updateRemoteStatus/updateRemotePushStatus once a status row can be created
// without a prior SetRemote) the same as "no remote configured" — not as a
// connection to rewrite. Otherwise it falls through to
// configureRemote("", ...), wiring up a git remote with an empty URL instead
// of reporting that there is none. legacyRemoteRow draws the same line one
// function over; this regresses the inconsistency.
func TestSetUpstreamBranch_StatusOnlyRowIsNotConfigured(t *testing.T) {
	svc := openOriginTestService(t)
	require.NoError(t, svc.Remote().RecordSyncError("origin", "boom"))

	err := svc.Remote().SetUpstreamBranch("origin", "main", "agent/test")
	require.Error(t, err, "a status-only row must not be treated as a configured remote")
	require.Contains(t, err.Error(), "no remote",
		"error must report no remote configured, not fail some other way")
}
