package repos

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/pki"
	"knomit/internal/store"
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
		fl, err := svc.FleetAt(svc.UpstreamBranch())
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
				branch = svc.UpstreamBranch()
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

	due := time.Now().Add(pki.Lifetime(oldAlpha) * 3 / 4)
	restore := SetFleetClockForTest(func() time.Time { return due })
	round(t, f.mri)
	restore()
	for i := 0; i < 4; i++ {
		round(t, f.mri)
		settleMerger(t, f.mri)
	}

	var revokedOnMain bool
	var serialOnMain, state string
	require.NoError(t, f.mri.WithRead(func(svc *store.Service) {
		fl, err := svc.FleetAt(svc.UpstreamBranch())
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
	restore := SetFleetClockForTest(due)
	round(t, f.mri)
	restore()
	require.Equal(t, old.SerialNumber, f.mainLeaf(t, "alpha").SerialNumber, "another key: not renewed")
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
