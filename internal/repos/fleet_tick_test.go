package repos

import (
	"bytes"
	"context"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"

	"knomit/internal/pki"
	"knomit/internal/pki/pkitest"
	"knomit/internal/store"
)

// installedCRL is the Number of the CRL installed in m's [tls].dir and its
// bytes.
func installedCRL(t *testing.T, m *Manager) (int64, []byte) {
	t.Helper()
	p := filepath.Join(m.deps.Cfg.TLS.Dir, pki.CRLFile)
	crl, err := pki.LoadCRL(p)
	require.NoError(t, err)
	raw, err := os.ReadFile(p)
	require.NoError(t, err)
	return crl.Number.Int64(), raw
}

// The P6 tick duty on a forge-hosted fleet: after the master revokes bravo,
// alpha's [tls].dir still holds CRL #1 until alpha's own round, which installs
// main's CRL #2 (the bytes main holds, the watermark raised to 2); a second
// round installs nothing (the file is untouched). Bravo's round installs it
// too: a revoked member that can still read main learns it.
// SABOTAGE: drop peers.runTick from the origin round -> alpha keeps #1 -> red.
// (The Number pre-check only spares a lock and a refusal: InstallCRL itself
// refuses an equal Number, which installMainCRL treats as nothing to do, so
// the mtime assert holds either way.)
func TestFleetTick_AMemberInstallsMainsNewerCRLInItsRound(t *testing.T) {
	ctx := context.Background()
	f := newForgeFleet(t, nil)
	installMasterLeaf(t, f)
	alpha := f.namedMember(t, "alpha")
	bravo := f.namedMember(t, "bravo")
	f.enrollAndJoin(t, alpha, "alpha")
	f.enrollAndJoin(t, bravo, "bravo")
	f.masterRounds(t)

	n, _ := installedCRL(t, alpha)
	require.Equal(t, int64(1), n, "fixture: alpha joined with CRL #1")
	rv, err := f.master.RevokeFleetMember(ctx, "bravo", "test")
	require.NoError(t, err)
	require.True(t, rv.Published, rv.PublishError)
	require.Equal(t, int64(2), f.mainCRLNumber(t))
	n, _ = installedCRL(t, alpha)
	require.Equal(t, int64(1), n, "nothing installs main's CRL on alpha before alpha's round")

	round(t, alpha.fleetRepo())
	n, raw := installedCRL(t, alpha)
	require.Equal(t, int64(2), n, "alpha's round installs main's newer CRL")
	onMain, err := runGitOut(f.bare, "show", "main:artifacts/fleet/crl.pem")
	require.NoError(t, err)
	require.Equal(t, onMain, string(raw), "the bytes main holds")
	root, err := pki.LoadRootCert(filepath.Join(alpha.deps.Cfg.TLS.Dir, pki.RootCertFile))
	require.NoError(t, err)
	accepted, err := pki.AcceptedNumber(alpha.deps.Cfg.TLS.Dir, root)
	require.NoError(t, err)
	require.Equal(t, int64(2), accepted.Int64(), "the watermark is raised with it")

	fi, err := os.Stat(filepath.Join(alpha.deps.Cfg.TLS.Dir, pki.CRLFile))
	require.NoError(t, err)
	round(t, alpha.fleetRepo())
	fi2, err := os.Stat(filepath.Join(alpha.deps.Cfg.TLS.Dir, pki.CRLFile))
	require.NoError(t, err)
	require.Equal(t, fi.ModTime(), fi2.ModTime(), "a CRL already installed is not written again")

	round(t, bravo.fleetRepo())
	n, _ = installedCRL(t, bravo)
	require.Equal(t, int64(2), n, "the revoked member's round installs the CRL that revokes it")
}

// The tick installs nothing for a copy whose [tls].dir holds no root, and
// nothing from a main whose root is not the installed one: a CRL never moves
// an instance to another fleet. Positive control: the same copy installs
// once its own root is back.
// SABOTAGE: drop the installed-root comparison -> InstallCRL refuses with
// RootDiffers, an error each round -> red on the nil error.
func TestFleetTick_NothingInstalledOrAnotherRootInstallsNothing(t *testing.T) {
	ctx := context.Background()
	f := newForgeFleet(t, nil)
	installMasterLeaf(t, f)
	alpha := f.namedMember(t, "alpha")
	f.enrollAndJoin(t, alpha, "alpha")
	f.masterRounds(t)
	bravo := f.namedMember(t, "bravo")
	f.enrollAndJoin(t, bravo, "bravo")
	_, err := f.master.RevokeFleetMember(ctx, "bravo", "test")
	require.NoError(t, err)
	syncFleet(t, alpha)
	dir := alpha.deps.Cfg.TLS.Dir
	ri := alpha.fleetRepo()

	require.NoError(t, pki.Uninstall(dir))
	var n *big.Int
	require.NoError(t, ri.WithRead(func(svc *store.Service) { n, err = alpha.installMainCRL(svc) }))
	require.NoError(t, err)
	require.Nil(t, n, "nothing installed: nothing to install")
	_, statErr := os.Stat(filepath.Join(dir, pki.CRLFile))
	require.True(t, os.IsNotExist(statErr), "no crl.pem written: %v", statErr)

	other := newForgeFleet(t, nil).root
	other.Install(t, other.Enroll(t, "alpha@"+pki.AgentEmailDomain, alpha.deps.KeyPath), dir)
	require.NoError(t, ri.WithRead(func(svc *store.Service) { n, err = alpha.installMainCRL(svc) }))
	require.NoError(t, err)
	require.Nil(t, n, "main's root is not the installed one: nothing installed")
	got, _ := installedCRL(t, alpha)
	require.Equal(t, int64(1), got, "the other fleet's CRL is untouched")

	require.NoError(t, pki.Uninstall(dir))
	f.root.Install(t, f.root.Enroll(t, "alpha@"+pki.AgentEmailDomain, alpha.deps.KeyPath), dir)
	require.NoError(t, ri.WithRead(func(svc *store.Service) { n, err = alpha.installMainCRL(svc) }))
	require.NoError(t, err)
	got, _ = installedCRL(t, alpha)
	require.Equal(t, int64(2), got, "positive control: with its own root installed, main's CRL #2 installs")
}

// #536 review nit 2: with nothing installed the tick installs nothing and
// logs nothing (it used to warn "another root" every round, since "" is not
// main's root). Positive control: another root installed still warns once
// per round.
// SABOTAGE: drop the nothing-installed early return -> a warning -> red.
func TestFleetTick_NothingInstalledLogsNothing(t *testing.T) {
	f := newForgeFleet(t, nil)
	installMasterLeaf(t, f)
	alpha := f.namedMember(t, "alpha")
	f.enrollAndJoin(t, alpha, "alpha")
	dir := alpha.deps.Cfg.TLS.Dir
	require.NoError(t, pki.Uninstall(dir))
	var buf bytes.Buffer
	prev := log.Logger
	log.Logger = zerolog.New(&buf)
	t.Cleanup(func() { log.Logger = prev })
	require.NoError(t, alpha.fleetRepo().WithRead(func(svc *store.Service) {
		n, err := alpha.installMainCRL(svc)
		require.NoError(t, err)
		require.Nil(t, n)
	}))
	require.NotContains(t, buf.String(), "another root", "nothing installed: no warning")

	other := newForgeFleet(t, nil).root
	other.Install(t, other.Enroll(t, "alpha@"+pki.AgentEmailDomain, alpha.deps.KeyPath), dir)
	require.NoError(t, alpha.fleetRepo().WithRead(func(svc *store.Service) {
		_, err := alpha.installMainCRL(svc)
		require.NoError(t, err)
	}))
	require.Contains(t, buf.String(), "another root", "positive control: another installed root still warns")
}

// #536 review nit 3: the master's revoke installs the CRL it signed; when
// the tick (outside fleetOpMu) installed that same CRL from main first, the
// EQUAL Number is success, not an install_error. A CRL older than the one
// accepted is still an error.
// SABOTAGE: return InstallCRL's error as is -> the equal row errs -> red.
func TestInstallSignedCRL_AnEqualNumberIsSuccess(t *testing.T) {
	fl := pkitest.New(t)
	dir := filepath.Join(t.TempDir(), "pki")
	fl.Install(t, fl.Enroll(t, "master@"+pki.AgentEmailDomain), dir)
	cur, err := pki.LoadCRL(filepath.Join(fl.Dir, pki.CRLFile))
	require.NoError(t, err)
	now := time.Now()
	n2 := big.NewInt(cur.Number.Int64() + 1)
	raw2, err := pki.SignCRL(fl.Root, nil, n2, now, now.Add(time.Hour))
	require.NoError(t, err)
	_, err = pki.InstallCRL(dir, fl.Root.Cert, raw2) // the tick, first
	require.NoError(t, err)
	require.NoError(t, installSignedCRL(dir, fl.Root.Cert, raw2, n2), "the same Number, already installed: success")
	raw1 := crlPEMOf(cur)
	require.Error(t, installSignedCRL(dir, fl.Root.Cert, raw1, cur.Number), "an older Number is still refused")
}
