package repos

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/pki/pkitest"
	"knomit/internal/platform/fileuri"
	"knomit/internal/store"
	"knomit/internal/testsupport/testsigner"
)

// emptyBare is a new, empty bare repository under the fleet's dir (the
// local-origin root): a forge's new repository before anything is pushed.
func emptyBare(t *testing.T, dir, name string) (string, string) {
	t.Helper()
	bare := filepath.Join(dir, name)
	require.NoError(t, os.MkdirAll(bare, 0o755))
	runGit(t, "", "init", "--bare", "--initial-branch=main", bare)
	return bare, fileuri.New(bare)
}

func bareRev(t *testing.T, bare, ref string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", bare, "rev-parse", "--verify", "-q", "refs/heads/"+ref).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func bareFile(t *testing.T, bare, rev, path string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", bare, "show", rev+":"+path).Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// hubOf makes f's master the hub: its copy detaches from the forge (POST
// /fleet/host), with the listener and the address hosting needs.
func hubOf(t *testing.T, f *forgeFleet) *RepoInstance {
	t.Helper()
	f.master.deps.Cfg.TLS.Addr = "127.0.0.1:0"
	f.master.deps.Cfg.ExternalAddresses = []string{"https://master.example"}
	require.NoError(t, hostHere(context.Background(), f.master))
	ri := f.master.fleetRepo()
	o, err := f.master.originOf(ri)
	require.NoError(t, err)
	require.Nil(t, o, "fixture: the master's copy is the hub")
	hosted, err := f.master.fleetHostAtMain(ri)
	require.NoError(t, err)
	require.Equal(t, []string{"https://master.example/git/fleet"}, hosted, "fixture: main says the hub hosts the fleet")
	return ri
}

// The hub moves the fleet onto a forge (user ruling 2026-10-16) with POST
// /fleet/follow and the forge's URL: the forge is seeded with this copy's
// branch as main and as the agent branch, it becomes the origin, the host
// record on main is the forge URL, and main is published there. The master
// is then the fetching master (its copy has an origin), and new bundles
// carry the forge URL.
// SABOTAGE: attach as a member does in followForge (skip moveFleetToForge)
// -> no host record names the forge -> red; write the hub URLs instead of the
// forge's in moveFleetToForge -> red. (The seed's own test is
// TestFleetMove_BackOntoAForgeWhoseMainIsBehind: onto an EMPTY forge the
// first origin round publishes main anyway.)
func TestFleetMove_TheHubMovesOntoAnEmptyForge(t *testing.T) {
	ctx := context.Background()
	f := newForgeFleet(t, nil)
	ri := hubOf(t, f)
	bare, url := emptyBare(t, f.dir, "moved.git")

	st, err := f.master.FollowFleetHost(ctx, OriginSpec{URL: url})
	require.NoError(t, err)
	require.True(t, st.Master, "still the master")
	require.Equal(t, url, st.Hosting, "this copy syncs from the forge now")
	require.Equal(t, []string{url}, st.HostRecord, "main says the forge hosts the fleet")
	require.NotNil(t, st.FollowsHost)
	require.True(t, *st.FollowsHost)

	o, err := f.master.originOf(ri)
	require.NoError(t, err)
	require.NotNil(t, o)
	require.Equal(t, url, o.URL)
	require.NotEqual(t, "cert", o.AuthMethod, "a forge origin")
	main := localTip(t, ri, "main")
	require.Equal(t, main, bareRev(t, bare, "main"), "the forge's main is this copy's main")
	require.Equal(t, localTip(t, ri, ri.AgentBranch()), bareRev(t, bare, ri.AgentBranch()), "and the master's branch is there")
	require.Contains(t, bareFile(t, bare, "main", fact.FleetHostPath), "url: "+url, "the host record on the forge's main")
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		require.True(t, ri.isFleetMaster(svc))
	}))
	// The bundle a new member gets names the forge.
	enr, err := f.master.EnrollFleetMember(ctx, "gamma", keyLine("gamma"), "")
	require.NoError(t, err)
	require.Equal(t, url, enr.FleetURL, "a new bundle carries the forge URL")
	require.True(t, enr.Published)
	require.NotEmpty(t, bareFile(t, bare, "main", "artifacts/fleet/certs/gamma.pem"), "the enroll's leaf is published on the forge")
}

// The hub moves back onto a forge this fleet lived on before, whose main is
// now BEHIND the hub's (the hub enrolled a member meanwhile). The forge's
// main is trusted as fetched, rewinds included (trust-the-forge-main), so
// the move seeds the forge with the hub's main BEFORE attaching it: the
// first origin round then fetches a main equal to the hub's and rewinds
// nothing. Every commit main had before the move is still on main after it.
// (Without the seed this case heals inside the first round: the round
// rewinds local main to the forge's, replays nothing because the agent
// branch descends from it, and publishes again. The seed's falsifying case
// is the diverged forge below.)
func TestFleetMove_BackOntoAForgeWhoseMainIsBehind(t *testing.T) {
	ctx := context.Background()
	f := newForgeFleet(t, nil)
	ri := hubOf(t, f)
	_, err := f.master.EnrollFleetMember(ctx, "gamma", keyLine("gamma"), "")
	require.NoError(t, err)
	mainBefore := localTip(t, ri, "main")
	forgeBefore := bareRev(t, f.bare, "main")
	require.NotEqual(t, forgeBefore, mainBefore, "fixture: the forge's main is behind the hub's")

	_, err = f.master.FollowFleetHost(ctx, OriginSpec{URL: f.url})
	require.NoError(t, err)
	mainAfter := localTip(t, ri, "main")
	require.Equal(t, mainAfter, bareRev(t, f.bare, "main"), "the forge's main is this copy's main")
	out, gerr := exec.Command("git", "-C", f.bare, "merge-base", "--is-ancestor", mainBefore, mainAfter).CombinedOutput()
	require.NoError(t, gerr, "the hub's main before the move is still on main after it (nothing rewound): %s", out)
	require.Contains(t, bareFile(t, f.bare, "main", "artifacts/fleet/certs/gamma.pem"), "CERTIFICATE", "the hub-era enroll is on the forge's main")
}

// A forge whose main moved on without the hub (someone wrote to it after the
// hub left) is refused before anything changes here: the seed pushes main
// without force, the forge refuses it, and the move answers publish_refused
// with the hub as it was. Attaching first would have adopted the forge's main
// as fetched (trust-the-forge-main) and replayed the hub's branch onto it.
// SABOTAGE: drop the PublishTo seed in moveFleetToForge -> the attach runs
// and the hub has an origin -> red.
func TestFleetMove_ADivergedForgeMainIsRefusedBeforeAnyChange(t *testing.T) {
	ctx := context.Background()
	f := newForgeFleet(t, nil)
	ri := hubOf(t, f)
	work := t.TempDir()
	runGit(t, "", "clone", f.bare, work)
	require.NoError(t, os.WriteFile(filepath.Join(work, "README.md"), []byte("written past the hub\n"), 0o644))
	runGit(t, work, "add", "-A")
	runGit(t, work, "-c", "user.email=h@h", "-c", "user.name=h", "commit", "-q", "-m", "diverge")
	runGit(t, work, "push", "-q", "origin", "main")
	forgeMain := bareRev(t, f.bare, "main")
	mainBefore := localTip(t, ri, "main")

	_, err := f.master.FollowFleetHost(ctx, OriginSpec{URL: f.url})
	status, code := fleetCode(t, err)
	require.Equal(t, http.StatusConflict, status)
	require.Equal(t, "publish_refused", code)
	o, err := f.master.originOf(ri)
	require.NoError(t, err)
	require.Nil(t, o, "the hub has no origin: nothing changed here")
	require.Equal(t, mainBefore, localTip(t, ri, "main"))
	require.Equal(t, forgeMain, bareRev(t, f.bare, "main"), "the forge's main is untouched")
}

// A move whose host-record write fails leaves the copy exactly as it was:
// the hub gets no origin back (a detach), and a forge copy its old origin;
// main keeps the old host record. What the seed pushed stays on the new
// forge, which nothing follows.
// SABOTAGE: drop restoreFleetOrigin in moveFleetToForge -> the hub keeps the
// new origin -> red.
func TestFleetMove_AFailedHostRecordRestoresTheOrigin(t *testing.T) {
	ctx := context.Background()
	failHost := func(t *testing.T) {
		setFleetHooks(t, fleetCreateHooks{hostWrite: func() error { return errors.New("the disk is full (test seam)") }})
	}

	t.Run("from the hub", func(t *testing.T) {
		f := newForgeFleet(t, nil)
		ri := hubOf(t, f)
		_, url := emptyBare(t, f.dir, "moved.git")
		mainBefore := localTip(t, ri, "main")
		failHost(t)
		_, err := f.master.FollowFleetHost(ctx, OriginSpec{URL: url})
		require.ErrorContains(t, err, "back on its previous origin")
		o, err := f.master.originOf(ri)
		require.NoError(t, err)
		require.Nil(t, o, "the hub has no origin again")
		hosted, err := f.master.fleetHostAtMain(ri)
		require.NoError(t, err)
		require.Equal(t, []string{"https://master.example/git/fleet"}, hosted)
		require.Equal(t, mainBefore, localTip(t, ri, "main"))
	})

	t.Run("from a forge", func(t *testing.T) {
		f := newForgeFleet(t, nil)
		_, url := emptyBare(t, f.dir, "moved.git")
		failHost(t)
		_, err := f.master.FollowFleetHost(ctx, OriginSpec{URL: url})
		require.ErrorContains(t, err, "back on its previous origin")
		o, err := f.master.originOf(f.mri)
		require.NoError(t, err)
		require.NotNil(t, o)
		require.Equal(t, f.url, o.URL, "the old forge is the origin again")
	})
}

// The same-fleet check of a forge follow: the forge's main must carry this
// fleet's root. Another fleet's forge is not_this_fleet for everyone; an
// empty forge is the master's to seed and not_this_fleet for a member; the
// fleet's own forge with a new credential is accepted, and an empty url
// keeps the stored URL (a credential change). Nothing changes on a refusal.
// SABOTAGE: treat remoteRootDiffers as a match -> the other fleet's forge is
// attached -> red; drop the master check on an empty main -> the member
// attaches the empty forge -> red.
func TestFleetFollow_AForgeMustHoldThisFleet(t *testing.T) {
	ctx := context.Background()
	f := newForgeFleet(t, nil)
	alpha := testsigner.Named("alpha")
	am, ari := f.join(t, "alpha", alpha)

	other := pkitest.New(t)
	otherBare := filepath.Join(f.dir, "other.git")
	seedFleetForge(t, otherBare, other)
	_, emptyURL := emptyBare(t, f.dir, "empty.git")

	for _, tc := range []struct {
		name string
		m    *Manager
		url  string
		code string
	}{
		{"a member, another fleet", am, fileuri.New(otherBare), "not_this_fleet"},
		{"the master, another fleet", f.master, fileuri.New(otherBare), "not_this_fleet"},
		{"a member, an empty forge", am, emptyURL, "not_this_fleet"},
	} {
		_, err := tc.m.FollowFleetHost(ctx, OriginSpec{URL: tc.url})
		status, code := fleetCode(t, err)
		require.Equal(t, http.StatusUnprocessableEntity, status, tc.name)
		require.Equal(t, tc.code, code, tc.name)
		o, err := tc.m.originOf(tc.m.fleetRepo())
		require.NoError(t, err)
		require.Equal(t, f.url, o.URL, "%s: the origin is unchanged", tc.name)
	}

	// The fleet's own forge with a new credential: stored as given.
	_, err := am.FollowFleetHost(ctx, OriginSpec{URL: f.url, AuthMethod: "none"})
	require.NoError(t, err)
	o, err := am.originOf(ari)
	require.NoError(t, err)
	require.Equal(t, "none", o.AuthMethod)
	// An empty url on a forge copy keeps the URL and takes the credential.
	_, err = am.FollowFleetHost(ctx, OriginSpec{AuthMethod: "basic", AuthToken: "alpha:pw2"})
	require.NoError(t, err)
	o, err = am.originOf(ari)
	require.NoError(t, err)
	require.Equal(t, f.url, o.URL, "the stored URL stays")
	require.Equal(t, "basic", o.AuthMethod)
	require.Equal(t, "alpha:pw2", o.AuthToken, "the rotated credential is stored")
}

// seedFleetForge is a bare forge of fleet root (its root.crt and CRL on
// main, with the fleet ontology): newForgeFleet's seed, for another fleet.
func seedFleetForge(t *testing.T, bare string, root *pkitest.Fleet) {
	t.Helper()
	files := map[string][]byte{OntologyPath: []byte(fleetOntologyYAML(t))}
	for _, name := range []string{"root.crt", "crl.pem"} {
		b, err := os.ReadFile(filepath.Join(root.Dir, name))
		require.NoError(t, err)
		files["artifacts/fleet/"+name] = b
	}
	require.NoError(t, os.MkdirAll(bare, 0o755))
	runGit(t, "", "init", "--bare", "--initial-branch=main", bare)
	work := t.TempDir()
	runGit(t, "", "clone", bare, work)
	for p, b := range files {
		require.NoError(t, os.MkdirAll(filepath.Join(work, filepath.Dir(p)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(work, p), b, 0o644))
	}
	runGit(t, work, "add", "-A")
	runGit(t, work, "-c", "user.email=h@h", "-c", "user.name=h", "commit", "-q", "-m", "fleet")
	runGit(t, work, "push", "-q", "origin", "main")
}

// A copy whose origin is not where main says the fleet is hosted cannot
// stand down (#528 N4): a hub that followed a peer which does not host the
// fleet would leave the fleet with no master at its host. Once main names
// this copy's origin, it may.
// SABOTAGE: drop refuseNotFollowingHost from StandDown -> the root is
// deleted -> red.
func TestFleetStandDown_RefusedWhileNotFollowingTheHost(t *testing.T) {
	ctx := context.Background()
	f := newForgeFleet(t, nil)
	require.NoError(t, f.master.writeFleetHost(ctx, f.mri, []string{"https://elsewhere.example/git/fleet"}))
	require.NoError(t, f.master.publishFleetMainNow(ctx, f.mri))
	err := f.master.StandDown(ctx)
	status, code := fleetCode(t, err)
	require.Equal(t, http.StatusConflict, status)
	require.Equal(t, "not_following_host", code)
	_, serr := os.Stat(f.master.fleetRootDir())
	require.NoError(t, serr, "the root stays")

	require.NoError(t, f.master.writeFleetHost(ctx, f.mri, []string{f.url}))
	require.NoError(t, f.master.publishFleetMainNow(ctx, f.mri))
	require.NoError(t, f.master.StandDown(ctx), "this copy follows the host on main")
}
