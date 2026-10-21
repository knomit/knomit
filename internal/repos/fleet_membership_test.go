package repos

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/pki"
	"knomit/internal/pki/pkitest"
	"knomit/internal/store"
	"knomit/internal/store/remotesync"
	"knomit/internal/testsupport/testsigner"
)

// namedMember starts an instance named id whose key file holds the key it
// signs with (testsigner.Named(id)), as a real instance's does: the leaf the
// master's enroll issues for that key is both the bundle a join checks and
// the leaf rule B checks the record's signature against.
func (f *forgeFleet) namedMember(t *testing.T, id string) *Manager {
	t.Helper()
	home := filepath.Join(f.dir, id)
	require.NoError(t, os.MkdirAll(home, 0o755))
	key, err := testsigner.WriteNamedKey(home, id)
	require.NoError(t, err)
	m := New(context.Background(), Deps{
		Cfg: config.Config{Home: home, OntologyRoot: "kb", LocalOriginRoot: f.dir,
			TLS: config.TLSConfig{Dir: filepath.Join(home, "pki")}},
		AgentBranch: "agent/" + id,
		AgentID:     id,
		Signer:      testsigner.Named(id),
		KeyPath:     key,
		Machine:     Options{Synchronous: true, CrashBackoff: testCrashBackoff},
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { m.Close() })
	return m
}

func keyLine(id string) string {
	return string(ssh.MarshalAuthorizedKey(testsigner.Named(id).PublicKey()))
}

// pemTypes is the block types of a PEM bundle, in order.
func pemTypes(raw string) []string {
	var out []string
	for rest := []byte(raw); ; {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			return out
		}
		out = append(out, blk.Type)
	}
}

// The whole membership on a forge, against #524's fetching master: the
// master enrolls alpha (leaf on main, bundle = leaf + root + CRL and nothing
// else), alpha joins with it (one push), the master's round admits and merges
// it, alpha leaves (one push, then nothing of the fleet is left on alpha, its
// lens kept), the master revokes it (CRL on main and installed here) and
// decommissions (repository, trust and root gone).
// SABOTAGE: publishMasterWrite returns nil without publishing -> the leaf
// never reaches the forge's main -> red; RevokeFleetMember skips InstallCRL
// -> the installed CRL Number stays 1 -> red.
func TestFleetMembership_ForgeEnrollJoinAdmitLeaveRevokeDecommission(t *testing.T) {
	ctx := context.Background()
	f := newForgeFleet(t, nil)
	runGit(t, f.bare, "config", "core.logAllRefUpdates", "always")
	masterKey := filepath.Join(f.dir, "master-key")
	require.NoError(t, os.MkdirAll(masterKey, 0o700))
	mk, err := testsigner.WriteNamedKey(masterKey, "master")
	require.NoError(t, err)
	f.root.Install(t, f.root.Enroll(t, "master@"+pki.AgentEmailDomain, mk), f.master.deps.Cfg.TLS.Dir)

	enr, err := f.master.EnrollFleetMember(ctx, "alpha", keyLine("alpha"), "")
	require.NoError(t, err)
	require.True(t, enr.Published, enr.PublishError)
	require.Equal(t, []string{"CERTIFICATE", "CERTIFICATE", "X509 CRL"}, pemTypes(enr.Bundle), "leaf, root, CRL: nothing else")
	require.True(t, strings.HasPrefix(enr.Bundle, pki.BundleHeader+"\nurl: "+f.url+"\nagent: alpha\nroot: "+enr.RootFingerprint+"\nserial: "+enr.Serial+"\n"), "the v1 document header: %q", enr.Bundle)
	require.Equal(t, f.url, enr.FleetURL, "a forge-hosted fleet's bundle names the forge URL")
	parsed, err := pki.SplitBundle([]byte(enr.Bundle))
	require.NoError(t, err)
	require.Equal(t, f.url, parsed.FleetURL, "and carries it")
	require.True(t, f.onForgeMain(t, "artifacts/fleet/certs/alpha.pem"), "the leaf is on the forge's main")
	require.True(t, f.onForgeMain(t, "artifacts/fleet/issued/"+enr.Serial+".pem"), "and its issuance record")

	alpha := f.namedMember(t, "alpha")
	_, err = alpha.EnrollFleetMember(ctx, "beta", keyLine("beta"), "")
	_, code := fleetCode(t, err)
	require.Equal(t, "not_registered", code, "a standalone instance enrolls nobody")
	joined, err := alpha.JoinFleet(ctx, FleetJoinSpec{Bundle: []byte(enr.Bundle)})
	require.NoError(t, err)
	st := joined
	require.Equal(t, FleetRegistered, st.State)
	require.NotEmpty(t, enr.RootFingerprint)
	require.Equal(t, enr.RootFingerprint, joined.RootFingerprint, "the join names the root it installed (review N7)")
	require.Equal(t, 2, pushCount(t, f.bare, "agent/alpha"), "the mount's round, then the join's one push")

	_, err = alpha.EnrollFleetMember(ctx, "beta", keyLine("beta"), "")
	status, code := fleetCode(t, err)
	require.Equal(t, http.StatusConflict, status)
	require.Equal(t, "not_master", code, "a member enrolls nobody")

	round(t, f.mri)
	require.Equal(t, f.rev(t, "agent/alpha"), localTip(t, f.mri, "agent/alpha"), "the master admitted alpha's join")
	settleMerger(t, f.mri)
	round(t, f.mri)
	require.True(t, f.onForgeMain(t, memberRecordPath("kb", "alpha")), "alpha's record is on main")
	syncFleet(t, alpha)
	st, err = alpha.FleetStatus(ctx)
	require.NoError(t, err)
	require.Equal(t, fact.MemberActive, st.RecordState)

	_, err = f.master.LeaveFleet(ctx)
	_, code = fleetCode(t, err)
	require.Equal(t, "is_master", code, "the master decommissions; it does not leave")

	kb := createRepo(t, alpha, "kb")
	fleetUID := alpha.fleetRepo().UID()
	_, err = alpha.CreateLens(ctx, Lens{Name: "reads-fleet", WriteUID: kb.UID(), Reads: []LensRead{{RepoUID: fleetUID}}})
	require.NoError(t, err)
	before := pushCount(t, f.bare, "agent/alpha")
	left, err := alpha.LeaveFleet(ctx)
	require.NoError(t, err)
	require.True(t, left.Pushed, left.PushError)
	require.Equal(t, before+1, pushCount(t, f.bare, "agent/alpha"), "one push of the left record")
	rec, err := runGitOut(f.bare, "show", "agent/alpha:"+memberRecordPath("kb", "alpha"))
	require.NoError(t, err)
	require.Contains(t, rec, "state: left")
	require.Nil(t, alpha.fleetRepo())
	require.Equal(t, []string{"kb"}, alpha.Names())
	l, _, err := alpha.LensRegistry().Get("reads-fleet")
	require.NoError(t, err)
	for _, r := range l.Reads {
		require.NotEqual(t, fleetUID, r.RepoUID, "dropped from the lens")
	}
	_, err = os.Stat(filepath.Join(alpha.deps.Cfg.TLS.Dir, pki.InstanceCertFile))
	require.ErrorIs(t, err, os.ErrNotExist, "uninstalled")

	_, err = f.master.RevokeFleetMember(ctx, "master", "")
	_, code = fleetCode(t, err)
	require.Equal(t, "own_id", code)
	_, err = f.master.RevokeFleetMember(ctx, "nobody", "")
	status, code = fleetCode(t, err)
	require.Equal(t, http.StatusNotFound, status)
	require.Equal(t, "not_enrolled", code)
	rv, err := f.master.RevokeFleetMember(ctx, "alpha", "left the fleet")
	require.NoError(t, err)
	require.True(t, rv.Published, rv.PublishError)
	require.Empty(t, rv.InstallError)
	require.Equal(t, "2", rv.CRLNumber)
	require.True(t, f.onForgeMain(t, "artifacts/fleet/revocations/"+rv.Serial+".txt"), "the revocation statement is on main")
	members, err := f.master.FleetMembers(ctx)
	require.NoError(t, err)
	states := map[string]string{}
	for _, mem := range members {
		states[mem.Agent] = mem.State
	}
	require.Equal(t, fact.MemberRevoked, states["alpha"], "derived from the CRL on main")
	installed, err := pki.LoadCRL(filepath.Join(f.master.deps.Cfg.TLS.Dir, pki.CRLFile))
	require.NoError(t, err)
	require.Equal(t, int64(2), installed.Number.Int64(), "the new CRL is installed here")
	log, err := os.ReadFile(filepath.Join(f.master.fleetRootDir(), pki.RevokedLogFile))
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(log), "\n"), "one line in the local revocation log")
	localCRL, err := os.ReadFile(filepath.Join(f.master.fleetRootDir(), pki.CRLFile))
	require.NoError(t, err)
	mainCRL, err := runGitOut(f.bare, "show", "main:artifacts/fleet/crl.pem")
	require.NoError(t, err)
	require.Equal(t, mainCRL, string(localCRL), "the local CRL copy is main's new CRL")
	_, err = f.master.RevokeFleetMember(ctx, "alpha", "")
	_, code = fleetCode(t, err)
	require.Equal(t, "already_revoked", code)

	mainBefore := f.rev(t, "main")
	st, err = f.master.DecommissionFleet(ctx)
	require.NoError(t, err)
	require.Equal(t, FleetStandalone, st.State)
	require.Nil(t, f.master.fleetRepo())
	_, err = os.Stat(f.master.fleetRootDir())
	require.ErrorIs(t, err, os.ErrNotExist, "the root key is deleted")
	_, err = os.Stat(filepath.Join(f.master.deps.Cfg.TLS.Dir, pki.RootCertFile))
	require.ErrorIs(t, err, os.ErrNotExist, "the trust material is uninstalled")
	// User ruling 7: the forge keeps its copy, and its main no longer names
	// a master, so another holder of the root key could claim the fleet.
	require.NotEqual(t, mainBefore, f.rev(t, "main"), "the decommission published the marker's deletion")
	require.False(t, f.onForgeMain(t, fact.FleetMasterPath), "the forge's main names no master")
}

func runGitOut(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	return string(out), err
}

// The root's move on a forge (R3.2): GET on the master returns the files of
// <home>/fleet, PUT installs them on a member (which is then the master),
// a second PUT is refused, and the old master (its copy has an origin) stands
// down and is a member again. PUT refuses unknown files, a missing key and a
// key that is not the certificate's.
// SABOTAGE: drop the non-empty <home>/fleet check in InstallFleetRoot -> the second PUT
// is accepted -> red.
func TestFleetMembership_ForgeRootMovesAndTheOldMasterStandsDown(t *testing.T) {
	ctx := context.Background()
	f := newForgeFleet(t, nil)
	enr, err := f.master.EnrollFleetMember(ctx, "alpha", keyLine("alpha"), "")
	require.NoError(t, err)
	alpha := f.namedMember(t, "alpha")
	_, err = alpha.JoinFleet(ctx, FleetJoinSpec{Bundle: []byte(enr.Bundle)})
	require.NoError(t, err)
	syncFleet(t, alpha)

	_, err = alpha.FleetRootFiles()
	_, code := fleetCode(t, err)
	require.Equal(t, "no_root", code)
	root, err := f.master.FleetRootFiles()
	require.NoError(t, err)
	wantKey, err := os.ReadFile(filepath.Join(f.master.fleetRootDir(), pki.RootKeyFile))
	require.NoError(t, err)
	require.Equal(t, string(wantKey), root.Files[pki.RootKeyFile], "the root key itself")
	require.Contains(t, root.Files, pki.RootCertFile)
	wantID, err := pki.RootID(f.root.Root.Cert)
	require.NoError(t, err)
	require.Equal(t, wantID, root.RootFingerprint)

	other := pkitest.New(t)
	otherKey, err := os.ReadFile(filepath.Join(other.Dir, pki.RootKeyFile))
	require.NoError(t, err)
	for _, tc := range []struct {
		files map[string]string
		code  string
	}{
		{map[string]string{pki.RootKeyFile: root.Files[pki.RootKeyFile], pki.RootCertFile: root.Files[pki.RootCertFile], "notes.txt": "x"}, "unknown_file"},
		{map[string]string{pki.RootCertFile: root.Files[pki.RootCertFile]}, "root_incomplete"},
		{map[string]string{pki.RootKeyFile: string(otherKey), pki.RootCertFile: root.Files[pki.RootCertFile]}, "root_invalid"},
	} {
		_, err := alpha.InstallFleetRoot(tc.files)
		_, code := fleetCode(t, err)
		require.Equal(t, tc.code, code)
		_, err = os.Stat(alpha.fleetRootDir())
		require.ErrorIs(t, err, os.ErrNotExist, "%s: nothing is installed", tc.code)
	}

	got, err := alpha.InstallFleetRoot(root.Files)
	require.NoError(t, err)
	require.Equal(t, wantID, got.RootFingerprint)
	ari := alpha.fleetRepo()
	// User ruling 6: holding the root is not being the master while main
	// names another instance (#531 re-review S4).
	require.NoError(t, ari.WithRead(func(svc *store.Service) {
		require.False(t, ari.isFleetMaster(svc), "alpha holds the root, but main names the master")
		require.True(t, alpha.holdsFleetRoot(svc))
	}))
	_, err = alpha.InstallFleetRoot(root.Files)
	status, code := fleetCode(t, err)
	require.Equal(t, http.StatusConflict, status)
	require.Equal(t, "root_present", code)

	require.NoError(t, f.master.StandDown(ctx), "the old master's copy has an origin: it may stand down")
	_, err = os.Stat(f.master.fleetRootDir())
	require.ErrorIs(t, err, os.ErrNotExist)
	require.NoError(t, f.mri.WithRead(func(svc *store.Service) { require.False(t, f.mri.isFleetMaster(svc), "the old master is a member") }))
	err = f.master.StandDown(ctx)
	_, code = fleetCode(t, err)
	require.Equal(t, "no_root", code)
	require.False(t, f.onForgeMain(t, fact.FleetMasterPath), "the stand-down published the marker's deletion")
	// alpha's next round: main names no master, so it claims the role.
	round(t, ari)
	st, err := alpha.FleetStatus(ctx)
	require.NoError(t, err)
	require.True(t, st.Master, "alpha claimed the role: %+v", st)
	require.Equal(t, "alpha", st.ActingMaster)
}

// The hub stands down FIRST (user ruling 2026-10-16): no precondition. The
// create wrote the master marker (artifacts/fleet/master) naming this
// instance; the stand-down's last act as master deletes it on main (a fleet
// with no acting master), the host record unchanged; then the root key goes.
// From then on the hub takes no member push (FleetPushRefusalStoodDown) and
// does no master work: enroll is refused, and its round claims no marker.
// SABOTAGE: StandDown skips deleteFleetMaster -> main still names it -> red;
// drop the hub's not-master refusal in FleetPushRefusal -> "" -> red.
func TestFleetMembership_TheHubStandsDownFirst(t *testing.T) {
	ctx := context.Background()
	m := newFleetHost(t, t.TempDir(), nil)
	_, err := m.CreateFleet(ctx, FleetCreateSpec{})
	require.NoError(t, err)
	ri := m.fleetRepo()
	before, err := m.fleetHostAtMain(ri)
	require.NoError(t, err)
	require.NotEmpty(t, before)
	named, err := m.fleetMasterAtMain(ri)
	require.NoError(t, err)
	require.Equal(t, m.AgentID(), named, "setup: the create names this instance the acting master")
	require.Empty(t, m.FleetPushRefusal(ri), "setup: the hub takes pushes while it is the master")

	require.NoError(t, m.StandDown(ctx))
	_, err = os.Stat(filepath.Join(m.fleetRootDir(), pki.RootKeyFile))
	require.True(t, os.IsNotExist(err), "the root key is gone: %v", err)
	named, err = m.fleetMasterAtMain(ri)
	require.NoError(t, err)
	require.Empty(t, named, "main has no acting master")
	after, err := m.fleetHostAtMain(ri)
	require.NoError(t, err)
	require.Equal(t, before, after, "the host record is unchanged")

	require.Contains(t, m.FleetPushRefusal(ri), FleetPushRefusalStoodDown)
	_, err = m.EnrollFleetMember(ctx, "zed", keyLine("zed"), "")
	_, code := fleetCode(t, err)
	require.Equal(t, "not_master", code)
	_ = ri.WithRead(func(svc *store.Service) { ri.fleetHostRound(ctx, svc) })
	named, err = m.fleetMasterAtMain(ri)
	require.NoError(t, err)
	require.Empty(t, named, "a stood-down hub claims no marker")
	st, err := m.FleetStatus(ctx)
	require.NoError(t, err)
	require.False(t, st.Master)
	require.Empty(t, st.ActingMaster)
}

// A fleet created before the marker has none: the root holder's round writes
// it once, naming itself; a marker naming anyone is never overwritten there.
// SABOTAGE: drop claimFleetMasterOnce from fleetHostRound -> no marker -> red.
func TestFleetMaster_LegacyFleetIsClaimedOnceByTheRootHolder(t *testing.T) {
	ctx := context.Background()
	m := newFleetHost(t, t.TempDir(), nil)
	_, err := m.CreateFleet(ctx, FleetCreateSpec{})
	require.NoError(t, err)
	ri := m.fleetRepo()
	require.NoError(t, m.commitFleetMasterDeletion(ctx, ri, m.AgentID()))
	require.NoError(t, m.publishFleetMainNow(ctx, ri))
	named, err := m.fleetMasterAtMain(ri)
	require.NoError(t, err)
	require.Empty(t, named, "setup: a fleet with no marker")

	_ = ri.WithRead(func(svc *store.Service) { ri.fleetHostRound(ctx, svc) })
	require.NoError(t, m.publishFleetMainNow(ctx, ri))
	named, err = m.fleetMasterAtMain(ri)
	require.NoError(t, err)
	require.Equal(t, m.AgentID(), named)

	// The master's own branch never names another agent: a marker naming
	// "other" written there is refused (host-record) BEFORE main moves, and
	// GET /fleet says why (judgeOwnRange).
	require.NoError(t, m.writeFleetArtifacts(ctx, ri, map[string]string{fact.FleetMasterPath: fact.RenderFleetMaster("other")}, "other"))
	mainBefore := localTip(t, ri, "main")
	err = m.publishFleetMainNow(ctx, ri)
	require.ErrorIs(t, err, remotesync.ErrOwnCommitRefused)
	require.Contains(t, err.Error(), "host-record")
	require.Equal(t, mainBefore, localTip(t, ri, "main"), "main did not move")
	st, err := m.FleetStatus(ctx)
	require.NoError(t, err)
	require.Contains(t, st.LastError, remotesync.OwnCommitRefusedText)
	require.Contains(t, st.LastError, "host-record")

	// Someone else's marker on main (here written onto main directly, as a
	// copy that follows another master's main would hold it) stays.
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		_, _, werr := svc.Facts().BatchWriteFacts(ctx, svc.Remote().UpstreamBranch(), map[string]string{fact.FleetMasterPath: fact.RenderFleetMaster("other")}, nil, "other", "created")
		require.NoError(t, werr)
	}))
	_ = ri.WithRead(func(svc *store.Service) { ri.fleetHostRound(ctx, svc) })
	named, err = m.fleetMasterAtMain(ri)
	require.NoError(t, err)
	require.Equal(t, "other", named)
}

// POST /fleet/host's guard (R3.3): a member keeps its origin;
// the master may detach once [tls].addr and external_addresses are set.
// SABOTAGE: guardFleetDetach returns nil for a member -> red.
func TestFleetMembership_DetachGuard(t *testing.T) {
	ctx := context.Background()
	f := newForgeFleet(t, nil)
	enr, err := f.master.EnrollFleetMember(ctx, "alpha", keyLine("alpha"), "")
	require.NoError(t, err)
	alpha := f.namedMember(t, "alpha")
	_, err = alpha.JoinFleet(ctx, FleetJoinSpec{Bundle: []byte(enr.Bundle)})
	require.NoError(t, err)

	// PR #528 S5: GET /fleet names the member's own sync interval (the
	// origin record's, 300 s by default), which is when it next sees main.
	// SABOTAGE: drop SyncIntervalSeconds from FleetStatus -> 0 -> red.
	ast, err := alpha.FleetStatus(ctx)
	require.NoError(t, err)
	var want int
	require.NoError(t, alpha.fleetRepo().WithRead(func(svc *store.Service) {
		r, rerr := svc.Remote().GetRemote("origin")
		require.NoError(t, rerr)
		want = int(loopInterval(r) / time.Second)
	}))
	require.Positive(t, want)
	require.Equal(t, want, ast.SyncIntervalSeconds)

	err = hostHere(ctx, alpha)
	status, code := fleetCode(t, err)
	require.Equal(t, http.StatusConflict, status)
	require.Equal(t, "member_keeps_origin", code)
	origin, err := alpha.hasOrigin(alpha.fleetRepo())
	require.NoError(t, err)
	require.True(t, origin, "the refused detach left the member's origin")

	_, code = fleetCode(t, hostHere(ctx, f.master))
	require.Equal(t, "listener_required", code)
	f.master.deps.Cfg.TLS.Addr = "127.0.0.1:0"
	_, code = fleetCode(t, hostHere(ctx, f.master))
	require.Equal(t, "addresses_required", code)
	f.master.deps.Cfg.ExternalAddresses = []string{"https://master.example"}
	require.NoError(t, hostHere(ctx, f.master), "the master hosts the fleet here")
	origin, err = f.master.hasOrigin(f.mri)
	require.NoError(t, err)
	require.False(t, origin, "the master's copy is the hub now")
	mst, err := f.master.FleetStatus(ctx)
	require.NoError(t, err)
	require.Zero(t, mst.SyncIntervalSeconds, "the hub has no origin round")
}

// localRound runs ONE round of ri's origin-less local reconcile, as the Sync
// stage's loop does (with the fleet hub's host-record duty).
func localRound(t *testing.T, ri *RepoInstance) {
	t.Helper()
	svc, release, err := ri.Acquire()
	require.NoError(t, err)
	defer release()
	runLocalReconcileLoop(context.Background(), svc, ri.Name(), ri.agentBranch, time.Hour, nil, nil, nil, true, ri.fleetHostRound, nil)
}

// The hub re-asserts the host record in every round of its local reconcile
// (PR #527 SF1). A detach whose record write fails is answered as done (the
// detach stood) and leaves main without the hub's record; the next round,
// here the Sync stage's own round after a rebuild, writes it and main follows.
// A round with nothing to change commits nothing, and a change of
// external_addresses is followed by the next round, written without the
// address's credentials (SF4).
// SABOTAGE: make fleetHostRound return at once -> main never holds the
// record -> red; pass nil for it in the Sync stage -> the rebuild's round
// does not write it -> red; HostFleetHere returns the write's error again
// -> red.
func TestFleetHost_TheHubReassertsItsRecordEachRound(t *testing.T) {
	ctx := context.Background()
	f := newForgeFleet(t, nil)
	f.master.deps.Cfg.TLS.Addr = "127.0.0.1:0"
	f.master.deps.Cfg.ExternalAddresses = []string{"https://master.example"}
	fails := 1
	setFleetHooks(t, fleetCreateHooks{hostWrite: func() error {
		if fails > 0 {
			fails--
			return errors.New("the disk is full (test seam)")
		}
		return nil
	}})
	require.NoError(t, hostHere(ctx, f.master), "the detach stood: it is answered as done")
	require.Zero(t, fails, "fixture: the record write failed once")
	ri := f.master.Get("fleet")
	origin, err := f.master.hasOrigin(ri)
	require.NoError(t, err)
	require.False(t, origin, "fixture: the master's copy is the hub")
	hosted, err := f.master.fleetHostAtMain(ri)
	require.NoError(t, err)
	require.Empty(t, hosted, "fixture: main holds no host record after the failed write")

	// The next round: the Sync stage's own first round, here at a restart
	// (Synchronous: one round, inline).
	deps := f.master.deps
	deps.Cfg.Git.LocalReconcileInterval = time.Hour // the fixture's 0 runs no local loop at all
	require.NoError(t, f.master.Close())
	m := New(context.Background(), deps)
	require.NoError(t, m.Start())
	t.Cleanup(func() { m.Close() })
	m.fleetBootWg.Wait()
	ri = m.Get("fleet")
	require.NotNil(t, ri, "fixture: the fleet repository mounts again")
	hosted, err = m.fleetHostAtMain(ri)
	require.NoError(t, err)
	require.Equal(t, []string{"https://master.example/git/fleet"}, hosted, "the next round wrote the hub's record and main followed")

	tip := localTip(t, ri, "main")
	localRound(t, ri)
	require.Equal(t, tip, localTip(t, ri, "main"), "a current record: the round commits nothing")

	m.deps.Cfg.ExternalAddresses = []string{"https://user:s3cret-token@moved.example:8443/"}
	localRound(t, ri)
	hosted, err = m.fleetHostAtMain(ri)
	require.NoError(t, err)
	require.Equal(t, []string{"https://moved.example:8443/git/fleet"}, hosted, "the round follows the addresses, without credentials")
	// GET /fleet shows no URL's credentials either (SF4).
	st, err := m.FleetStatus(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{"https://moved.example:8443/git/fleet"}, st.FleetURLs, "the hub's own URLs, without credentials")
	require.Equal(t, st.FleetURLs, st.HostRecord)
}

// sameFleetURL is the follows_host comparison: credentials, the case of the
// scheme and host, a default port and a trailing slash do not count; the
// path does. The rows are testdata/same_fleet_url.json, the ONE table the
// browser's port (web/src/fleetText.ts sameFleetURL, read by
// web/src/connectNarrow.test.tsx) is held to as well; the typed side is
// trimmed, as followFleetHost trims it and the page trims what is typed.
// SABOTAGE: compare the strings exactly -> the equal rows are red.
func TestSameFleetURL(t *testing.T) {
	raw, err := os.ReadFile("testdata/same_fleet_url.json")
	require.NoError(t, err)
	var rows []struct {
		Typed  string `json:"typed"`
		Stored string `json:"stored"`
		Same   bool   `json:"same"`
	}
	require.NoError(t, json.Unmarshal(raw, &rows))
	require.GreaterOrEqual(t, len(rows), 30, "fixture: the shared table is read")
	for _, row := range rows {
		require.Equal(t, row.Same, sameFleetURL(strings.TrimSpace(row.Typed), row.Stored), "%q vs %q", row.Typed, row.Stored)
	}
}

// credentialFree drops a URL's userinfo, a token in it included (PR #527
// SF4), keeps an ssh:// user (it is the login, not a secret; PR #528 nit 1),
// strips the userinfo of a URL that does not parse too (PR #528 S6), and
// leaves what is not a URL with a scheme as it is.
// SABOTAGE: return u unchanged -> the token rows are red; return u when
// url.Parse fails -> the unparseable rows keep u:tok@ -> red; drop the ssh
// arm -> ssh://git@ loses its user -> red.
func TestCredentialFree(t *testing.T) {
	for in, want := range map[string]string{
		"https://x-access-token:ghp_abc123@github.com/org/fleet.git": "https://github.com/org/fleet.git",
		"https://oauth2@gitlab.example/org/fleet":                    "https://gitlab.example/org/fleet",
		"https://hub.example:7443/git/fleet":                         "https://hub.example:7443/git/fleet",
		"git@github.com:org/fleet.git":                               "git@github.com:org/fleet.git",
		"ssh://git:pw@host/org/fleet":                                "ssh://git@host/org/fleet",
		"ssh://git@github.com/a/f.git":                               "ssh://git@github.com/a/f.git",
		"https://u:tok@exa mple.com/x":                               "https://exa mple.com/x",
		"HTTPS://u:t@k@exa%zzmple.com/x?y=1":                         "HTTPS://exa%zzmple.com/x?y=1",
		"http://[::1:u:tok@host/x":                                   "http://host/x",
	} {
		require.Equal(t, want, credentialFree(in), in)
	}
}

// Activity's order (PR #527 N4): newest first by the full timestamp, a
// sub-second one included, then at the same instant by a stable key (the
// kind, then agent, serial, branch, tip, detail), whatever order the sources
// were read in: every permutation of the input sorts the same.
// SABOTAGE: sort by At alone (stable, the source order) -> the reversed input
// keeps joined above revoked -> red; drop the kind key -> red.
func TestSortFleetEvents_SameSecondIsNotSourceOrder(t *testing.T) {
	sec := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	want := []FleetEvent{
		{At: sec.Add(1500 * time.Millisecond), Kind: FleetEventRefused, Agent: "bravo", Tip: "t2"},
		{At: sec.Add(300 * time.Millisecond), Kind: FleetEventEnrolled, Agent: "charlie", Serial: "0c"},
		{At: sec, Kind: FleetEventRevoked, Agent: "bravo", Serial: "0b"},
		{At: sec, Kind: FleetEventAdmitted, Agent: "bravo", Tip: "t1"},
		{At: sec, Kind: FleetEventJoined, Agent: "alpha"},
		{At: sec, Kind: FleetEventJoined, Agent: "bravo"},
	}
	for _, in := range [][]int{{0, 1, 2, 3, 4, 5}, {5, 4, 3, 2, 1, 0}, {4, 5, 3, 0, 2, 1}} {
		events := make([]FleetEvent, len(in))
		for i, k := range in {
			events[i] = want[k]
		}
		sortFleetEvents(events)
		require.Equal(t, want, events, "input order %v", in)
	}
}

// Review N5: the fleet repository's detach (check and detach) takes the
// fleet lock, so a stand-down cannot run between the master check and the
// detach.
// SABOTAGE: HostFleetHere without lockFleetOp -> it returns at once -> red.
func TestFleetMembership_DetachTakesTheFleetLock(t *testing.T) {
	ctx := context.Background()
	m := newFleetHost(t, t.TempDir(), nil)
	_, err := m.CreateFleet(ctx, FleetCreateSpec{})
	require.NoError(t, err)
	waiting := make(chan struct{}, 1)
	setFleetHooks(t, fleetCreateHooks{contended: func() { waiting <- struct{}{} }})
	m.fleetOpMu.Lock()
	done := make(chan struct{})
	go func() { _ = hostHere(ctx, m); close(done) }()
	select {
	case <-waiting:
	case <-done:
		m.fleetOpMu.Unlock()
		t.Fatal("the fleet repository's detach ran without the fleet lock")
	case <-time.After(30 * time.Second):
		t.Fatal("neither waited nor returned")
	}
	m.fleetOpMu.Unlock()
	<-done
}

// Review N5, follow-up: the master check and the detach are ONE step under
// the fleet lock. The detach waits on the lock; while it waits a stand-down
// lands (the root key goes), and only then is the lock released. The check
// therefore runs after the stand-down: the copy is no longer the master's,
// the detach is refused and the origin stays.
// SABOTAGE: take the lock AFTER guardFleetDetach (check outside, detach
// inside) -> the check passed before the stand-down -> the origin is
// detached -> red.
func TestFleetMembership_DetachChecksAndDetachesAsOneStep(t *testing.T) {
	ctx := context.Background()
	f := newForgeFleet(t, nil)
	f.master.deps.Cfg.TLS.Addr = "127.0.0.1:0"
	f.master.deps.Cfg.ExternalAddresses = []string{"https://master.example"}
	standDown := make(chan struct{})
	setFleetHooks(t, fleetCreateHooks{contended: func() {
		require.NoError(t, os.Remove(filepath.Join(f.master.fleetRootDir(), pki.RootKeyFile)))
		close(standDown)
	}})
	f.master.fleetOpMu.Lock()
	errc := make(chan error, 1)
	go func() { errc <- hostHere(ctx, f.master) }()
	select {
	case <-standDown:
	case err := <-errc:
		f.master.fleetOpMu.Unlock()
		t.Fatalf("the detach ran without waiting for the fleet lock: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the detach neither waited nor returned")
	}
	f.master.fleetOpMu.Unlock()
	_, code := fleetCode(t, <-errc)
	require.Equal(t, "member_keeps_origin", code, "the check ran after the stand-down")
	origin, err := f.master.hasOrigin(f.mri)
	require.NoError(t, err)
	require.True(t, origin, "the refused detach left the origin")
}

// #518 R4: the generic create's "creating this repository again will adopt
// that branch" is not shown for a fleet, whose create is not re-run to adopt
// it (its rollback deletes the branch on the forge). Review N2: a join's
// clone (also through the fleet door) names the join's undo, never the
// create's rollback.
// SABOTAGE: ignore the fleet argument -> the fleet text offers to adopt ->
// red; fleetUndoText ignores the mode -> the join names the create -> red.
func TestAgentBranchAlreadyPushed_FleetIsNotOfferedToAdopt(t *testing.T) {
	base := context.Canceled
	at := func(spec CreateSpec) string {
		err := agentBranchAlreadyPushed(base, "https://forge/x.git", "agent/a", "main", fleetUndoText(spec))
		require.ErrorIs(t, err, context.Canceled)
		return err.Error()
	}
	repo := at(CreateSpec{Mode: "initialize"})
	create := at(CreateSpec{Mode: "custom", fleetDoor: true})
	join := at(CreateSpec{Mode: "clone", fleetDoor: true})
	require.Contains(t, repo, "adopt that branch")
	require.NotContains(t, create, "adopt")
	require.Contains(t, create, "the fleet create's rollback deletes that branch on the forge")
	require.NotContains(t, join, "adopt")
	require.NotContains(t, join, "create")
	require.Contains(t, join, "the refused join takes that branch back")
}

// leafOf is the leaf the forge's main holds for id ("" when none).
func leafOf(t *testing.T, f *forgeFleet, id string) string {
	t.Helper()
	out, err := runGitOut(f.bare, "show", "main:artifacts/fleet/certs/"+id+".pem")
	if err != nil {
		return ""
	}
	return out
}

// B1 (PR #525 review): an enroll over an id that already has a leaf on main.
// Refused while that leaf is not on main's CRL (`enrolled`), and for a
// different key at any time (`rekey_unsupported`, §1.5: a lost or rotated key
// is a fresh start under a new id); the SAME key once its old leaf is
// revoked is the §1.5 Rejoin, admitted. A refusal changes nothing: the leaf
// on main and the local audit log are as before. Enroll, enroll, revoke never
// leaves a serial that no revoke can reach: every leaf ever on main ends on
// its CRL.
// SABOTAGE: drop the `enrolled` refusal -> the second enroll replaces serial
// A, the revoke reaches only B -> red; drop the key comparison -> the other
// key is enrolled after the revoke -> red.
func TestFleetMembership_EnrollOverAnExistingID(t *testing.T) {
	ctx := context.Background()
	f := newForgeFleet(t, nil)
	leafOnMain := func() string {
		out, err := runGitOut(f.bare, "show", "main:artifacts/fleet/certs/alpha.pem")
		require.NoError(t, err)
		return out
	}
	issuedLines := func() int {
		b, err := os.ReadFile(filepath.Join(f.master.fleetRootDir(), pki.IssuedLogFile))
		require.NoError(t, err)
		return strings.Count(string(b), "\n")
	}
	refused := func(key, want string) {
		t.Helper()
		leaf, lines := leafOnMain(), issuedLines()
		_, err := f.master.EnrollFleetMember(ctx, "alpha", keyLine(key), "")
		status, code := fleetCode(t, err)
		require.Equal(t, http.StatusConflict, status)
		require.Equal(t, want, code)
		require.Equal(t, leaf, leafOnMain(), "a refused enroll leaves the leaf on main as it was")
		require.Equal(t, lines, issuedLines(), "and logs no issuance")
	}
	crlOnMain := func() *x509.RevocationList {
		out, err := runGitOut(f.bare, "show", "main:artifacts/fleet/crl.pem")
		require.NoError(t, err)
		p := filepath.Join(t.TempDir(), "crl.pem")
		require.NoError(t, os.WriteFile(p, []byte(out), 0o644))
		crl, err := pki.LoadCRL(p)
		require.NoError(t, err)
		return crl
	}

	a, err := f.master.EnrollFleetMember(ctx, "alpha", keyLine("alpha"), "")
	require.NoError(t, err)
	require.True(t, a.Published, a.PublishError)
	// alpha joins and the master admits it: a §1.5 rejoin is of a member,
	// whose record the replacing leaf renews (replace_when: leaf-renewal).
	alpha := f.namedMember(t, "alpha")
	_, err = alpha.JoinFleet(ctx, FleetJoinSpec{Bundle: []byte(a.Bundle)})
	require.NoError(t, err)
	round(t, f.mri)
	settleMerger(t, f.mri)
	round(t, f.mri)
	require.True(t, f.onForgeMain(t, memberRecordPath("kb", "alpha")), "alpha's record is on main")

	refused("alpha", "enrolled")
	refused("alpha-other-key", "rekey_unsupported")

	rv, err := f.master.RevokeFleetMember(ctx, "alpha", "rejoin")
	require.NoError(t, err)
	require.Equal(t, a.Serial, rv.Serial, "the revoke reaches the first leaf")

	refused("alpha-other-key", "rekey_unsupported")

	b, err := f.master.EnrollFleetMember(ctx, "alpha", keyLine("alpha"), "")
	require.NoError(t, err, "the same key with its old leaf on the CRL is the rejoin")
	require.True(t, b.Published, b.PublishError)
	require.NotEqual(t, a.Serial, b.Serial)

	rv, err = f.master.RevokeFleetMember(ctx, "alpha", "left again")
	require.NoError(t, err)
	require.Equal(t, b.Serial, rv.Serial)

	// An id enrolled and revoked that never joined has no record for a new
	// leaf to renew: never_joined, before anything is issued or written, so
	// the master's branch never holds a commit main cannot take.
	g, err := f.master.EnrollFleetMember(ctx, "gamma", keyLine("gamma"), "")
	require.NoError(t, err)
	require.True(t, g.Published, g.PublishError)
	_, err = f.master.RevokeFleetMember(ctx, "gamma", "lost bundle")
	require.NoError(t, err)
	gammaLeaf, lines := leafOf(t, f, "gamma"), issuedLines()
	_, err = f.master.EnrollFleetMember(ctx, "gamma", keyLine("gamma"), "")
	status, code := fleetCode(t, err)
	require.Equal(t, http.StatusConflict, status)
	require.Equal(t, "never_joined", code)
	require.Equal(t, gammaLeaf, leafOf(t, f, "gamma"), "the leaf on main is as it was")
	require.Equal(t, lines, issuedLines(), "and no issuance is logged")
	require.NoError(t, f.master.publishMasterWrite(ctx, f.mri), "main still moves: nothing illegal was written")
	crl := crlOnMain()
	for _, s := range []string{a.Serial, b.Serial} {
		n, ok := new(big.Int).SetString(s, 16)
		require.True(t, ok)
		require.True(t, pki.IsRevoked(crl, n), "serial %s is on main's CRL", s)
	}
}

// The fleet URL an enroll's bundle carries is the one members use, and the
// master chooses it: a hub with one external address needs no choice; with
// several, no address is address_required listing them, a chosen one is
// used, one that is not configured is address_unknown, and a non-https one
// is address_not_https; a forge-hosted fleet's bundle names the forge, and
// an address there is address_not_applicable. Every refusal issues nothing.
// SABOTAGE: fleetURLForBundle defaulting to the first address when several
// are configured -> the address_required row passes the enroll -> red.
func TestFleetMembership_EnrollBundleNamesTheFleetURL(t *testing.T) {
	ctx := context.Background()
	two := []string{"https://hub-a.example:19279", "https://hub-b.example"}
	for _, tc := range []struct {
		name    string
		addrs   []string
		address string
		want    string
		code    string
		choices []string
	}{
		{name: "one address", addrs: []string{"https://hub.example:19279"}, want: "https://hub.example:19279/git/fleet"},
		{name: "several, none chosen", addrs: two, code: "address_required", choices: two},
		{name: "several, one chosen", addrs: two, address: two[1], want: "https://hub-b.example/git/fleet"},
		{name: "not configured", addrs: two, address: "https://elsewhere.example", code: "address_unknown", choices: two},
		{name: "not https", addrs: []string{"https://hub.example:19279", "http://10.0.0.5:19278"}, address: "http://10.0.0.5:19278", code: "address_not_https"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newFleetHost(t, t.TempDir(), func(d *Deps) { d.Cfg.ExternalAddresses = tc.addrs })
			_, err := m.CreateFleet(ctx, FleetCreateSpec{})
			require.NoError(t, err)
			enr, err := m.EnrollFleetMember(ctx, "alpha", keyLine("alpha"), tc.address)
			if tc.code != "" {
				var fe *FleetError
				require.ErrorAs(t, err, &fe)
				require.Equal(t, tc.code, fe.Code, err.Error())
				require.Equal(t, tc.choices, fe.Choices)
				require.Len(t, issuedSerials(t, m.deps.Cfg.Home), 1, "a refused enroll issues nothing")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, enr.FleetURL)
			b, err := pki.SplitBundle([]byte(enr.Bundle))
			require.NoError(t, err)
			require.Equal(t, tc.want, b.FleetURL)
		})
	}

	t.Run("forge-hosted with an address", func(t *testing.T) {
		f := newForgeFleet(t, nil)
		_, err := f.master.EnrollFleetMember(ctx, "alpha", keyLine("alpha"), "https://hub.example")
		_, code := fleetCode(t, err)
		require.Equal(t, "address_not_applicable", code)
	})
}

// A join reads the fleet URL from the bundle: one without it is refused
// before anything is installed.
func TestFleetMembership_JoinNeedsTheBundlesURL(t *testing.T) {
	dir := t.TempDir()
	_, root := seedJoinableFleet(t, filepath.Join(dir, "fleet.git"))
	m := newFleetManager(t, dir)
	bundle := root.Bundle(t, root.Enroll(t, "test-fleet@"+pki.AgentEmailDomain, m.deps.KeyPath))
	_, err := m.JoinFleet(context.Background(), FleetJoinSpec{Bundle: bundle})
	_, code := fleetCode(t, err)
	require.Equal(t, "bundle_invalid", code)
	require.ErrorContains(t, err, "names no fleet repository URL")
	_, err = os.Stat(filepath.Join(m.deps.Cfg.TLS.Dir, pki.InstanceCertFile))
	require.ErrorIs(t, err, os.ErrNotExist, "nothing was installed")
}

// joinReachErr is the one mapping join and join:check share (NEW-1): only a
// genuine refusal is bundle_refused (422); every other failure to classify
// the bundle's URL is unreachable (502). Each row is the error as
// pki.ClassifyOrigin words it, and, for the join, as ResolveOrigin wraps it.
// SABOTAGE: bundle_refused for anything not pki.ErrOriginUnreachable (the
// pre-NEW-1 mapping) -> the hang-up, handshake and trust-file rows red; drop
// the ErrOriginCertRefused arm -> the revoked row red.
func TestJoinReachErr_OnlyARefusalIsBundleRefused(t *testing.T) {
	for _, tc := range []struct {
		cause  string
		err    error
		code   string
		status int
	}{
		{"the host refused the bundle's certificate", fmt.Errorf("pki: the origin h:1 is an instance of this fleet, but: %w: remote error: tls: bad certificate", pki.ErrRefusedByPeer), "bundle_refused", 422},
		{"our verifier refused the host's certificate", fmt.Errorf("%w (h:1): %w", pki.ErrOriginCertRefused, pki.ErrRevoked), "bundle_refused", 422},
		{"no TCP connection", fmt.Errorf("%w: can't reach h:1: connection refused", pki.ErrOriginUnreachable), "unreachable", 502},
		{"a hang-up after the handshake", errors.New("pki: the origin h:1 is an instance of this fleet, but: pki: peer did not answer after the handshake: EOF"), "unreachable", 502},
		{"a handshake that failed after our verifier accepted", errors.New("pki: the origin h:1 presents a certificate of this fleet, but the handshake failed: read: connection reset by peer"), "unreachable", 502},
		{"this instance's trust files unusable", errors.New("pki: this instance's fleet trust files are unusable, so the origin h:1 cannot be judged: no CRL"), "unreachable", 502},
	} {
		var texts []string
		for _, wrapped := range []struct {
			route string
			err   error
		}{{"join:check", tc.err}, {"join", fmt.Errorf("%w: %w", ErrOriginUnreachable, tc.err)}} {
			fe := joinReachErr("h:1", wrapped.err)
			if fe.Code != tc.code || fe.Status != tc.status {
				t.Errorf("%s, %s: %d %s, want %d %s", tc.cause, wrapped.route, fe.Status, fe.Code, tc.status, tc.code)
			}
			if strings.Contains(fe.Msg, ErrOriginUnreachable.Error()) {
				t.Errorf("%s, %s: the wrap's prefix leaks into the text: %s", tc.cause, wrapped.route, fe.Msg)
			}
			texts = append(texts, fe.Msg)
		}
		if texts[0] != texts[1] {
			t.Errorf("%s: join and join:check say different words: %q vs %q", tc.cause, texts[0], texts[1])
		}
	}
}

// The bundle's url: line is unauthenticated, so the join checks its shape
// before anything dials it, on join and join:check alike (PR #527 N1): the
// internal scheme, plain http, another scheme and a URL with no host are
// bundle_invalid (422) on both routes, and nothing is installed. The shapes a
// master's enroll writes (a hub's https URL, a forge origin's) pass this
// check (they then fail later here, as there is no fleet at them).
// SABOTAGE: drop the bundleURLProblem check from joinPreflight -> the
// internal-scheme row is unreachable on join:check and clone_failed or
// unreachable on the join -> red.
func TestFleetMembership_JoinRefusesABundleURLThatIsNoFleetURL(t *testing.T) {
	dir := t.TempDir()
	_, root := seedJoinableFleet(t, filepath.Join(dir, "fleet.git"))
	m := newFleetManager(t, dir)
	bundle := root.Bundle(t, root.Enroll(t, "test-fleet@"+pki.AgentEmailDomain, m.deps.KeyPath))
	// The internal scheme, derived as the transport derives it (only pki spells it).
	internalForm, err := pki.GitURL("https://hub.example:7443/git/fleet", true)
	require.NoError(t, err)
	refused := 0
	for _, u := range []string{internalForm, "http://hub.example/git/fleet", "ftp://hub.example/fleet", "https:///git/fleet", "not a url"} {
		withURL := pkitest.WithURL(t, bundle, u)
		_, err := m.JoinFleet(context.Background(), FleetJoinSpec{Bundle: withURL})
		status, code := fleetCode(t, err)
		require.Equal(t, "bundle_invalid", code, "join %s: %v", u, err)
		require.Equal(t, http.StatusUnprocessableEntity, status)
		_, err = m.CheckFleetJoin(context.Background(), FleetJoinSpec{Bundle: withURL})
		_, code = fleetCode(t, err)
		require.Equal(t, "bundle_invalid", code, "join:check %s: %v", u, err)
		_, err = os.Stat(filepath.Join(m.deps.Cfg.TLS.Dir, pki.InstanceCertFile))
		require.ErrorIs(t, err, os.ErrNotExist, "nothing was installed")
		refused++
	}
	require.Equal(t, 5, refused)
	for _, u := range []string{"https://hub.example:7443/git/fleet", "ssh://git@forge.example/org/fleet.git", "git@forge.example:org/fleet.git", "file:///srv/fleet.git"} {
		require.Empty(t, bundleURLProblem(u), u)
	}
}

// GET /fleet/members' rows: after an enroll, the enrolled id (a leaf on main,
// no record) is a row of its own, state enrolled, at its leaf's path, since
// the enroll's commit; the master's own record keeps the since of the commit
// that wrote it. Once the leaf is revoked the enrolled row reads revoked.
// SABOTAGE: drop the enrolled rows -> one row -> red; give every row the
// tip's time -> the master's since is after the enroll began -> red.
func TestFleetMembers_EnrolledRowsAndSince(t *testing.T) {
	ctx := context.Background()
	m := newFleetHost(t, t.TempDir(), nil)
	_, err := m.CreateFleet(ctx, FleetCreateSpec{})
	require.NoError(t, err)
	time.Sleep(1100 * time.Millisecond) // commit times have whole seconds
	before := time.Now().UTC().Truncate(time.Second)
	_, err = m.EnrollFleetMember(ctx, "bravo", keyLine("bravo"), "")
	require.NoError(t, err)
	after := time.Now().UTC()

	rows, err := m.FleetMembers(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 2, "%+v", rows)
	own, enrolled := rows[0], rows[1]
	require.Equal(t, m.AgentID(), own.Agent)
	require.Equal(t, fact.MemberActive, own.State)
	require.NotNil(t, own.Since)
	require.True(t, own.Since.Before(before), "the record's since is its own commit's (%v), not the enroll's (after %v)", own.Since, before)
	require.Equal(t, "bravo", enrolled.Agent)
	require.Equal(t, MemberEnrolled, enrolled.State)
	require.Equal(t, "artifacts/fleet/certs/bravo.pem", enrolled.Path)
	require.NotNil(t, enrolled.Since)
	require.False(t, enrolled.Since.Before(before) || enrolled.Since.After(after), "the enroll's commit: %v in [%v, %v]", enrolled.Since, before, after)

	_, err = m.RevokeFleetMember(ctx, "bravo", "")
	require.NoError(t, err)
	rows, err = m.FleetMembers(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, fact.MemberRevoked, rows[1].State, "an enrolled id whose leaf is on the CRL is revoked")
}

// F3 (#527 follow-up): an origin URL that embeds a credential (a forge token
// in its userinfo) never leaves the master in a bundle, in the enroll's
// fleet_url, or in the address_not_applicable text: a joiner supplies its own
// auth, so the master's token would reach every joiner otherwise.
// SABOTAGE: fleetURLForBundle returns o.URL as stored -> the token is in the
// bundle -> red; the address_not_applicable text with o.URL -> red.
func TestFleetMembership_NoBundleCarriesUserinfo(t *testing.T) {
	ctx := context.Background()
	f := newForgeFleet(t, nil)
	const secret = "ghp_s3cr3tT0k3n"
	tokenURL := "https://x-access-token:" + secret + "@127.0.0.1:1/org/fleet.git"
	org, err := f.master.originOf(f.mri)
	require.NoError(t, err)
	require.NotNil(t, org, "fixture: the forge fleet's copy has an origin")
	org.URL = tokenURL
	require.NoError(t, f.master.Origins().Set(f.mri.UID(), *org))

	enr, err := f.master.EnrollFleetMember(ctx, "alpha", keyLine("alpha"), "")
	require.NoError(t, err)
	require.Equal(t, "https://127.0.0.1:1/org/fleet.git", enr.FleetURL)
	require.NotContains(t, enr.Bundle, secret)
	require.NotContains(t, enr.Bundle, "x-access-token")
	b, err := pki.SplitBundle([]byte(enr.Bundle))
	require.NoError(t, err)
	require.Equal(t, "https://127.0.0.1:1/org/fleet.git", b.FleetURL)

	_, err = f.master.EnrollFleetMember(ctx, "bravo", keyLine("bravo"), "https://hub.example")
	_, code := fleetCode(t, err)
	require.Equal(t, "address_not_applicable", code)
	require.NotContains(t, err.Error(), secret)
	require.Contains(t, err.Error(), "https://127.0.0.1:1/org/fleet.git")
}

// F3: Activity's host-changed detail is read without userinfo, whatever the
// record on main holds (every writer strips it today; the read does too).
// SABOTAGE: mainEvents joins c.Host as read -> red.
func TestFleetActivity_HostChangedDetailCarriesNoUserinfo(t *testing.T) {
	evs := mainEvents(store.FleetMainCommit{At: time.Now(), Author: "master", HostChanged: true,
		Host: []string{"https://u:ghp_tok@hub.example/git/fleet", "https://hub2.example/git/fleet"}}, nil)
	require.Len(t, evs, 1)
	require.Equal(t, FleetEventHostChanged, evs[0].Kind)
	require.Equal(t, "hosted at https://hub.example/git/fleet, https://hub2.example/git/fleet", evs[0].Detail)
}

// hostHere is POST /fleet/host's call, its error only.
func hostHere(ctx context.Context, m *Manager) error {
	_, err := m.HostFleetHere(ctx, FleetHostSpec{})
	return err
}
