package repos

import (
	"context"
	"errors"
	"math/big"

	"github.com/rs/zerolog/log"

	"knomit/internal/pki"
	"knomit/internal/store"
)

// The tick duty of every copy of the fleet repository (fleet design P6): in
// each round, after the fetch, this instance installs what the fleet's main
// holds for it and is newer than what [tls].dir holds. Master or member, hub
// or forge: it reads main only, so it acts on what the master published.
//
//   - The CRL: main's crl.pem, when its Number is above the one this
//     instance accepted for the installed root (pki.InstallCRL, which checks
//     the root and the Number again under the install lock). After a revoke,
//     every member's mTLS listener refuses the revoked peer once its own
//     round has run: the listener adopts the new crl.pem on its next
//     handshake or tick (pki.Server's reloader).
//
// It runs in the forge round after a successful fetch (fleetPeers.tick,
// before the master's duties) and in the hub's local round (fleetHostRound).
// It takes no fleetOpMu: the install lock serializes it with a revoke's own
// install, and a CRL already installed is skipped, never an error.

// fleetTickRound is fleetPeers.tick for ri: the tick duty above. A failure
// is logged; the next round tries again.
func (ri *RepoInstance) fleetTickRound(ctx context.Context, svc *store.Service) {
	if ri == nil || !ri.fleetDoor || !ri.IsFleet() || svc == nil || ctx.Err() != nil {
		return
	}
	m := ri.env.m
	n, err := m.installMainCRL(svc)
	switch {
	case err != nil:
		log.Warn().Err(err).Str("fleet", ri.Name()).Msg("fleet: main's CRL was not installed here; the next round retries")
	case n != nil:
		log.Info().Str("fleet", ri.Name()).Str("crl_number", n.String()).Msg("fleet: installed the newer CRL from main")
		// A CRL that lists this instance's own certificate stops its rounds
		// from the next one (fleet_refusal.go).
		m.noteFleetRevoked("")
	}
}

// installMainCRL installs the CRL on svc's main (the fleet repository) into
// [tls].dir when it is newer than the CRL this instance accepted for the
// installed root, and answers its Number; nil, nil when there is nothing to
// install: no PKI or no usable CRL on main, main's root not the installed
// one (nothing installed here included; logged: a CRL never installs or
// changes a root), or a Number not above the accepted one.
func (m *Manager) installMainCRL(svc *store.Service) (*big.Int, error) {
	dir := m.deps.Cfg.TLS.Dir
	installed, err := pki.InstalledRoot(dir)
	if err != nil {
		return nil, err
	}
	f, err := svc.FleetAt(svc.UpstreamBranch())
	if err != nil {
		return nil, err
	}
	if f.PKI == nil || f.PKI.Root == nil || f.PKI.CRL == nil || f.PKI.CRL.Number == nil {
		return nil, nil
	}
	onMain, err := pki.RootID(f.PKI.Root)
	if err != nil {
		return nil, err
	}
	if onMain != installed.Fingerprint {
		log.Warn().Str("installed_root", installed.Fingerprint).Str("main_root", onMain).
			Msg("fleet: the fleet repository's main holds another root than the one installed here; its CRL is not installed")
		return nil, nil
	}
	accepted, err := pki.AcceptedNumber(dir, f.PKI.Root)
	if err != nil {
		return nil, err
	}
	if accepted != nil && f.PKI.CRL.Number.Cmp(accepted) <= 0 {
		return nil, nil
	}
	n, err := pki.InstallCRL(dir, f.PKI.Root, crlPEMOf(f.PKI.CRL))
	if errors.Is(err, pki.ErrCRLRollback) {
		// Another install (a revoke here) took this Number or a later one
		// between the check and the lock.
		return nil, nil
	}
	return n, err
}
