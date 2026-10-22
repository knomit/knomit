package repos

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/foundation/pki"
)

// mainLeaf is id's leaf on the forge's main.
func (f *forgeFleet) mainLeaf(t *testing.T, id string) *x509.Certificate {
	t.Helper()
	raw, err := runGitOut(f.bare, "show", "main:artifacts/fleet/certs/"+id+".pem")
	require.NoError(t, err)
	blk, _ := pem.Decode([]byte(raw))
	require.NotNil(t, blk)
	c, err := x509.ParseCertificate(blk.Bytes)
	require.NoError(t, err)
	return c
}

// installedLeaf is the instance certificate installed in m's [tls].dir.
func installedLeaf(t *testing.T, m *Manager) *x509.Certificate {
	t.Helper()
	c, err := pki.LoadInstanceCert(m.deps.Cfg.TLS.Dir)
	require.NoError(t, err)
	return c
}

// The renewal duties on a forge-hosted fleet. With the decision clock moved
// into the final third of the leaves' and the CRL's own lifetimes, ONE master
// round re-signs the CRL (same entries, Number + 1, its own lifetime from
// now) and re-issues every due leaf (alpha's and the master's own) for the
// same key and its own lifetime, with the issuance record, and publishes them
// (main moved: the master's own-commit judge admitted every one, leaf-renewal
// included). Bravo, who left (its record says left), is not renewed. With the
// real clock back, another round changes nothing. Alpha's round installs its
// renewed leaf; the master's installs its own.
// SABOTAGE: drop fleetRenewRound from fleetMasterDuty -> main keeps the old
// leaves -> red; renew a `left` record's leaf -> bravo's serial changes ->
// red; issue for fleetLeafValidity instead of the leaf's own lifetime ->
// the master's 1 h leaf comes back 90 days long -> red; drop installMainLeaf
// from the tick -> alpha keeps its old certificate -> red.
func TestFleetRenew_ForgeMasterRenewsDueLeavesAndResignsTheCRL(t *testing.T) {
	ctx := context.Background()
	f := newForgeFleet(t, nil)
	installMasterLeaf(t, f)
	// The master's key file is the key it signs with (installMasterLeaf
	// wrote it), as on a real instance: its own renewal installs only for it.
	f.master.deps.KeyPath = filepath.Join(f.dir, "master-key", "master_ed25519")
	alpha := f.namedMember(t, "alpha")
	bravo := f.namedMember(t, "bravo")
	f.enrollAndJoin(t, alpha, "alpha")
	f.enrollAndJoin(t, bravo, "bravo")
	f.masterRounds(t)
	_, err := bravo.LeaveFleet(ctx)
	require.NoError(t, err)
	// Bravo leaves (the leave duty revokes that leaf) and the master enrolls
	// it again before it rejoins: main holds bravo's `left` record and a
	// FRESH, unrevoked leaf the left record does not name. Only the `left`
	// state keeps that leaf from a renewal (leaf-renewal would need an
	// active record anyway).
	f.masterRounds(t)
	_, err = f.master.EnrollFleetMember(ctx, "bravo", keyLine("bravo"), "")
	require.NoError(t, err)
	require.Equal(t, "left", memberState(t, f.master, "bravo"), "fixture: main says left, the fresh leaf is not revoked")

	oldAlpha, oldMaster, oldBravo := f.mainLeaf(t, "alpha"), f.mainLeaf(t, "master"), f.mainLeaf(t, "bravo")
	oldCRL := f.mainCRLNumber(t)
	mainBefore := f.rev(t, "main")
	// Fixture: nothing is due at the real clock.
	round(t, f.mri)
	require.Equal(t, mainBefore, f.rev(t, "main"), "fixture: nothing falls due now")

	// The decision clock in the final third of alpha's 90-day leaf (and so
	// past the master's 1 h leaf and the 1 h CRL).
	due := time.Now().Add(pki.Lifetime(oldAlpha) * 3 / 4)
	restore := SetFleetClockForTest(func() time.Time { return due })
	round(t, f.mri)
	restore()

	require.NotEqual(t, mainBefore, f.rev(t, "main"), "the round published the renewals")
	require.Equal(t, oldCRL+1, f.mainCRLNumber(t), "the CRL is re-signed with Number + 1")
	for _, tc := range []struct {
		id  string
		old *x509.Certificate
	}{{"alpha", oldAlpha}, {"master", oldMaster}} {
		got := f.mainLeaf(t, tc.id)
		require.NotEqual(t, tc.old.SerialNumber, got.SerialNumber, "%s's leaf is re-issued", tc.id)
		require.True(t, got.NotBefore.After(tc.old.NotBefore), "%s: a later NotBefore", tc.id)
		require.Equal(t, tc.old.PublicKey, got.PublicKey, "%s: the same key", tc.id)
		require.Equal(t, tc.old.EmailAddresses, got.EmailAddresses, "%s: the same identity", tc.id)
		require.Equal(t, pki.Lifetime(tc.old), pki.Lifetime(got), "%s: its own lifetime", tc.id)
		require.True(t, f.onForgeMain(t, "artifacts/fleet/issued/"+got.SerialNumber.Text(16)+".pem"), "%s: the issuance record", tc.id)
	}
	require.Equal(t, oldBravo.SerialNumber, f.mainLeaf(t, "bravo").SerialNumber, "a left member is not renewed")
	crl, err := pki.LoadCRL(filepath.Join(f.master.fleetRootDir(), pki.CRLFile))
	require.NoError(t, err)
	require.Equal(t, oldCRL+1, crl.Number.Int64(), "the master's own CRL copy follows")

	after := f.rev(t, "main")
	round(t, f.mri)
	require.Equal(t, after, f.rev(t, "main"), "with the real clock nothing more is due")

	round(t, alpha.fleetRepo())
	require.True(t, bytes.Equal(f.mainLeaf(t, "alpha").Raw, installedLeaf(t, alpha).Raw), "alpha's round installs its renewed certificate")
	n, _ := installedCRL(t, alpha)
	require.Equal(t, oldCRL+1, n, "and the re-signed CRL")
	round(t, f.mri)
	require.True(t, bytes.Equal(f.mainLeaf(t, "master").Raw, installedLeaf(t, f.master).Raw), "the master installs its own renewal")
	_, err = os.Stat(filepath.Join(f.master.fleetRootDir(), pki.IssuedLogFile))
	require.NoError(t, err, "the renewals are in the local issuance log")
}
