package repos

import (
	"errors"
	"strings"
	"time"

	"knomit/internal/store/verify"

	"github.com/rs/zerolog/log"

	"knomit/internal/pki"
)

// A refused sync of the fleet repository, classified (fleet design S2; GET
// /fleet last_error_refusal). A member tells these apart from what it holds
// itself, without asking anyone:
//
//   - revoked: the peer refused this instance's certificate at the TLS
//     handshake with the bad_certificate alert (pki.ErrRefusedByPeer; the
//     alert crypto/tls sends for every VerifyConnection refusal) while that
//     certificate is VALID here: it chains to the installed root, is inside
//     its validity, and the installed CRL does not list it. This instance
//     verified the peer under the same root first (the handshake got that
//     far), so the peer is of this fleet, and the one thing such a peer
//     refuses a valid certificate of its own root for is its CRL. Also
//     revoked: the installed CRL lists the installed certificate (a forge
//     member's own round installed it, fleet_tick.go).
//   - moved: THIS instance refused the peer: the address presents a
//     certificate that does not chain to the installed root
//     (pki.ErrUntrustedRoot, this instance's own verdict): another fleet, or
//     the fleet on another root, answers there now.
//   - other: the peer refused this instance's certificate and it is NOT
//     valid here either (expired or not yet valid by this clock, or not of
//     the installed root), or the alert is not bad_certificate (a host whose
//     trust material is uninstalled fails the handshake before judging
//     anyone).
//
// A revoked member's origin rounds stop (fleetSyncHalt): no fetch, no push.
// The stop is kept in memory, so a restart retries once; GET /fleet keeps
// saying revoked from the recorded error and the installed material.

// The classes of last_error_refusal.
const (
	FleetRefusalRevoked = "revoked"
	FleetRefusalMoved   = "moved"
	FleetRefusalOther   = "other"
)

// tlsBadCertificateAlert is how crypto/tls reports the bad_certificate alert
// a peer sends when its VerifyConnection refuses this instance's
// certificate.
const tlsBadCertificateAlert = "tls: bad certificate"

// classifyFleetRefusal classifies a recorded sync error of the fleet
// repository given this instance's own standing (pki.InstalledStanding): one
// of the FleetRefusal* classes, "" when the error is no TLS refusal. A
// commit refused by the host's judge is not a TLS refusal, whatever its text
// quotes.
func classifyFleetRefusal(lastErr string, own pki.Standing) string {
	switch {
	case lastErr == "" || strings.Contains(lastErr, verify.CommitRefusedByJudge) || strings.Contains(lastErr, verify.CommitRefusedByValidation):
		return ""
	case strings.Contains(lastErr, pki.ErrRefusedByPeer.Error()):
		if own == pki.StandingRevoked || own == pki.StandingValid && strings.Contains(lastErr, tlsBadCertificateAlert) {
			return FleetRefusalRevoked
		}
		return FleetRefusalOther
	case strings.Contains(lastErr, pki.ErrUntrustedRoot.Error()):
		return FleetRefusalMoved
	}
	return ""
}

// ownStanding is this instance's certificate as its installed material says
// now, and its serial (lowercase hex, "" with none).
func (m *Manager) ownStanding() (pki.Standing, string) {
	st, leaf := pki.InstalledStanding(m.deps.Cfg.TLS.Dir, time.Now())
	if leaf == nil || leaf.SerialNumber == nil {
		return st, ""
	}
	return st, leaf.SerialNumber.Text(16)
}

// fleetRevokedHere reports whether this instance's certificate is revoked as
// far as it can tell: lastErr (the fleet's last recorded sync error)
// classifies revoked, or the installed CRL lists the installed certificate.
// Never on a holder of the fleet root (the master, or one that may become
// it). serial is the installed certificate's.
func (m *Manager) fleetRevokedHere(lastErr string) (revoked bool, serial string) {
	if holds, err := m.rootKeyHere(); err != nil || holds {
		return false, ""
	}
	own, serial := m.ownStanding()
	if serial == "" {
		return false, ""
	}
	return own == pki.StandingRevoked || classifyFleetRefusal(lastErr, own) == FleetRefusalRevoked, serial
}

// noteFleetRevoked stops the fleet repository's origin rounds while this
// instance's certificate is revoked (fleetRevokedHere). Called with every
// recorded push outcome (fleetPushed) and after the tick installs a CRL.
func (m *Manager) noteFleetRevoked(lastErr string) {
	revoked, serial := m.fleetRevokedHere(lastErr)
	if !revoked {
		return
	}
	if cur := m.fleetHaltSerial.Load(); cur == nil || *cur != serial {
		m.fleetHaltSerial.Store(&serial)
		log.Warn().Str("serial", serial).Msg("fleet: this instance's certificate is revoked; the fleet repository's sync stops (leave the fleet, then join again with a new bundle)")
	}
}

// fleetFetchRefused is fleetPeers.fetchRefused: a fetch of the fleet
// repository that failed at the TLS handshake, either side refusing
// (pki.ErrRefusedByPeer, pki.ErrUntrustedRoot), is recorded as the fleet's
// last attempt, like a push's outcome (a hub member refused at the handshake
// may have nothing to push, and a push with nothing to send succeeds without
// dialing), and a revoked certificate stops the rounds (noteFleetRevoked).
// Answers true for such a refusal: the round pushes nothing.
func (ri *RepoInstance) fleetFetchRefused(err error) bool {
	if ri == nil || !ri.fleetDoor || !ri.IsFleet() || err == nil {
		return false
	}
	if !errors.Is(err, pki.ErrRefusedByPeer) && !errors.Is(err, pki.ErrUntrustedRoot) {
		return false
	}
	m := ri.env.m
	if db := m.ControlDB(); db != nil {
		if rerr := recordFleetAttempt(db, err, time.Now()); rerr != nil {
			log.Warn().Err(rerr).Msg("fleet: recording the refused fetch failed")
		}
	}
	m.noteFleetRevoked(err.Error())
	return true
}

// fleetSyncHalt is fleetPeers.halt: why this copy's origin rounds stop, ""
// while they run. They stop while the certificate this process found
// revoked (noteFleetRevoked) is still the installed one; another certificate
// (a leave and a join) runs them again.
func (ri *RepoInstance) fleetSyncHalt() string {
	if ri == nil || !ri.fleetDoor || !ri.IsFleet() {
		return ""
	}
	m := ri.env.m
	halted := m.fleetHaltSerial.Load()
	if halted == nil {
		return ""
	}
	if _, serial := m.ownStanding(); serial != *halted {
		return ""
	}
	return "this instance's certificate is revoked"
}
