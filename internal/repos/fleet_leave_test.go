package repos

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"knomit/internal/fact"
	"knomit/internal/pki"
	"knomit/internal/pki/pkitest"
	"knomit/internal/store"
	"knomit/internal/testsupport/testsigner"
)

// mainLeafSerial is the serial (lowercase hex) of id's leaf on the forge's
// main.
func (f *forgeFleet) mainLeafSerial(t *testing.T, id string) string {
	t.Helper()
	raw, err := runGitOut(f.bare, "show", "main:artifacts/fleet/certs/"+id+".pem")
	require.NoError(t, err)
	blk, _ := pem.Decode([]byte(raw))
	require.NotNil(t, blk)
	c, err := x509.ParseCertificate(blk.Bytes)
	require.NoError(t, err)
	return c.SerialNumber.Text(16)
}

// installMasterLeaf installs the forge master's own leaf in its [tls].dir,
// as a real master's create does, so its InstallCRL has a root to check.
func installMasterLeaf(t *testing.T, f *forgeFleet) {
	t.Helper()
	dir := filepath.Join(f.dir, "master-key")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	mk, err := testsigner.WriteNamedKey(dir, "master")
	require.NoError(t, err)
	// Back-dated by an hour: the installed leaf must start strictly before any
	// renewal the master issues later in the test, whatever second the test
	// runs in. A leaf issued "now" shares the renewal's one-second NotBefore
	// when the fixture is fast, and installMainLeaf (NotBefore strictly later)
	// then installs nothing.
	_, pub, err := pki.LoadSigner(mk)
	require.NoError(t, err)
	is, err := pki.IssueLeaf(f.root.Root, pub, "master@"+pki.AgentEmailDomain, 2*time.Hour, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	blk, _ := pem.Decode(is.CertPEM)
	cert, err := x509.ParseCertificate(blk.Bytes)
	require.NoError(t, err)
	f.root.Install(t, pkitest.Member{KeyPath: mk, Pub: pub, CertPEM: is.CertPEM, Cert: cert}, f.master.deps.Cfg.TLS.Dir)
}

// mainCRLNumber is the Number of the CRL on the forge's main.
func (f *forgeFleet) mainCRLNumber(t *testing.T) int64 {
	t.Helper()
	raw, err := runGitOut(f.bare, "show", "main:artifacts/fleet/crl.pem")
	require.NoError(t, err)
	crl, err := parseCRLPEM([]byte(raw))
	require.NoError(t, err)
	return crl.Number.Int64()
}

// memberState is agent's row state on GET /fleet/members of m.
func memberState(t *testing.T, m *Manager, agent string) string {
	t.Helper()
	rows, err := m.FleetMembers(context.Background())
	require.NoError(t, err)
	for _, r := range rows {
		if r.Agent == agent {
			return r.State
		}
	}
	return ""
}

// enrollAndJoin enrolls id on the forge master and joins m with the bundle:
// the join's answer.
func (f *forgeFleet) enrollAndJoin(t *testing.T, m *Manager, id string) FleetStatus {
	t.Helper()
	ctx := context.Background()
	enr, err := f.master.EnrollFleetMember(ctx, id, keyLine(id), "")
	require.NoError(t, err)
	require.True(t, enr.Published, enr.PublishError)
	st, err := m.JoinFleet(ctx, FleetJoinSpec{Bundle: []byte(enr.Bundle)})
	require.NoError(t, err)
	return st
}

// masterRounds runs the forge master's origin round, the merger, and two more
// rounds: what brings a member's pushed branch onto the forge's main (fetch +
// judge, merge, publish) and lets the round after that act on what main holds.
func (f *forgeFleet) masterRounds(t *testing.T) {
	t.Helper()
	round(t, f.mri)
	settleMerger(t, f.mri)
	round(t, f.mri)
	round(t, f.mri)
}

// Leave means revoke, and the join reports pending vs accepted by where the
// record is (user rulings 2026-10-16), on a forge-hosted fleet:
//   - the join answers record_state pending (the record is on alpha's branch
//     only, nothing judged it); after the master's round merges it into main
//     and alpha syncs, active (accepted);
//   - alpha leaves with a left record naming its leaf (leaf_serial); the
//     master's rounds, with no call, revoke that leaf once the left record is
//     on main: the CRL and the statement on main, the CRL
//     installed on the master and in its local copies; more rounds revoke
//     nothing more;
//   - the master enrolls alpha again (no `enrolled` refusal: no manual
//     revoke), alpha rejoins and reads pending although main holds a record
//     for it (`left`, not its candidate's state); the master's rounds do NOT
//     revoke the fresh leaf (the left record names the old serial) and the
//     rejoin is accepted.
//
// SABOTAGE: drop `peers.runDuty` from the origin round -> the leaf is never
// revoked -> red; drop the serial match in leftLeafDue -> the fresh leaf is
// revoked on the rejoin -> red; drop `main.State != branch.State`
// from recordStateOf -> the rejoin reads `left` -> red.
func TestFleetLeave_ForgeMasterRevokesTheLeaverAndTheRejoinIsAccepted(t *testing.T) {
	ctx := context.Background()
	f := newForgeFleet(t, nil)
	installMasterLeaf(t, f)
	alpha := f.namedMember(t, "alpha")

	joined := f.enrollAndJoin(t, alpha, "alpha")
	require.Equal(t, FleetRegistered, joined.State)
	require.Equal(t, RecordPending, joined.RecordState, "the join's record is on alpha's branch only: pending")
	f.masterRounds(t)
	require.True(t, f.onForgeMain(t, memberRecordPath("kb", "alpha")), "fixture: the master merged alpha's record")
	st, err := alpha.FleetStatus(ctx)
	require.NoError(t, err)
	require.Equal(t, RecordPending, st.RecordState, "alpha has not fetched the new main yet: still pending")
	syncFleet(t, alpha)
	st, err = alpha.FleetStatus(ctx)
	require.NoError(t, err)
	require.Equal(t, fact.MemberActive, st.RecordState, "on main: accepted")

	left, err := alpha.LeaveFleet(ctx)
	require.NoError(t, err)
	require.True(t, left.Pushed, left.PushError)
	require.Equal(t, int64(1), f.mainCRLNumber(t), "fixture: nothing revoked yet")
	f.masterRounds(t)
	rec, err := runGitOut(f.bare, "show", "main:"+memberRecordPath("kb", "alpha"))
	require.NoError(t, err)
	require.Contains(t, rec, "state: left", "the left record is on main")
	require.Contains(t, rec, "leaf_serial: "+f.mainLeafSerial(t, "alpha")+"\n", "the left record names the leaf alpha left with")
	require.Equal(t, int64(2), f.mainCRLNumber(t), "the master revoked alpha with no call")
	require.Equal(t, fact.MemberRevoked, memberState(t, f.master, "alpha"), "derived from main's CRL")
	statements, err := runGitOut(f.bare, "ls-tree", "--name-only", "main", "artifacts/fleet/revocations/")
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(statements, ".txt"), "one revocation statement on main: %q", statements)
	installed, err := pki.LoadCRL(filepath.Join(f.master.deps.Cfg.TLS.Dir, pki.CRLFile))
	require.NoError(t, err)
	require.Equal(t, int64(2), installed.Number.Int64(), "the new CRL is installed on the master")
	audit, err := os.ReadFile(filepath.Join(f.master.fleetRootDir(), pki.RevokedLogFile))
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(audit), "\n"), "one line in the local revocation log")
	require.Contains(t, string(audit), leaveRevokeReason)

	mainBefore := f.rev(t, "main")
	f.masterRounds(t)
	require.Equal(t, mainBefore, f.rev(t, "main"), "once: a revoked leaver is not revoked again")
	require.Equal(t, int64(2), f.mainCRLNumber(t))

	rejoined := f.enrollAndJoin(t, alpha, "alpha")
	require.Equal(t, RecordPending, rejoined.RecordState, "main says left, the candidate says active: pending")
	f.masterRounds(t)
	require.Equal(t, int64(2), f.mainCRLNumber(t), "the fresh leaf, issued after the leave, is not revoked")
	require.Equal(t, fact.MemberActive, memberState(t, f.master, "alpha"), "the rejoin is on main")
	syncFleet(t, alpha)
	st, err = alpha.FleetStatus(ctx)
	require.NoError(t, err)
	require.Equal(t, fact.MemberActive, st.RecordState, "the rejoin is accepted")
}

// The leave duty is master work (#531's predicate): a holder of the fleet's
// root key whom main's master marker does NOT name revokes no leaver, neither
// through its own origin rounds (peers.isMaster) nor when the duty is called
// directly, as the forge's fleetMasterDuty and the hub's fleetHostRound call
// it: fleetLeaveRound checks actsAsFleetMaster itself, so this one test pins
// both hostings (#532 review SF2). The left record is on main and the leaf
// unrevoked while beta runs (the members row says revoke_due, the master's
// status revokes_left, beta's not); the named master then revokes it
// (positive control: the fixture could have revoked).
// SABOTAGE: drop the actsAsFleetMaster check from fleetLeaveRound (or gate it
// on holdsFleetRoot) -> beta revokes alpha -> red.
func TestFleetLeave_ARootHolderMainDoesNotNameRevokesNoLeaver(t *testing.T) {
	ctx := context.Background()
	beta := testsigner.Named("beta")
	f := newForgeFleet(t, map[string]ssh.Signer{"beta": beta})
	installMasterLeaf(t, f)
	bm, bri := f.join(t, "beta", beta)
	alpha := f.namedMember(t, "alpha")
	f.enrollAndJoin(t, alpha, "alpha")
	f.masterRounds(t)
	installFleetRoot(t, bm, f.root)
	round(t, bri)

	left, err := alpha.LeaveFleet(ctx)
	require.NoError(t, err)
	require.True(t, left.Pushed, left.PushError)
	// The master merges and publishes the left record with its duty off, so
	// main holds a leaver only beta could revoke; the duty is back on below.
	duty := f.mri.peers.duty
	f.mri.peers.duty = nil
	f.masterRounds(t)
	f.mri.peers.duty = duty
	rec, err := runGitOut(f.bare, "show", "main:"+memberRecordPath("kb", "alpha"))
	require.NoError(t, err)
	require.Contains(t, rec, "state: left", "fixture: the left record is on main")
	require.Equal(t, int64(1), f.mainCRLNumber(t), "fixture: alpha's leaf is not revoked yet")
	rows, err := f.master.FleetMembers(ctx)
	require.NoError(t, err)
	due := map[string]bool{}
	for _, r := range rows {
		due[r.Agent] = r.RevokeDue
	}
	require.True(t, due["alpha"], "the left row is revoke_due")
	require.False(t, due["beta"])
	mst, err := f.master.FleetStatus(ctx)
	require.NoError(t, err)
	require.True(t, mst.RevokesLeft, "the acting master's rounds revoke leavers")
	bst, err := bm.FleetStatus(ctx)
	require.NoError(t, err)
	require.False(t, bst.RevokesLeft, "beta's do not")

	mainBefore := f.rev(t, "main")
	tlsCRL := filepath.Join(bm.deps.Cfg.TLS.Dir, pki.CRLFile)
	crlBefore, _ := os.ReadFile(tlsCRL)
	round(t, bri)
	round(t, bri)
	require.NoError(t, bri.WithRead(func(svc *store.Service) {
		require.True(t, bm.holdsFleetRoot(svc), "setup: beta holds the root")
		require.False(t, bri.actsAsFleetMaster(svc), "main names the master, not beta")
		bri.fleetMasterDuty(ctx, svc)
		bri.fleetLeaveRound(ctx, svc) // the hub's call, made directly
	}))
	round(t, bri)
	require.Equal(t, mainBefore, f.rev(t, "main"), "beta publishes nothing to main")
	require.Equal(t, int64(1), f.mainCRLNumber(t), "beta revokes no leaver")
	branchCRL, err := runGitOut(f.bare, "show", "agent/beta:artifacts/fleet/crl.pem")
	require.NoError(t, err)
	mainCRL, err := runGitOut(f.bare, "show", "main:artifacts/fleet/crl.pem")
	require.NoError(t, err)
	require.Equal(t, mainCRL, branchCRL, "no revocation on beta's branch either")
	crlAfter, _ := os.ReadFile(tlsCRL)
	require.Equal(t, crlBefore, crlAfter, "beta installs no CRL")
	_, err = os.Stat(filepath.Join(bm.fleetRootDir(), pki.RevokedLogFile))
	require.True(t, os.IsNotExist(err), "beta logs no revocation: %v", err)

	// The named master does it.
	round(t, f.mri)
	require.Equal(t, int64(2), f.mainCRLNumber(t), "the master revokes the leaver")
}

// The leave duty never builds a CRL from main's while the master's agent
// branch holds one main does not (an unpublished revocation): it waits, and
// after the publish it revokes every leaver in ONE pass, each CRL built on the
// one before, so the final CRL lists all of them and the earlier revocation.
// SABOTAGE: drop the unpublished check -> the pass builds from main's CRL and
// drops gamma's serial -> red; build every revocation from main's CRL (no
// chaining) -> alpha's serial is missing from the final CRL -> red; drop the
// "already on main's CRL" skip -> the revoked alpha fails the next pass
// before delta, who left later, is revoked -> red.
func TestFleetLeave_TheDutyWaitsForAnUnpublishedCRLAndChainsEachRevocation(t *testing.T) {
	ctx := context.Background()
	f := newForgeFleet(t, nil)
	installMasterLeaf(t, f)
	members := map[string]*Manager{}
	for _, id := range []string{"alpha", "beta", "delta"} {
		members[id] = f.namedMember(t, id)
		require.Equal(t, RecordPending, f.enrollAndJoin(t, members[id], id).RecordState)
	}
	f.masterRounds(t)
	for _, id := range []string{"alpha", "beta"} {
		_, err := members[id].LeaveFleet(ctx)
		require.NoError(t, err)
	}
	enr, err := f.master.EnrollFleetMember(ctx, "gamma", keyLine("gamma"), "")
	require.NoError(t, err)
	// Fetch + judge, merge, publish: both left records reach main, with the
	// rounds' duty off (a round can merge and publish within itself, so its
	// count says nothing); the test drives the duty directly below.
	duty := f.mri.peers.duty
	f.mri.peers.duty = nil
	f.masterRounds(t)
	f.mri.peers.duty = duty
	for _, id := range []string{"alpha", "beta"} {
		rec, err := runGitOut(f.bare, "show", "main:"+memberRecordPath("kb", id))
		require.NoError(t, err)
		require.Contains(t, rec, "state: left", "fixture: %s's left record is on main", id)
	}
	before := f.mainCRLNumber(t)

	// An unpublished revocation of gamma: on the agent branch, not on main.
	unlock := f.master.lockFleetOp()
	fl, err := f.master.fleetAtMain(f.mri)
	require.NoError(t, err)
	root, err := pki.LoadRoot(f.master.fleetRootDir())
	require.NoError(t, err)
	leaf, err := fl.PKI.Cert("gamma")
	require.NoError(t, err)
	require.NotNil(t, leaf)
	_, _, err = f.master.revokeLeaf(ctx, f.mri, root, fl.PKI.CRL, "gamma", leaf, "test")
	require.NoError(t, err)
	revoked, err := f.master.revokeLeftMembers(ctx, f.mri)
	require.NoError(t, err)
	unlock()
	require.Empty(t, revoked, "an unpublished CRL on the agent branch: the duty waits")

	require.NoError(t, f.master.publishMasterWrite(ctx, f.mri))
	unlock = f.master.lockFleetOp()
	revoked, err = f.master.revokeLeftMembers(ctx, f.mri)
	unlock()
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"alpha", "beta"}, revoked)
	require.NoError(t, f.master.publishMasterWrite(ctx, f.mri))
	fl, err = f.master.fleetAtMain(f.mri)
	require.NoError(t, err)
	require.Equal(t, before+3, fl.PKI.CRL.Number.Int64(), "gamma's, then alpha's and beta's")
	for _, id := range []string{"alpha", "beta", "gamma"} {
		require.True(t, fl.PKI.Revoked(id), "%s is on main's CRL", id)
	}
	require.NotEmpty(t, enr.Serial)

	// delta leaves later: the next pass skips the leavers main's CRL already
	// lists and revokes delta alone.
	_, err = members["delta"].LeaveFleet(ctx)
	require.NoError(t, err)
	f.mri.peers.duty = nil
	f.masterRounds(t)
	f.mri.peers.duty = duty
	unlock = f.master.lockFleetOp()
	revoked, err = f.master.revokeLeftMembers(ctx, f.mri)
	unlock()
	require.NoError(t, err)
	require.Equal(t, []string{"delta"}, revoked, "only the new leaver")
}

// recordStateOf is the one rule: the candidate on the branch only, or in
// another state than main's, is pending; on main it is main's state
// (revoked when main's CRL says so); on neither it is absent.
// SABOTAGE: answer main's state whenever main holds a record -> the rejoin
// row reads left -> red.
func TestRecordStateOf(t *testing.T) {
	rec := func(state string) store.FleetMember {
		return store.FleetMember{Member: fact.Member{Agent: "a", State: state}}
	}
	none := store.FleetMember{}
	for _, c := range []struct {
		name            string
		branch          store.FleetMember
		onBranch        bool
		main            store.FleetMember
		onMain, revoked bool
		want            string
	}{
		{"neither", none, false, none, false, false, ""},
		{"branch only (a join)", rec(fact.MemberActive), true, none, false, false, RecordPending},
		{"main in the branch's state", rec(fact.MemberActive), true, rec(fact.MemberActive), true, false, fact.MemberActive},
		{"a rejoin: main still says left", rec(fact.MemberActive), true, rec(fact.MemberLeft), true, false, RecordPending},
		{"accepted and revoked", rec(fact.MemberActive), true, rec(fact.MemberActive), true, true, fact.MemberRevoked},
		{"main only", none, false, rec(fact.MemberActive), true, false, fact.MemberActive},
	} {
		require.Equal(t, c.want, recordStateOf(c.branch, c.onBranch, c.main, c.onMain, c.revoked), c.name)
	}
}

// #532 review SF1: a stale `left` record, pushed before the operator revoked
// alpha by hand and enrolled it again, is merged by the master AFTER the fresh
// leaf landed on main. The record names the OLD leaf's serial, so the duty
// revokes nothing: the fresh leaf stays valid and the rejoin is accepted.
// SABOTAGE: drop the serial match from leftLeafDue -> the fresh leaf is
// revoked (CRL 3) -> red.
func TestFleetLeave_AStaleLeftMergedAfterAReenrollNeverRevokesTheFreshLeaf(t *testing.T) {
	ctx := context.Background()
	f := newForgeFleet(t, nil)
	installMasterLeaf(t, f)
	alpha := f.namedMember(t, "alpha")
	f.enrollAndJoin(t, alpha, "alpha")
	f.masterRounds(t)
	syncFleet(t, alpha)
	oldSerial := f.mainLeafSerial(t, "alpha")

	left, err := alpha.LeaveFleet(ctx)
	require.NoError(t, err)
	require.True(t, left.Pushed, left.PushError)
	// The operator revokes by hand and enrolls alpha again before the
	// master's round merged the left record.
	_, err = f.master.RevokeFleetMember(ctx, "alpha", "by hand")
	require.NoError(t, err)
	enr, err := f.master.EnrollFleetMember(ctx, "alpha", keyLine("alpha"), "")
	require.NoError(t, err)
	require.True(t, enr.Published, enr.PublishError)
	require.Equal(t, int64(2), f.mainCRLNumber(t), "fixture: the manual revoke")
	freshSerial := f.mainLeafSerial(t, "alpha")
	require.NotEqual(t, oldSerial, freshSerial, "fixture: a fresh leaf on main")

	f.masterRounds(t)
	rec, err := runGitOut(f.bare, "show", "main:"+memberRecordPath("kb", "alpha"))
	require.NoError(t, err)
	require.Contains(t, rec, "state: left", "fixture: the stale left record landed on main after the fresh leaf")
	require.Contains(t, rec, "leaf_serial: "+oldSerial+"\n", "it names the old leaf")
	require.Equal(t, int64(2), f.mainCRLNumber(t), "the fresh leaf is not revoked by a stale left record")
	rows, err := f.master.FleetMembers(ctx)
	require.NoError(t, err)
	for _, r := range rows {
		require.False(t, r.RevokeDue, "nothing is due: %s", r.Agent)
	}

	_, err = alpha.JoinFleet(ctx, FleetJoinSpec{Bundle: []byte(enr.Bundle)})
	require.NoError(t, err)
	f.masterRounds(t)
	require.Equal(t, int64(2), f.mainCRLNumber(t))
	require.Equal(t, fact.MemberActive, memberState(t, f.master, "alpha"), "the rejoin is accepted")
}

// A `left` record written before leaf_serial existed names no leaf. The
// choice (#532 review SF1): the duty never revokes for such a record, so it
// can never revoke a later fresh leaf; its operator revokes by hand, as
// before this change. Simulated by a leave with no certificate installed (the
// record then names none).
// SABOTAGE: let leftLeafDue accept an empty leaf_serial -> alpha's leaf is
// revoked -> red.
func TestFleetLeave_ALeftRecordNamingNoLeafIsNotRevokedAutomatically(t *testing.T) {
	ctx := context.Background()
	f := newForgeFleet(t, nil)
	installMasterLeaf(t, f)
	alpha := f.namedMember(t, "alpha")
	f.enrollAndJoin(t, alpha, "alpha")
	f.masterRounds(t)
	syncFleet(t, alpha)

	require.NoError(t, os.Remove(filepath.Join(alpha.deps.Cfg.TLS.Dir, pki.InstanceCertFile)))
	left, err := alpha.LeaveFleet(ctx)
	require.NoError(t, err)
	require.True(t, left.Pushed, left.PushError)
	f.masterRounds(t)
	rec, err := runGitOut(f.bare, "show", "main:"+memberRecordPath("kb", "alpha"))
	require.NoError(t, err)
	require.Contains(t, rec, "state: left", "fixture: the left record is on main")
	require.NotContains(t, rec, "leaf_serial", "fixture: it names no leaf")
	require.Equal(t, int64(1), f.mainCRLNumber(t), "not revoked automatically")
	require.Equal(t, fact.MemberLeft, memberState(t, f.master, "alpha"))
	rows, err := f.master.FleetMembers(ctx)
	require.NoError(t, err)
	for _, r := range rows {
		require.False(t, r.RevokeDue, "not due: %s", r.Agent)
	}
}
