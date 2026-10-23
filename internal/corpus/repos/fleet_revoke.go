package repos

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"knomit/internal/domain/fact"
	"knomit/internal/foundation/pki"
)

// Fleet revoke (master): the CRL derivation and the one caller of
// pki.InstallCRL on the membership side (archtest installCRLCallers; the tick
// duty in fleet_tick.go is the other). See fleet_membership.go for the
// operations overview.

// FleetRevocation is what POST /fleet/revoke answers.
type FleetRevocation struct {
	AgentID      string `json:"agent_id"`
	Serial       string `json:"serial"`
	CRLNumber    string `json:"crl_number"`
	Published    bool   `json:"published"`
	PublishError string `json:"publish_error,omitempty"`
	InstallError string `json:"install_error,omitempty"`
}

// RevokeFleetMember revokes agentID's leaf (master only): a new CRL from the
// CURRENT one on the fleet's main plus the leaf's serial, and its signed
// revocation statement, committed on the master's agent branch and
// published; the CRL is installed here at once and copied to <home>/fleet.
func (m *Manager) RevokeFleetMember(ctx context.Context, agentID, reason string) (FleetRevocation, error) {
	defer m.lockFleetOp()()
	ri, err := m.masterFleet()
	if err != nil {
		return FleetRevocation{}, err
	}
	if agentID == m.AgentID() {
		return FleetRevocation{}, fleetErr(http.StatusConflict, "own_id", "this is the master's own agent id: decommission the fleet instead")
	}
	f, err := m.fleetAtMain(ri)
	if err != nil {
		return FleetRevocation{}, err
	}
	if f.PKI == nil || f.PKI.CRL == nil {
		return FleetRevocation{}, errors.New("fleet revoke: the fleet's main holds no usable CRL")
	}
	leaf, err := f.PKI.Cert(agentID)
	if err != nil {
		return FleetRevocation{}, err
	}
	if leaf == nil {
		return FleetRevocation{}, fleetErr(http.StatusNotFound, "not_enrolled", "the fleet's main holds no certificate for agent "+agentID)
	}
	root, err := pki.LoadRoot(m.fleetRootDir())
	if err != nil {
		return FleetRevocation{}, err
	}
	out, _, err := m.revokeLeaf(ctx, ri, root, f.PKI.CRL, agentID, leaf, reason)
	if err != nil {
		return FleetRevocation{}, err
	}
	if perr := m.publishMasterWrite(ctx, ri); perr != nil {
		out.Published, out.PublishError = false, perr.Error()
	}
	return out, nil
}

// revokeLeaf is the one revocation both Revoke and the master's leave duty
// (revokeLeftMembers) make: a new CRL from base (main's current CRL, or the
// one a revocation earlier in the same pass made) plus leaf's serial, and its
// signed revocation statement, committed on the master's agent branch; then
// the master's own copies (the local audit log and CRL, what GET /fleet/root
// hands a new master) and its installed CRL. It does NOT publish: the caller
// does (Revoke at once, the duty through its own round). The answer's
// Published is true; the new CRL is returned for the next revocation of a
// pass. The caller holds fleetOpMu.
func (m *Manager) revokeLeaf(ctx context.Context, ri *RepoInstance, root pki.Root, base *x509.RevocationList, agentID string, leaf *x509.Certificate, reason string) (FleetRevocation, *x509.RevocationList, error) {
	now := time.Now()
	rv, err := pki.RevokeSerial(root, base, pki.Revoked{Serial: leaf.SerialNumber, At: now, Reason: reason}, now, now.Add(fleetCRLValidity))
	switch {
	case errors.Is(err, pki.ErrAlreadyRevoked):
		return FleetRevocation{}, nil, fleetErr(http.StatusConflict, "already_revoked", "agent "+agentID+"'s certificate is already on the CRL")
	case err != nil:
		return FleetRevocation{}, nil, fleetErr(http.StatusUnprocessableEntity, "revoke_failed", err.Error())
	}
	next, err := parseCRLPEM(rv.CRLPEM)
	if err != nil {
		return FleetRevocation{}, nil, fleetErr(http.StatusUnprocessableEntity, "revoke_failed", err.Error())
	}
	ont := ri.Ontology()
	layout, _ := ont.PKILayout()
	statements, err := artifactPattern(ont, fact.PKIRevocation)
	if err != nil {
		return FleetRevocation{}, nil, err
	}
	serial := leaf.SerialNumber.Text(16)
	if err := m.writeFleetArtifacts(ctx, ri, map[string]string{
		layout.CRL: string(rv.CRLPEM),
		strings.Replace(statements, "*", serial, 1): string(rv.Statement),
	}, "fleet: revoke "+agentID+" (serial "+serial+")"); err != nil {
		return FleetRevocation{}, nil, err
	}
	out := FleetRevocation{AgentID: agentID, Serial: serial, CRLNumber: rv.Number.String(), Published: true}
	// The master's own copies: its local audit log and CRL (what GET
	// /fleet/root hands a new master), and its installed CRL.
	if err := pki.RecordRevocation(m.fleetRootDir(), rv); err != nil {
		log.Warn().Err(err).Msg("fleet revoke: the local revocation log or CRL copy was not written")
	}
	if err := installSignedCRL(m.deps.Cfg.TLS.Dir, root.Cert, rv.CRLPEM, rv.Number); err != nil {
		out.InstallError = err.Error()
	}
	log.Info().Str("agent", agentID).Str("serial", serial).Str("reason", reason).Msg("fleet: member revoked")
	return out, next, nil
}

// installSignedCRL installs the CRL this master just signed (raw, Number
// number) into dir. The tick runs outside fleetOpMu (fleet_tick.go), so
// between the commit and this install it may already have installed the
// same CRL from main: pki.InstallCRL then refuses an EQUAL Number with
// ErrCRLRollback, which is success here, as the tick treats it (#536 review
// nit 3). An accepted Number that is not this one stays an error.
func installSignedCRL(dir string, root *x509.Certificate, raw []byte, number *big.Int) error {
	_, err := pki.InstallCRL(dir, root, raw)
	if errors.Is(err, pki.ErrCRLRollback) {
		if acc, aerr := pki.AcceptedNumber(dir, root); aerr == nil && acc != nil && number != nil && acc.Cmp(number) == 0 {
			return nil
		}
	}
	return err
}

// parseCRLPEM parses one X509 CRL PEM block.
func parseCRLPEM(raw []byte) (*x509.RevocationList, error) {
	blk, _ := pem.Decode(raw)
	if blk == nil || blk.Type != "X509 CRL" {
		return nil, errors.New("no X509 CRL PEM block")
	}
	return x509.ParseRevocationList(blk.Bytes)
}
