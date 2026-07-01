//go:build contract

package storytests

import (
	"strings"
	"testing"

	"knomit/internal/config"
	"knomit/internal/store"
	"knomit/internal/testenv"
	"knomit/internal/testenv/gitserver"
)

// Cell B — symptom #4: an expired/invalid token during sync must surface a
// VISIBLE error, not silently succeed nor silently downgrade to anonymous.
//
// Scenario: an HTTP remote requires HTTP Basic auth ("u"/"p"). The repo
// connects with the correct credentials (threaded via cfg.Remote /
// WithRemoteAuth into the production resolveAuth for the initial clone). After
// a successful initial sync, the token is invalidated server-side (the server
// now rejects the previously-valid credentials with 401). We trigger another
// production Sync with those (now-invalid) credentials and assert the remote's
// PERSISTED status reflects the failure.
//
// CONTRACT: after the failed reconcile, remotes.last_status == "error" with a
// non-nil last_error — the failure is visible. It must NOT be "ok" (silent
// success) and must NOT be a silent fall-back to anonymous that hides the auth
// problem.
//
// Characterization notes:
//   - remote_sync.go Sync() persists last_status/last_error on EVERY return
//     (updateRemoteStatus in the deferred block), so a 401 that reaches Sync's
//     fetch and returns an error is recorded → expected GREEN for this path.
//   - The known silent-anonymous downgrade lives in internal/repos/sync.go
//     makeRemoteAuthFn (line ~27): if resolveAuthWithOrigin RETURNS AN ERROR it
//     logs "using anonymous" and returns nil. That path is only hit when auth
//     RESOLUTION fails (e.g. an unreadable SSH key), NOT when a well-formed
//     token is rejected by the server — which is this scenario. So this cell
//     characterizes the server-rejects-valid-format-token case; the resolution-
//     failure downgrade is a separate, untested-here path.
func TestContract_TokenExpiry_SurfacesVisibleError(t *testing.T) {
	sb := testenv.NewStoryboard(t)

	remote := sb.BareRemoteHTTP("origin")
	remote.WriteMain("kb/seed.md", testenv.Fact("seed"), "seed on origin")
	remote.Fault().RequireBasicAuth("u", "p")

	// Connect with the CORRECT credentials so the initial clone/reconcile
	// authenticates and succeeds.
	repo := sb.Repo("a").
		WithRemoteAuth(config.RemoteAuthConfig{AuthMethod: "basic", User: "u", Password: "p"}).
		Connect(remote)

	// Sanity: a reconcile with valid creds succeeds and records "ok".
	if _, err := repo.SyncAuthed("agent/test"); err != nil {
		t.Fatalf("precondition: initial authed sync failed: %v", err)
	}
	if st := repo.RemoteStatus(); st == nil || st.LastStatus == nil || *st.LastStatus != "ok" {
		t.Fatalf("precondition: expected last_status \"ok\" after valid sync, got %v", statusStr(repo.RemoteStatus()))
	}

	// Invalidate the token: the server now returns 401 on the fetch
	// advertisement — modelling a token that has expired / been revoked.
	remote.Fault().SetStatus(gitserver.ClassInfoRefs, 401)

	// Trigger another reconcile with the SAME (now-invalid) credentials.
	_, syncErr := repo.SyncAuthed("agent/test")

	st := repo.RemoteStatus()
	if st == nil {
		t.Fatal("CONTRACT VIOLATION (symptom #4): origin remote record disappeared")
	}

	// The failure must be VISIBLE in persisted status.
	if st.LastStatus == nil || *st.LastStatus != "error" {
		t.Fatalf("CONTRACT VIOLATION (symptom #4): expired token did not surface — "+
			"expected persisted last_status \"error\", got %q (sync returned err=%v). "+
			"A now-invalid token silently succeeded or was swallowed.",
			statusStr(st), syncErr)
	}
	if st.LastError == nil || *st.LastError == "" {
		t.Fatalf("CONTRACT VIOLATION (symptom #4): last_status is \"error\" but "+
			"last_error is empty — the reason for the auth failure is not visible")
	}
	// The Sync call itself must also have returned the error (not swallowed it).
	if syncErr == nil {
		t.Fatalf("CONTRACT VIOLATION (symptom #4): Sync returned nil against a 401 "+
			"remote — the auth failure was silently swallowed / downgraded to anonymous")
	}
	t.Logf("token expiry surfaced: last_status=%q last_error=%q sync_err=%v",
		*st.LastStatus, *st.LastError, syncErr)
}

func statusStr(r *store.Remote) string {
	if r == nil || r.LastStatus == nil {
		return "<nil>"
	}
	return strings.TrimSpace(*r.LastStatus)
}
