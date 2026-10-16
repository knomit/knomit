package repos

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// A forge repository that is not empty but has no main (someone else's
// project, on master or trunk, or only tags) is not_this_fleet for the master
// too (#529 B1): "empty" means the remote advertises no ref at all. Nothing
// is pushed there and nothing changes here. A truly empty one is still the
// master's to move onto (TestFleetMove_TheHubMovesOntoAnEmptyForge).
// SABOTAGE: advEmpty answers true whenever main is missing (the old rule)
// -> the master moves the fleet onto trunk-only.git -> red.
func TestFleetFollow_ARepositoryWithoutMainIsNotEmpty(t *testing.T) {
	ctx := context.Background()
	f := newForgeFleet(t, nil)
	refs := func(bare string) string {
		out, err := exec.Command("git", "-C", bare, "for-each-ref", "--format=%(refname) %(objectname)").Output()
		require.NoError(t, err)
		return string(out)
	}
	theirs := func(name, branch, tag string) (string, string) {
		bare, url := emptyBare(t, f.dir, name)
		work := t.TempDir()
		runGit(t, "", "clone", bare, work)
		require.NoError(t, os.WriteFile(filepath.Join(work, "README.md"), []byte("someone else's project\n"), 0o644))
		runGit(t, work, "add", "-A")
		runGit(t, work, "-c", "user.email=o@o", "-c", "user.name=o", "commit", "-q", "-m", "theirs")
		if branch != "" {
			runGit(t, work, "push", "-q", "origin", "HEAD:refs/heads/"+branch)
		}
		if tag != "" {
			runGit(t, work, "tag", tag)
			runGit(t, work, "push", "-q", "origin", "refs/tags/"+tag)
		}
		require.NotEmpty(t, refs(bare), "fixture: %s holds a ref", name)
		return bare, url
	}
	refused := func(t *testing.T, bare, url, name string) {
		t.Helper()
		before := refs(bare)
		_, err := f.master.FollowFleetHost(ctx, OriginSpec{URL: url})
		status, code := fleetCode(t, err)
		require.Equal(t, http.StatusUnprocessableEntity, status, name)
		require.Equal(t, "not_this_fleet", code, name)
		require.ErrorContains(t, err, "EMPTY forge repository", name)
		require.Equal(t, before, refs(bare), "nothing was pushed to %s", name)
		hosted, err := f.master.fleetHostAtMain(f.mri)
		require.NoError(t, err)
		require.NotContains(t, hosted, url, "the host record does not name %s", name)
	}
	for _, tc := range []struct{ name, branch, tag string }{
		{"trunk-only.git", "trunk", ""},
		{"master-only.git", "master", ""},
		{"tag-only.git", "", "v1"},
	} {
		bare, url := theirs(tc.name, tc.branch, tc.tag)
		refused(t, bare, url, tc.name)
		o, err := f.master.originOf(f.mri)
		require.NoError(t, err)
		require.Equal(t, f.url, o.URL, "%s: the master's origin is unchanged", tc.name)
	}
	// From the hub too: the same check.
	ri := hubOf(t, f)
	bare, url := theirs("trunk-from-hub.git", "trunk", "")
	refused(t, bare, url, "trunk-from-hub.git")
	o, err := f.master.originOf(ri)
	require.NoError(t, err)
	require.Nil(t, o, "the hub is still the hub")
}

// "Update access" on the MASTER of a forge-hosted fleet changes the
// credential and nothing else (#529 S3, N-a): no seed, no host record, no
// publish. The proof is a forge whose main moved past the master's (a direct
// write, trusted as fetched): a move would seed main there and be refused
// publish_refused; a credential change is stored (the attach then restarts
// the origin round, which fetches and pushes as every round does). The
// stored credential unchanged is a named no-op that runs nothing, not even
// the probe (the forge is away then), and the stored method with no token
// keeps the stored token (#529 N-b).
// SABOTAGE: send the master's same-URL follow down followForge (the move)
// -> publish_refused -> red; drop the unchanged check -> the probe of the
// forge that is away fails -> red.
func TestFleetFollow_TheMasterChangesOnlyItsCredential(t *testing.T) {
	ctx := context.Background()
	f := newForgeFleet(t, nil)
	require.NoError(t, f.master.writeFleetHost(ctx, f.mri, []string{f.url}))
	require.NoError(t, f.master.publishFleetMainNow(ctx, f.mri))
	work := t.TempDir()
	runGit(t, "", "clone", f.bare, work)
	require.NoError(t, os.WriteFile(filepath.Join(work, "README.md"), []byte("written on the forge\n"), 0o644))
	runGit(t, work, "add", "-A")
	runGit(t, work, "-c", "user.email=h@h", "-c", "user.name=h", "commit", "-q", "-m", "a direct write")
	runGit(t, work, "push", "-q", "origin", "main")

	res, err := f.master.FollowFleetHost(ctx, OriginSpec{URL: f.url, AuthMethod: "basic", AuthToken: "master:pw1"})
	require.NoError(t, err, "a credential change, not a move")
	require.Equal(t, FleetFollowCredential, res.Change)
	o, err := f.master.originOf(f.mri)
	require.NoError(t, err)
	require.Equal(t, f.url, o.URL)
	require.Equal(t, "basic", o.AuthMethod)
	require.Equal(t, "master:pw1", o.AuthToken)
	require.NotNil(t, res.FollowsHost)
	require.True(t, *res.FollowsHost)
	hosted, err := f.master.fleetHostAtMain(f.mri)
	require.NoError(t, err)
	require.Equal(t, []string{f.url}, hosted, "the host record is as it was")

	// Unchanged: the stored method with no token keeps the stored token, so
	// nothing changes and nothing runs.
	away := f.bare + ".away"
	require.NoError(t, os.Rename(f.bare, away))
	t.Cleanup(func() { _ = os.Rename(away, f.bare) })
	for _, spec := range []OriginSpec{{AuthMethod: "basic"}, {URL: f.url}, {}} {
		res, err = f.master.FollowFleetHost(ctx, spec)
		require.NoError(t, err, "%+v", spec)
		require.Equal(t, FleetFollowUnchanged, res.Change, "%+v", spec)
	}
	o, err = f.master.originOf(f.mri)
	require.NoError(t, err)
	require.Equal(t, "master:pw1", o.AuthToken, "the stored token stays")
	// A new method is probed first: with the forge away it is refused, and
	// nothing is stored.
	_, err = f.master.FollowFleetHost(ctx, OriginSpec{AuthMethod: "none"})
	require.ErrorIs(t, err, ErrOriginUnreachable)
	// #529 N-3: the origin stays attached on a credential change, so the
	// refusal must not say "origin was not attached".
	// SABOTAGE: probeFleetForge wraps ErrOriginUnreachable with %w again ->
	// the text carries it -> red.
	require.NotContains(t, err.Error(), "origin was not attached")
	require.Contains(t, err.Error(), "the forge could not be reached")
	o, err = f.master.originOf(f.mri)
	require.NoError(t, err)
	require.Equal(t, "basic", o.AuthMethod, "a refused change stores nothing")
}

// The scheme check of a credential change with no url (#529 N-d): the REST
// edge has no URL to check the method against, so repos checks it against
// the stored one.
func TestRefuseMethodForURL(t *testing.T) {
	for _, tc := range []struct {
		url, method string
		refused     bool
	}{
		{"https://forge.example/fleet.git", "ssh", true},
		{"https://forge.example/fleet.git", "token", false},
		{"ssh://git@forge.example/fleet.git", "token", true},
		{"git@forge.example:fleet.git", "basic", true},
		{"git@forge.example:fleet.git", "ssh", false},
		{"https://forge.example/fleet.git", "none", false},
	} {
		err := refuseMethodForURL(tc.url, tc.method)
		if !tc.refused {
			require.NoError(t, err, "%s %s", tc.url, tc.method)
			continue
		}
		status, code := fleetCode(t, err)
		require.Equal(t, http.StatusUnprocessableEntity, status)
		require.Equal(t, "auth_invalid", code, "%s %s", tc.url, tc.method)
	}
}

// After a move that stopped between the attach and the host record (#529
// S4), GET /fleet says so on the master, from the server's state: the host
// record names this instance (the move started at the hub: Host the fleet
// here is the repair) or a forge (move_back_to names it, and moving the fleet
// there is the repair). The repair clears it. With the record naming the
// origin, or on the hub, it is absent.
// SABOTAGE: interruptedMove never answers RecordIsHere -> the hub case
// names this instance's own hub URL as move_back_to -> red.
func TestFleetStatus_AnInterruptedMove(t *testing.T) {
	ctx := context.Background()
	attach := func(t *testing.T, m *Manager, ri *RepoInstance, url string) {
		t.Helper()
		decided, err := m.ResolveOrigin(ctx, OriginSpec{URL: url})
		require.NoError(t, err)
		require.NoError(t, ri.WithRead(func(svc *store.Service) { decided.Branch = svc.UpstreamBranch() }))
		_, err = m.Send(ctx, ri, AttachOrigin(decided))
		require.NoError(t, err)
	}

	t.Run("from the hub", func(t *testing.T) {
		f := newForgeFleet(t, nil)
		ri := hubOf(t, f)
		st, err := f.master.FleetStatus(ctx)
		require.NoError(t, err)
		require.Nil(t, st.InterruptedMove, "the hub")
		attach(t, f.master, ri, f.url) // the move's attach, then the crash
		st, err = f.master.FleetStatus(ctx)
		require.NoError(t, err)
		require.NotNil(t, st.FollowsHost)
		require.False(t, *st.FollowsHost)
		require.NotNil(t, st.InterruptedMove)
		require.True(t, st.InterruptedMove.RecordIsHere, "main still says this instance hosts the fleet")
		require.Empty(t, st.InterruptedMove.MoveBackTo, "never this instance's own hub URL")

		require.NoError(t, hostHere(ctx, f.master), "the repair: Host the fleet here")
		st, err = f.master.FleetStatus(ctx)
		require.NoError(t, err)
		require.Nil(t, st.InterruptedMove)
		require.True(t, *st.FollowsHost)
	})

	t.Run("from a forge", func(t *testing.T) {
		f := newForgeFleet(t, nil)
		require.NoError(t, f.master.writeFleetHost(ctx, f.mri, []string{f.url}))
		require.NoError(t, f.master.publishFleetMainNow(ctx, f.mri))
		st, err := f.master.FleetStatus(ctx)
		require.NoError(t, err)
		require.Nil(t, st.InterruptedMove, "the record names the origin")
		_, url := emptyBare(t, f.dir, "moved.git")
		attach(t, f.master, f.mri, url)
		st, err = f.master.FleetStatus(ctx)
		require.NoError(t, err)
		require.False(t, *st.FollowsHost)
		require.NotNil(t, st.InterruptedMove)
		require.False(t, st.InterruptedMove.RecordIsHere)
		require.Equal(t, f.url, st.InterruptedMove.MoveBackTo, "the forge the record names")

		res, err := f.master.FollowFleetHost(ctx, OriginSpec{URL: st.InterruptedMove.MoveBackTo})
		require.NoError(t, err, "the repair: move the fleet back where main says it is")
		require.Equal(t, FleetFollowMoved, res.Change)
		require.Nil(t, res.InterruptedMove)
		require.True(t, *res.FollowsHost)
	})
}

// A copy whose push the old host refused because the host's copy has an
// origin now, while it follows that old host, is told the fleet moved (#529
// S5): last_error_fleet_moved, from the recorded error and follows_host.
// Following the host, or another error, is not flagged.
// SABOTAGE: drop the follows_host condition -> the following copy is
// flagged -> red.
func TestFleetStatus_LastErrorFleetMoved(t *testing.T) {
	ctx := context.Background()
	f := newForgeFleet(t, nil)
	refused := errors.New(`push: knomit: this copy of the fleet repository "fleet" ` + FleetPushRefusalHasOrigin + `, not to this instance`)
	flagged := func() bool {
		st, err := f.master.FleetStatus(ctx)
		require.NoError(t, err)
		return st.LastErrorFleetMoved
	}
	require.NoError(t, f.master.writeFleetHost(ctx, f.mri, []string{f.url}))
	require.NoError(t, f.master.publishFleetMainNow(ctx, f.mri))
	require.NoError(t, recordFleetAttempt(f.master.ControlDB(), refused, time.Now()))
	require.False(t, flagged(), "this copy follows the host")

	require.NoError(t, f.master.writeFleetHost(ctx, f.mri, []string{"https://elsewhere.example/git/fleet"}))
	require.NoError(t, f.master.publishFleetMainNow(ctx, f.mri))
	require.NoError(t, recordFleetAttempt(f.master.ControlDB(), refused, time.Now()))
	require.True(t, flagged(), "the old host refused it, and main says the fleet moved")
	require.NoError(t, recordFleetAttempt(f.master.ControlDB(), errors.New("push: connection reset"), time.Now()))
	require.False(t, flagged(), "another error")
}

// Every refusal POST /fleet/follow answers is credential-free (#529 S2): a
// token in the URL's user slot (https://<token>@host/…, which Go's HTTP
// client quotes without redacting and go-git quotes whole) never reaches the
// error text or the FleetError the REST layer writes, on the follow's
// unreachable path and on the move's publish_refused path, nor the move's
// log line when its publish fails.
// SABOTAGE: FollowFleetHost returns err without credentialFreeErr ->
// LEAKTOK in the text -> red; drop credentialFreeText from the move's
// publish warning -> red.
func TestFleetFollow_ErrorTextsAreCredentialFree(t *testing.T) {
	ctx := context.Background()
	const tok = "LEAKTOK"
	noLeak := func(t *testing.T, err error, what string) {
		t.Helper()
		require.Error(t, err, what)
		require.NotContains(t, err.Error(), tok, "%s: Error()", what)
		var fe *FleetError
		if errors.As(err, &fe) {
			require.NotContains(t, fe.Msg, tok, "%s: the FleetError's Msg", what)
		}
	}

	t.Run("unreachable", func(t *testing.T) {
		f := newForgeFleet(t, nil)
		srv := httptest.NewServer(http.NotFoundHandler()) // plain http behind an https URL
		defer srv.Close()
		hostPort := strings.TrimPrefix(srv.URL, "http://")
		_, err := f.master.FollowFleetHost(ctx, OriginSpec{URL: "https://" + tok + "@" + hostPort + "/fleet.git"})
		noLeak(t, err, "unreachable")
		require.ErrorIs(t, err, ErrOriginUnreachable, "the cause is kept")
		require.Contains(t, err.Error(), "https://"+hostPort+"/fleet.git", "the URL is still named, without its userinfo")
	})

	t.Run("publish_refused", func(t *testing.T) {
		f := newForgeFleet(t, nil)
		hubOf(t, f)
		_, url := emptyBare(t, f.dir, "moved.git")
		setFleetHooks(t, fleetCreateHooks{seedPublish: func() error {
			return errors.New(`unexpected client error: unexpected requesting "https://` + tok + `@forge.example/fleet.git/git-receive-pack" status code: 500`)
		}})
		_, err := f.master.FollowFleetHost(ctx, OriginSpec{URL: url})
		noLeak(t, err, "publish_refused")
		_, code := fleetCode(t, err)
		require.Equal(t, "publish_refused", code)
		require.Contains(t, err.Error(), "https://forge.example/fleet.git", "the URL is still named, without its userinfo")
	})

	t.Run("the move's publish warning", func(t *testing.T) {
		f := newForgeFleet(t, nil)
		hubOf(t, f)
		_, url := emptyBare(t, f.dir, "moved.git")
		var buf bytes.Buffer
		orig := log.Logger
		log.Logger = zerolog.New(&buf).Level(zerolog.WarnLevel)
		t.Cleanup(func() { log.Logger = orig })
		setFleetHooks(t, fleetCreateHooks{beforePublish: func() error {
			return errors.New(`Post "https://` + tok + `@forge.example/fleet.git/git-receive-pack": EOF`)
		}})
		_, err := f.master.FollowFleetHost(ctx, OriginSpec{URL: url})
		require.NoError(t, err, "a failed publish after the record is the origin round's to retry")
		require.Contains(t, buf.String(), "main is not published on the forge yet", "fixture: the warning ran")
		require.NotContains(t, buf.String(), tok)
		require.Contains(t, buf.String(), "https://forge.example/fleet.git")
	})
}

// credentialFreeText strips the userinfo of every URL in a text, the
// unparseable fallback included, and leaves an ssh user and an scp-style
// address alone (they name an account, not a secret).
func TestCredentialFreeText(t *testing.T) {
	for in, want := range map[string]string{
		`Get "https://LEAKTOK@h:1/x.git/info/refs?service=git-upload-pack": EOF`: `Get "https://h:1/x.git/info/refs?service=git-upload-pack": EOF`,
		`a https://u:pw@h/x and https://t@k/y`:                                   `a https://h/x and https://k/y`,
		`bad https://LEAKTOK@h:port/x`:                                           `bad https://h:port/x`,
		`ssh://git:pw@h/x and git@h:x`:                                           `ssh://git@h/x and git@h:x`,
		`no url here`:                                                            `no url here`,
	} {
		require.Equal(t, want, credentialFreeText(in), in)
	}
}
