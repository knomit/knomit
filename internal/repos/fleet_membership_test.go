package repos

import (
	"context"
	"encoding/pem"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/pki"
	"knomit/internal/pki/pkitest"
	"knomit/internal/store"
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

	enr, err := f.master.EnrollFleetMember(ctx, "alpha", keyLine("alpha"))
	require.NoError(t, err)
	require.True(t, enr.Published, enr.PublishError)
	require.Equal(t, []string{"CERTIFICATE", "CERTIFICATE", "X509 CRL"}, pemTypes(enr.Bundle), "leaf, root, CRL: nothing else")
	require.True(t, f.onForgeMain(t, "artifacts/fleet/certs/alpha.pem"), "the leaf is on the forge's main")
	require.True(t, f.onForgeMain(t, "artifacts/fleet/issued/"+enr.Serial+".pem"), "and its issuance record")

	alpha := f.namedMember(t, "alpha")
	_, err = alpha.EnrollFleetMember(ctx, "beta", keyLine("beta"))
	_, code := fleetCode(t, err)
	require.Equal(t, "not_registered", code, "a standalone instance enrolls nobody")
	st, err := alpha.JoinFleet(ctx, FleetJoinSpec{URL: f.url, Bundle: []byte(enr.Bundle)})
	require.NoError(t, err)
	require.Equal(t, FleetRegistered, st.State)
	require.Equal(t, 2, pushCount(t, f.bare, "agent/alpha"), "the mount's round, then the join's one push")

	_, err = alpha.EnrollFleetMember(ctx, "beta", keyLine("beta"))
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
	require.Equal(t, "master", code, "the master decommissions; it does not leave")

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
	require.Equal(t, mainBefore, f.rev(t, "main"), "the forge keeps its copy, inert")
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
	enr, err := f.master.EnrollFleetMember(ctx, "alpha", keyLine("alpha"))
	require.NoError(t, err)
	alpha := f.namedMember(t, "alpha")
	_, err = alpha.JoinFleet(ctx, FleetJoinSpec{URL: f.url, Bundle: []byte(enr.Bundle)})
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
	require.NoError(t, ari.WithRead(func(svc *store.Service) { require.True(t, ari.isFleetMaster(svc), "alpha is the master now") }))
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
	st, err := alpha.FleetStatus(ctx)
	require.NoError(t, err)
	require.True(t, st.Master)
}

// The hub (the master's copy has no origin) cannot stand down: point it at
// the new master first. Its detach pre-flight is the create's: the listener
// and an address.
// SABOTAGE: drop the hub check in StandDown -> the root is deleted -> red.
func TestFleetMembership_TheHubCannotStandDown(t *testing.T) {
	ctx := context.Background()
	m := newFleetHost(t, t.TempDir(), nil)
	_, err := m.CreateFleet(ctx, FleetCreateSpec{})
	require.NoError(t, err)
	err = m.StandDown(ctx)
	status, code := fleetCode(t, err)
	require.Equal(t, http.StatusConflict, status)
	require.Equal(t, "hub", code)
	_, err = os.Stat(filepath.Join(m.fleetRootDir(), pki.RootKeyFile))
	require.NoError(t, err, "the root key stays")
	st, err := m.FleetStatus(ctx)
	require.NoError(t, err)
	require.True(t, st.Master)
	require.Equal(t, "none", st.Hosting)
}

// DELETE /repos/{fleet}/origin's guard (R3.3): a member keeps its origin;
// the master may detach once [tls].addr and external_addresses are set.
// SABOTAGE: GuardFleetDetach returns nil for a member -> red.
func TestFleetMembership_DetachGuard(t *testing.T) {
	ctx := context.Background()
	f := newForgeFleet(t, nil)
	enr, err := f.master.EnrollFleetMember(ctx, "alpha", keyLine("alpha"))
	require.NoError(t, err)
	alpha := f.namedMember(t, "alpha")
	_, err = alpha.JoinFleet(ctx, FleetJoinSpec{URL: f.url, Bundle: []byte(enr.Bundle)})
	require.NoError(t, err)

	err = alpha.GuardFleetDetach(alpha.fleetRepo())
	status, code := fleetCode(t, err)
	require.Equal(t, http.StatusConflict, status)
	require.Equal(t, "member_keeps_origin", code)

	_, code = fleetCode(t, f.master.GuardFleetDetach(f.mri))
	require.Equal(t, "listener_required", code)
	f.master.deps.Cfg.TLS.Addr = "127.0.0.1:0"
	_, code = fleetCode(t, f.master.GuardFleetDetach(f.mri))
	require.Equal(t, "addresses_required", code)
	f.master.deps.Cfg.ExternalAddresses = []string{"https://master.example"}
	require.NoError(t, f.master.GuardFleetDetach(f.mri), "the master hosts the fleet here")
	require.NoError(t, alpha.GuardFleetDetach(createRepo(t, alpha, "kb")), "any other repository: nothing to check")
}

// The operator's merge of a pushed branch on the fleet repository (R3.3,
// PR #524 residue): only the master merges, and it re-judges the branch
// (tip minus main) first. A local agent/x whose one commit edits another
// member's folder is refused not-owner; one that edits only its author's own
// folder is admitted.
// SABOTAGE: drop the JudgePeerBranch call -> the not-owner branch is
// admitted -> red; drop the isFleetMaster check -> the member row passes the
// master check and is judged instead -> red.
func TestFleetMembership_ManualMergeIsMasterOnlyAndReJudged(t *testing.T) {
	ctx := context.Background()
	// The persisted agent id, as production sets it: every commit here is
	// authored by the master, whatever branch it lands on.
	m := newFleetHost(t, t.TempDir(), func(d *Deps) { d.AgentID = "test-fleet" })
	_, err := m.CreateFleet(ctx, FleetCreateSpec{})
	require.NoError(t, err)
	ri := m.fleetRepo()
	own := m.AgentID()
	plant := func(branch, path string) plumbing.Hash {
		var tip string
		require.NoError(t, ri.WithRead(func(svc *store.Service) {
			require.NoError(t, svc.Branches().CreateBranch(ctx, branch, svc.UpstreamBranch()))
			_, err := svc.Facts().WriteFact(ctx, branch, path, "---\nkind: pragmatic\ntype: policy\n---\n# x\n\nx\n", "plant", "")
			require.NoError(t, err)
			tip, err = svc.Branches().HeadCommit(ctx, branch)
			require.NoError(t, err)
		}))
		return plumbing.NewHash(tip)
	}
	bad := plant("agent/x", "kb/members/somebody/evil.md")
	good := plant("agent/y", "kb/members/"+own+"/note.md")

	var badErr, goodErr error
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		badErr = ri.FleetMergeRefusal(ctx, svc, "agent/x", bad)
		goodErr = ri.FleetMergeRefusal(ctx, svc, "agent/y", good)
	}))
	status, code := fleetCode(t, badErr)
	require.Equal(t, http.StatusConflict, status)
	require.Equal(t, "judge_refused", code)
	require.ErrorContains(t, badErr, "not-owner")
	require.NoError(t, goodErr, "control: an own-folder edit is admitted")

	// A member (no root here) is refused before any judging.
	f := newForgeFleet(t, nil)
	enr, err := f.master.EnrollFleetMember(ctx, "alpha", keyLine("alpha"))
	require.NoError(t, err)
	alpha := f.namedMember(t, "alpha")
	_, err = alpha.JoinFleet(ctx, FleetJoinSpec{URL: f.url, Bundle: []byte(enr.Bundle)})
	require.NoError(t, err)
	ari := alpha.fleetRepo()
	require.NoError(t, ari.WithRead(func(svc *store.Service) {
		_, code := fleetCode(t, ari.FleetMergeRefusal(ctx, svc, "agent/x", bad))
		require.Equal(t, "not_master", code)
	}))
}

// #518 R4: the generic create's "creating this repository again will adopt
// that branch" is not shown for a fleet, whose create is not re-run to adopt
// it (its rollback deletes the branch on the forge).
// SABOTAGE: ignore the fleet argument -> the fleet text offers to adopt -> red.
func TestAgentBranchAlreadyPushed_FleetIsNotOfferedToAdopt(t *testing.T) {
	base := context.Canceled
	repo := agentBranchAlreadyPushed(base, "https://forge/x.git", "agent/a", "main", false)
	flt := agentBranchAlreadyPushed(base, "https://forge/x.git", "agent/a", "main", true)
	require.ErrorIs(t, flt, context.Canceled)
	require.Contains(t, repo.Error(), "adopt that branch")
	require.NotContains(t, flt.Error(), "adopt")
	require.Contains(t, flt.Error(), "rollback deletes that branch on the forge")
}
