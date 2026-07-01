//go:build contract

package storytests

import (
	"strings"
	"testing"

	"knomit/internal/config"
	"knomit/test/testenv"
)

// Cell — symptom #4 RESIDUAL: silent-anonymous auth downgrade on auth
// RESOLUTION failure.
//
// This is a DISTINCT path from contract_token_expiry_test.go. That cell covers
// a well-formed token that the SERVER rejects (401) — which reaches Sync()'s
// fetch and is recorded. This cell covers a credential that cannot even be
// RESOLVED locally (an SSH auth method with no readable key): the failure
// happens inside makeRemoteAuthFn BEFORE Sync() is ever called, so the buggy
// code (internal/repos/sync.go) logged "using anonymous" and returned nil,
// letting the reconcile proceed anonymously.
//
// Scenario:
//   - An HTTP remote that permits ANONYMOUS access (no RequireBasicAuth). This
//     is the trap: an anonymous fetch SUCCEEDS, so a silent downgrade to nil
//     auth would produce a green "ok" status and the broken credential would
//     never surface.
//   - The repo connects anonymously (clean clone), then its persisted origin
//     record is reconfigured to auth_method="ssh" with NO key path available
//     (record has no key, fallback cfg has none, Deps.KeyPath is empty). So the
//     next auth resolution fails with "ssh auth requires a key path".
//   - We then drive one PRODUCTION reconcile through the real auth factory via
//     RepoInstance.ActivateSync (the startSync closure calls makeRemoteAuthFn →
//     resolveAuthWithOrigin, exactly the reconcile-loop path). This is what the
//     background tick / PUT /api/v1/{repo}/origin does.
//
// CONTRACT: after that tick the persisted remote status is an ERROR whose
// last_error names the auth/credential resolution failure — it must NOT be
// status="ok" (which would prove the silent-anonymous downgrade), and the
// reconcile call itself must return a non-nil error.
//
// RED before the fix (makeRemoteAuthFn swallows the resolution error, returns
// nil, Sync succeeds anonymously → status "ok", ActivateSync returns nil).
// GREEN after the fix (the resolution error is propagated, persisted via
// RecordSyncError, and returned from ActivateSync).
func TestContract_AuthResolutionFailure_SurfacesVisibleError(t *testing.T) {
	sb := testenv.NewStoryboard(t)

	// An HTTP remote that allows anonymous access — no RequireBasicAuth. A
	// silent downgrade to anonymous would SUCCEED against it, hiding the bug.
	remote := sb.BareRemoteHTTP("origin")
	remote.WriteMain("kb/seed.md", testenv.Fact("seed"), "seed on origin")

	// Connect anonymously so the initial clone succeeds and origin is
	// registered in the remotes table.
	repo := sb.Repo("a").
		WithRemoteAuth(config.RemoteAuthConfig{}).
		Connect(remote)

	// Reconfigure the PERSISTED origin record to demand SSH auth with no key
	// available anywhere. remoteAuthFromRecord copies auth_method from the
	// record over the (empty) fallback, so the next resolution takes the ssh
	// branch and fails ("ssh auth requires a key path") — a RESOLUTION failure,
	// not a server rejection.
	if _, err := repo.RawSQL().Exec(
		`UPDATE remotes SET auth_method = 'ssh', auth_token = '' WHERE name = 'origin'`,
	); err != nil {
		t.Fatalf("setup: could not reconfigure origin auth to ssh: %v", err)
	}

	// Drive one production reconcile through the real auth factory. ActivateSync
	// → startSync → makeRemoteAuthFn(cfg.Remote, keyPath) → resolveAuthWithOrigin
	// is exactly the reconcile-loop / origin-refresh path that harbours the bug.
	syncErr := repo.Instance().ActivateSync(remote.URL())

	st := repo.RemoteStatus()
	if st == nil {
		t.Fatal("CONTRACT VIOLATION (symptom #4: silent-anonymous auth downgrade): " +
			"origin remote record disappeared")
	}

	// The failure must be VISIBLE in persisted status — NOT "ok".
	if st.LastStatus == nil || *st.LastStatus != "error" {
		t.Fatalf("CONTRACT VIOLATION (symptom #4: silent-anonymous auth downgrade): "+
			"an unresolvable SSH credential did not surface — expected persisted "+
			"last_status \"error\", got %q (ActivateSync returned err=%v). The reconcile "+
			"silently downgraded to anonymous and synced against a remote that permits "+
			"anonymous access, masking the broken credential.",
			statusStr(st), syncErr)
	}
	if st.LastError == nil || *st.LastError == "" {
		t.Fatalf("CONTRACT VIOLATION (symptom #4: silent-anonymous auth downgrade): "+
			"last_status is \"error\" but last_error is empty — the reason for the "+
			"auth-resolution failure is not visible (ActivateSync err=%v)", syncErr)
	}
	// last_error must name the credential/auth resolution problem, not some
	// unrelated failure.
	le := strings.ToLower(*st.LastError)
	if !strings.Contains(le, "auth") && !strings.Contains(le, "ssh") && !strings.Contains(le, "key") {
		t.Fatalf("CONTRACT VIOLATION (symptom #4: silent-anonymous auth downgrade): "+
			"last_error does not name an auth/credential resolution failure: %q", *st.LastError)
	}
	// The reconcile call itself must have returned the error, not swallowed it.
	if syncErr == nil {
		t.Fatalf("CONTRACT VIOLATION (symptom #4: silent-anonymous auth downgrade): "+
			"ActivateSync returned nil against an unresolvable SSH credential — the auth "+
			"failure was silently swallowed / downgraded to anonymous")
	}
	t.Logf("auth-resolution failure surfaced: last_status=%q last_error=%q activate_err=%v",
		*st.LastStatus, *st.LastError, syncErr)
}
