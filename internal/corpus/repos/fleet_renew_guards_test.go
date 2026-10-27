package repos

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"knomit/internal/domain/fact"
	"knomit/internal/engine/store"
	"knomit/internal/foundation/pki"
)

// PR #537 review B1 (the reviewer's repro, kept as a regression test): a revoke whose publish failed sits on the
// master's agent branch only. The next master round fetches, runs the duties
// (renewal sees main's CRL, which does not list the leaf yet, and the agent
// branch's leaf equals main's), and then publishes both: main ends with the
// old serial on its CRL AND a fresh, unrevoked leaf for the revoked member.
func TestFleetRenew_AnUnpublishedRevokeIsNotUndoneByARenewal(t *testing.T) {
	ctx := context.Background()
	f := newForgeFleet(t, nil)
	installMasterLeaf(t, f)
	alpha := f.namedMember(t, "alpha")
	f.enrollAndJoin(t, alpha, "alpha")
	f.masterRounds(t)
	oldAlpha := f.mainLeaf(t, "alpha")

	// The forge is unreachable while the operator revokes alpha.
	aside := f.bare + ".aside"
	require.NoError(t, os.Rename(f.bare, aside))
	rv, err := f.master.RevokeFleetMember(ctx, "alpha", "compromised")
	require.NoError(t, os.Rename(aside, f.bare))
	require.NoError(t, err)
	require.False(t, rv.Published, "fixture: the revoke's publish failed")

	// The next round, with alpha's leaf in the final third of its lifetime.
	due := time.Now().Add(pki.Lifetime(oldAlpha) * 3 / 4)
	restore := SetFleetClockForTest(func() time.Time { return due })
	round(t, f.mri)
	restore()
	round(t, f.mri)

	var revokedOnMain bool
	var serialOnMain string
	require.NoError(t, f.mri.WithRead(func(svc *store.Service) {
		fl, err := svc.FleetAt(svc.Remote().UpstreamBranch())
		require.NoError(t, err)
		revokedOnMain = fl.PKI.Revoked("alpha")
		c, _ := fl.PKI.Cert("alpha")
		serialOnMain = c.SerialNumber.Text(16)
	}))
	t.Logf("old serial %s, main's serial now %s, revoked on main: %v, CRL #%d",
		oldAlpha.SerialNumber.Text(16), serialOnMain, revokedOnMain, f.mainCRLNumber(t))
	round(t, alpha.fleetRepo())
	inst, err := pki.LoadInstanceCert(alpha.deps.Cfg.TLS.Dir)
	require.NoError(t, err)
	t.Logf("alpha's installed serial after its round: %s", inst.SerialNumber.Text(16))
	require.True(t, revokedOnMain, "alpha was revoked, but main now holds an unrevoked leaf for it (serial %s)", serialOnMain)
}

// PR #537 review B2 (the reviewer's repro, kept as a regression test): a member's `left` record merged into the
// master's agent branch but not yet on main. The renewal duty reads records
// from main only (active), re-issues the leaver's leaf, and the round
// publishes both; the leave duty then never revokes, because the left
// record's leaf_serial names the OLD serial (leftLeafDue).
//
// #651: the round that admits alpha's leave runs its duties while the
// consensus merger (its own goroutine) may not have merged agent/alpha yet,
// so the `left` record is on neither main nor the agent branch. The test used
// to pass only when the merger won that race; under load it lost about 1 run
// in 25. The merger is now parked for the due round, so the window is open
// on every run, and the agent branch is asserted to still read alpha active
// when the duty has run.
// SABOTAGE: drop the pushedLeftSerials filter in renewFleetLeaves -> alpha
// is renewed in the window -> red on every run.
func TestFleetRenew_ALeaveOnTheAgentBranchIsNotRenewedBeforeMain(t *testing.T) {
	ctx := context.Background()
	f := newForgeFleet(t, nil)
	installMasterLeaf(t, f)
	alpha := f.namedMember(t, "alpha")
	f.enrollAndJoin(t, alpha, "alpha")
	f.masterRounds(t)
	oldAlpha := f.mainLeaf(t, "alpha")

	_, err := alpha.LeaveFleet(ctx)
	require.NoError(t, err)
	stateAt := func(branch string) string {
		var s string
		require.NoError(t, f.mri.WithRead(func(svc *store.Service) {
			if branch == "" {
				branch = svc.Remote().UpstreamBranch()
			}
			fl, err := svc.FleetAt(branch)
			require.NoError(t, err)
			for _, m := range fl.Members {
				if m.Agent == "alpha" {
					s = m.State
				}
			}
		}))
		return s
	}
	t.Logf("fixture: alpha on main %q, on the agent branch %q", stateAt(""), stateAt(f.mri.AgentBranch()))

	// Park the merger before it merges alpha's branch: the due round admits
	// the leave and runs its duties with the `left` record on agent/alpha
	// only.
	release := make(chan struct{})
	var releaseOnce sync.Once
	unpark := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unpark)
	setConsensusHooks(t, consensusHooks{beforeMerge: func(branch string) {
		if branch == "agent/alpha" {
			<-release
		}
	}})
	due := time.Now().Add(pki.Lifetime(oldAlpha) * 3 / 4)
	restore := SetFleetClockForTest(func() time.Time { return due })
	round(t, f.mri)
	restore()
	require.Equal(t, "left", stateAt("agent/alpha"), "fixture: the due round admitted alpha's leave")
	require.Equal(t, "active", stateAt(f.mri.AgentBranch()), "fixture: the leave was not merged before the duty ran")
	unpark()
	for i := 0; i < 4; i++ {
		round(t, f.mri)
		settleMerger(t, f.mri)
	}

	var revokedOnMain bool
	var serialOnMain, state string
	require.NoError(t, f.mri.WithRead(func(svc *store.Service) {
		fl, err := svc.FleetAt(svc.Remote().UpstreamBranch())
		require.NoError(t, err)
		revokedOnMain = fl.PKI.Revoked("alpha")
		c, _ := fl.PKI.Cert("alpha")
		serialOnMain = c.SerialNumber.Text(16)
		for _, m := range fl.Members {
			if m.Agent == "alpha" {
				state = m.State + " leaf_serial=" + m.LeafSerial
			}
		}
	}))
	t.Logf("old serial %s, main's serial %s, revoked on main %v, record %s", oldAlpha.SerialNumber.Text(16), serialOnMain, revokedOnMain, state)
	require.True(t, revokedOnMain, "alpha left, but main holds an unrevoked leaf for it")
}

// renewFixture is a forge fleet with alpha joined and accepted, and a
// decision clock in the final third of alpha's leaf.
func renewFixture(t *testing.T) (*forgeFleet, *Manager, *x509.Certificate, func() time.Time) {
	t.Helper()
	f := newForgeFleet(t, nil)
	installMasterLeaf(t, f)
	alpha := f.namedMember(t, "alpha")
	f.enrollAndJoin(t, alpha, "alpha")
	f.masterRounds(t)
	old := f.mainLeaf(t, "alpha")
	due := time.Now().Add(pki.Lifetime(old) * 3 / 4)
	return f, alpha, old, func() time.Time { return due }
}

// PR #537 review S1, guard 1 of 4: a leaf whose id has TWO member records is
// not renewed (the leaf-renewal artifact rule requires exactly one, so the
// master's own judge would refuse the commit and hold main). Positive
// control: with the duplicate gone, the same round renews it.
// SABOTAGE: `len(recs) == 1` -> `len(recs) >= 1` in renewableAt -> red.
func TestFleetRenew_TwoRecordsForTheIdAreNotRenewed(t *testing.T) {
	f, _, old, due := renewFixture(t)
	pub := old.PublicKey.(ed25519.PublicKey)
	dup := "kb/members/alpha/duplicate.md"
	f.forgeMainCommit(t, dup, activeRecordFile(t, "alpha", pub), "a second record for alpha")
	round(t, f.mri) // the master's copy (main and agent branch) holds both
	restore := SetFleetClockForTest(due)
	round(t, f.mri)
	restore()
	require.Equal(t, old.SerialNumber, f.mainLeaf(t, "alpha").SerialNumber, "two records: not renewed")

	f.forgeMainCommit(t, dup, nil, "the duplicate goes")
	round(t, f.mri)
	restore = SetFleetClockForTest(due)
	round(t, f.mri)
	restore()
	require.NotEqual(t, old.SerialNumber, f.mainLeaf(t, "alpha").SerialNumber, "positive control: one record renews")
}

// Guard 2 of 4: a record naming ANOTHER key than the leaf certifies is not
// renewed (a renewal is for the same key only; the judge would refuse it).
// SABOTAGE: drop fact.SameKey from renewableAt -> red.
func TestFleetRenew_ARecordWithAnotherKeyIsNotRenewed(t *testing.T) {
	f, _, old, due := renewFixture(t)
	other, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	f.forgeMainCommit(t, memberRecordPath("kb", "alpha"), activeRecordFile(t, "alpha", other), "alpha's record names another key")
	round(t, f.mri)
	// Called directly: a renewal the duty did commit would be held back by
	// the master's own judge (main would not move), which is exactly the
	// stuck main this guard spares, so main alone cannot show it.
	restore := SetFleetClockForTest(due)
	defer restore()
	renewed, err := f.master.renewFleetLeaves(context.Background(), f.mri)
	require.NoError(t, err)
	require.Contains(t, renewed, "master", "positive control: the master's own due leaf is renewed in the same call")
	require.NotContains(t, renewed, "alpha", "another key: not renewed")
	own, err := fleetAtBranch(f.mri, f.mri.AgentBranch())
	require.NoError(t, err)
	mine, err := own.PKI.Cert("alpha")
	require.NoError(t, err)
	require.Equal(t, old.SerialNumber, mine.SerialNumber, "no renewal of alpha committed on the agent branch")
}

// Guard 3 of 4: a renewal that is on the agent branch and not on main yet
// is not renewed AGAIN (a second leaf for the same id, the first never
// published). Called directly, twice, as two rounds whose publish failed
// would. Positive control: the first call renews.
// SABOTAGE: drop the "agent branch holds the same leaf as main" check -> the
// second call renews again -> red.
func TestFleetRenew_AnUnpublishedRenewalIsNotRenewedAgain(t *testing.T) {
	f, _, old, due := renewFixture(t)
	restore := SetFleetClockForTest(due)
	defer restore()
	ctx := context.Background()
	first, err := f.master.renewFleetLeaves(ctx, f.mri)
	require.NoError(t, err)
	require.Contains(t, first, "alpha", "positive control: the first call renews alpha")
	head := localTip(t, f.mri, f.mri.AgentBranch())
	second, err := f.master.renewFleetLeaves(ctx, f.mri)
	require.NoError(t, err)
	require.NotContains(t, second, "alpha", "the unpublished renewal is not renewed again")
	require.Equal(t, head, localTip(t, f.mri, f.mri.AgentBranch()), "nothing more committed")
	require.Equal(t, old.SerialNumber, f.mainLeaf(t, "alpha").SerialNumber, "fixture: main never saw the renewal")
}

// Guard 4 of 4: a re-signed CRL that is on the agent branch and not on main
// yet is not re-signed again (two CRLs with consecutive Numbers, the first
// never published). Positive control: the first call re-signs.
// SABOTAGE: drop the "agent branch holds the same CRL as main" check in
// resignFleetCRL -> the second call re-signs -> red.
func TestFleetRenew_AnUnpublishedCRLIsNotResignedAgain(t *testing.T) {
	f, _, _, due := renewFixture(t)
	restore := SetFleetClockForTest(due)
	defer restore()
	ctx := context.Background()
	n, err := f.master.resignFleetCRL(ctx, f.mri)
	require.NoError(t, err)
	require.Equal(t, "2", n, "positive control: the first call re-signs (#2)")
	head := localTip(t, f.mri, f.mri.AgentBranch())
	n, err = f.master.resignFleetCRL(ctx, f.mri)
	require.NoError(t, err)
	require.Empty(t, n, "the unpublished re-sign is not re-signed again")
	require.Equal(t, head, localTip(t, f.mri, f.mri.AgentBranch()), "nothing more committed")
}

// PR #537 review N3: a copy whose installed root is not main's root (after a
// fleet move) installs no leaf from main and reports no error: there is
// nothing for it there, as for the CRL. Main's leaf is a fresh renewal, so
// only the root comparison stops it.
// SABOTAGE: drop the root comparison in installMainLeaf -> InstallLeaf's
// ErrUntrustedRoot comes back every round -> red.
func TestFleetTick_ALeafOfAnotherRootIsNotInstalled(t *testing.T) {
	f, alpha, _, due := renewFixture(t)
	dir := alpha.deps.Cfg.TLS.Dir
	require.NoError(t, pki.Uninstall(dir))
	other := newForgeFleet(t, nil).root
	other.Install(t, other.Enroll(t, "alpha@"+pki.AgentEmailDomain, alpha.deps.KeyPath), dir)
	time.Sleep(1100 * time.Millisecond) // main's renewal starts later than the installed leaf
	restore := SetFleetClockForTest(due)
	round(t, f.mri)
	restore()
	before, err := pki.LoadInstanceCert(dir)
	require.NoError(t, err)
	require.True(t, f.mainLeaf(t, "alpha").NotBefore.After(before.NotBefore), "fixture: main's leaf is newer than the installed one")
	var serial string
	syncFleet(t, alpha)
	require.NoError(t, alpha.fleetRepo().WithRead(func(svc *store.Service) { serial, err = alpha.installMainLeaf(svc) }))
	require.NoError(t, err)
	require.Empty(t, serial)
	after, err := pki.LoadInstanceCert(dir)
	require.NoError(t, err)
	require.Equal(t, before.Raw, after.Raw, "the installed leaf is untouched")
}

// leftRecordFile is a `left` member record for id holding pub and naming
// serial, as Leave writes it (the serial is free text to the judge).
func leftRecordFile(t *testing.T, id string, pub ed25519.PublicKey, serial string) string {
	t.Helper()
	k, err := ssh.NewPublicKey(pub)
	require.NoError(t, err)
	return "---\nkind: pragmatic\ntype: policy\n---\n# member " + id + "\n\n" +
		fact.RenderMember(fact.Member{Agent: id, State: fact.MemberLeft, Key: k, Host: "h", LeafSerial: serial})
}

// pushedBranch makes branch, a pushed member branch on the master's copy
// (what an admitted fetch leaves), from main, with files written on it. The
// merger is held for the rest of the test, so nothing merges, re-judges or
// demotes the branch while the duty reads it.
func pushedBranch(t *testing.T, f *forgeFleet, branch string, files map[string]string) {
	t.Helper()
	ctx := context.Background()
	f.mri.consensus.runMu.Lock()
	t.Cleanup(f.mri.consensus.runMu.Unlock)
	require.NoError(t, f.mri.WithRead(func(svc *store.Service) {
		require.NoError(t, svc.CreateBranch(ctx, branch, svc.Remote().UpstreamBranch()))
		for p, content := range files {
			_, err := svc.Facts().WriteFact(ctx, branch, p, content, "fixture: "+p, "learn")
			require.NoError(t, err)
		}
	}))
	require.True(t, f.mri.IsPushedBranch(branch), "fixture: %s is a pushed branch", branch)
}

// #654 review B1: the pushed-branch leave check matches like leftLeafDue,
// per agent. Mallory's own `left` record names ALPHA's serial (leaf_serial is
// free text to the judge, and every serial is public on main); that is a
// record of mallory's, so alpha is still renewed. Positive control (a fresh
// fixture): alpha's own `left` record naming its serial, on a pushed branch
// not merged yet, does stop alpha's renewal, and only alpha's.
// SABOTAGE: match the pushed `left` records by serial alone, whoever's record
// it is (the first #654 revision) -> alpha is not renewed in the first half
// -> red.
func TestFleetRenew_AnotherMembersLeftRecordNamingTheSerialDoesNotStopTheRenewal(t *testing.T) {
	ctx := context.Background()
	f, _, old, due := renewFixture(t)
	mallory, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	pushedBranch(t, f, "agent/mallory", map[string]string{
		memberRecordPath("kb", "mallory"): leftRecordFile(t, "mallory", mallory, old.SerialNumber.Text(16)),
	})
	restore := SetFleetClockForTest(due)
	renewed, err := f.master.renewFleetLeaves(ctx, f.mri)
	restore()
	require.NoError(t, err)
	require.Contains(t, renewed, "alpha", "mallory's record naming alpha's serial blocks nothing")

	f2, _, old2, due2 := renewFixture(t)
	pushedBranch(t, f2, "agent/alpha", map[string]string{
		memberRecordPath("kb", "alpha"): leftRecordFile(t, "alpha", old2.PublicKey.(ed25519.PublicKey), old2.SerialNumber.Text(16)),
	})
	restore = SetFleetClockForTest(due2)
	renewed, err = f2.master.renewFleetLeaves(ctx, f2.mri)
	restore()
	require.NoError(t, err)
	require.NotContains(t, renewed, "alpha", "positive control: alpha's own left record stops alpha's renewal")
	require.Contains(t, renewed, "master", "and only alpha's: the master's due leaf is renewed in the same call")
}

// #654 review S1: a pushed branch whose records for ONE agent cannot be read
// stops that agent's renewal only (fail closed per agent: its own `left`
// record could be the unreadable one); every other due leaf is renewed.
// SABOTAGE: return the read error from pushedLeaves for the whole duty (the
// first #654 revision) -> nothing is renewed, the master included -> red.
func TestFleetRenew_AnUnreadablePushedFolderStopsOnlyThatAgent(t *testing.T) {
	ctx := context.Background()
	f, _, _, due := renewFixture(t)
	pushedBranch(t, f, "agent/other", nil)
	fleetRenewHooksMu.Lock()
	fleetRenewTestHooks = fleetRenewHooks{agentRecords: func(branch, agent string) error {
		if branch == "agent/other" && agent == "alpha" {
			return errors.New("injected: alpha's folder cannot be read")
		}
		return nil
	}}
	fleetRenewHooksMu.Unlock()
	t.Cleanup(func() {
		fleetRenewHooksMu.Lock()
		fleetRenewTestHooks = fleetRenewHooks{}
		fleetRenewHooksMu.Unlock()
	})
	restore := SetFleetClockForTest(due)
	renewed, err := f.master.renewFleetLeaves(ctx, f.mri)
	restore()
	require.NoError(t, err)
	require.NotContains(t, renewed, "alpha", "alpha's records on a pushed branch are unreadable: not renewed")
	require.Contains(t, renewed, "master", "the master's renewal does not depend on alpha's folder")
}
