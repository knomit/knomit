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
//   - The repo connects anonymously (clean clone), then its origin is
//     reconfigured to auth_method="ssh" with no usable SSH key anywhere (the
//     record names none, the fallback cfg names none, and the only key path the
//     manager has is the Storyboard's fake agent key, which is not a private
//     key). So the next auth resolution fails inside resolveAuth.
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
//
// # The INJECTION POINT moved; not one assertion did
//
// auth_method is control-plane state now. The credential lives in control.db,
// Manager.OriginAuth reads it from there with NO fallback to the store's legacy
// auth columns, and remoteAuthFromRecord — which used to copy auth_method off
// the store's remotes row on every resolution — is gone. So the raw UPDATE
// below no longer reconfigures anything by itself: it now establishes the
// LEGACY on-disk shape (a remotes row naming a method, holding no token), and
// the Restart() that follows is what makes it live, because the boot migration
// (Manager.migrateCredential, via gateCredential) carries that method into
// control.db and clears the store's columns.
//
// That is deliberately the stronger of the two ways to re-aim this cell. The
// alternative — writing auth_method straight into control.db — would test the
// contract alone; going through a boot tests the contract AND the migration
// that upgrading installs actually traverse, which is the path this exact
// regression came in on: while migrateCredential dropped method-only rows, a
// pre-upgrade install with an http:// origin and auth_method='ssh' stopped
// failing loudly and started resolving ANONYMOUSLY, reporting "ok".
//
// Nothing here was relaxed. All four assertions and the closing t.Log are
// unchanged, the remote still permits anonymous access so a downgrade still
// shows up as green, and the store-side legacy shape has its own unit-level
// cover in internal/repos/credential_migrate_test.go
// (TestBootCarriesAMethodOnlyLegacyRowIntoControlDB). A future reader who finds
// this cell green must not conclude the store is still consulted for auth: it
// is not, and a fix that only made THIS cell pass by re-reading the store would
// be re-introducing the two-copies-of-a-credential design that control.db
// ownership replaced.
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

	// Put the repo into the LEGACY on-disk shape: a remotes row that names an
	// auth method and holds no token. This is what a pre-upgrade install has for
	// every origin whose method was configured without a stored secret.
	if _, err := repo.RawSQL().Exec(
		`UPDATE remotes SET auth_method = 'ssh', auth_token = '' WHERE name = 'origin'`,
	); err != nil {
		t.Fatalf("setup: could not write the legacy ssh auth_method row: %v", err)
	}

	// Re-boot. THIS is what makes the method live: boot runs gateCredential →
	// migrateCredential, which carries auth_method into control.db (no Crypt
	// needed — there is no token to encrypt) and clears the store's columns.
	// Without the carry the method would be inert and the fetch would go out
	// anonymously, which is the regression this cell guards.
	repo.Restart()

	// Precondition, so the scenario cannot go vacuous: control.db must now own
	// the unresolvable method. If this ever reads "" the cell below would be
	// asserting on an anonymous sync that has no credential to fail on.
	if got := repo.OriginAuthMethod(); got != "ssh" {
		t.Fatalf("setup: the boot migration did not carry auth_method into control.db: "+
			"want %q, got %q — without it nothing unresolvable is configured and the "+
			"contract below would pass vacuously", "ssh", got)
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
