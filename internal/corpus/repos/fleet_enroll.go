package repos

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"knomit/internal/engine/store/fleetrec"

	"github.com/rs/zerolog/log"
	"golang.org/x/crypto/ssh"

	"knomit/internal/domain/fact"
	"knomit/internal/engine/store"
	"knomit/internal/foundation/pki"
)

// Fleet enroll (master): issue a leaf for (agent id, ssh-ed25519 key), commit it
// and its issuance record on the master's agent branch, publish main, and hand
// back the bundle document. See fleet_membership.go for the operations overview.

// FleetEnrollment is what POST /fleet/enroll answers.
type FleetEnrollment struct {
	AgentID string `json:"agent_id"`
	Serial  string `json:"serial"`
	Bundle  string `json:"bundle"` // the bundle document: header (fleet URL, agent, root, serial), then leaf, root and CRL PEM
	// FleetURL is the fleet repository URL the bundle carries.
	FleetURL        string `json:"fleet_url"`
	RootFingerprint string `json:"root_fingerprint"`
	// Published says main holds the leaf now (a joiner is admitted only
	// then); PublishError is why not. The sync loop publishes it again.
	Published    bool   `json:"published"`
	PublishError string `json:"publish_error,omitempty"`
}

func certPEMOf(c *x509.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})
}

func crlPEMOf(c *x509.RevocationList) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "X509 CRL", Bytes: c.Raw})
}

// artifactPattern is the repo path pattern of the ontology's one artifact
// rule with pki role role (one '*' where the file name goes).
func artifactPattern(ont *fact.Ontology, role string) (string, error) {
	var found []string
	for pat, block := range ont.Artifacts {
		if v, _ := block[fact.ArtifactKeyPKI].(string); v == role {
			found = append(found, pat)
		}
	}
	if len(found) != 1 || strings.Count(found[0], "*") != 1 {
		return "", fmt.Errorf("fleet: the fleet ontology must declare exactly one pki: %s pattern with one '*', found %v", role, found)
	}
	return fact.ArtifactsRoot + "/" + found[0], nil
}

// parseKeyLine reads an ssh-ed25519 public key line (`knomit identity show`
// prints it).
func parseKeyLine(line string) (ed25519.PublicKey, error) {
	pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(strings.TrimSpace(line)))
	if err != nil {
		return nil, fleetErr(http.StatusUnprocessableEntity, "invalid_key", "public_key is not an ssh public key line: "+err.Error())
	}
	cpk, ok := pk.(ssh.CryptoPublicKey)
	if !ok {
		return nil, fleetErr(http.StatusUnprocessableEntity, "invalid_key", "public_key is not usable for a certificate")
	}
	pub, ok := cpk.CryptoPublicKey().(ed25519.PublicKey)
	if !ok {
		return nil, fleetErr(http.StatusUnprocessableEntity, "invalid_key", "public_key is "+pk.Type()+", want ssh-ed25519")
	}
	return pub, nil
}

// EnrollFleetMember issues a leaf for agentID's key (master only), commits it
// and its issuance record on the master's agent branch, publishes main and
// returns the bundle the joiner joins with (POST /fleet/join). address picks
// which of this instance's external_addresses the bundle's fleet URL is built
// from (fleetURLForBundle); every refusal comes before the leaf is issued.
func (m *Manager) EnrollFleetMember(ctx context.Context, agentID, keyLine, address string) (FleetEnrollment, error) {
	defer m.lockFleetOp()()
	if err := pki.ValidAgentID(agentID); err != nil {
		return FleetEnrollment{}, fleetErr(http.StatusUnprocessableEntity, "invalid_agent_id", err.Error())
	}
	pub, err := parseKeyLine(keyLine)
	if err != nil {
		return FleetEnrollment{}, err
	}
	ri, err := m.masterFleet()
	if err != nil {
		return FleetEnrollment{}, err
	}
	if agentID == m.AgentID() {
		return FleetEnrollment{}, fleetErr(http.StatusConflict, "own_id", "this is the master's own agent id: its leaf is issued at create")
	}
	fleetURL, err := m.fleetURLForBundle(ri, address)
	if err != nil {
		return FleetEnrollment{}, err
	}
	root, err := pki.LoadRoot(m.fleetRootDir())
	if err != nil {
		return FleetEnrollment{}, err
	}
	f, err := m.fleetAtMain(ri)
	if err != nil {
		return FleetEnrollment{}, err
	}
	if f.PKI == nil || f.PKI.CRL == nil {
		return FleetEnrollment{}, errors.New("fleet enroll: the fleet's main holds no usable CRL")
	}
	if err := refuseEnrolledID(f.PKI, agentID, pub); err != nil {
		return FleetEnrollment{}, err
	}
	if err := m.refuseRenewalWithoutRecord(ri, f.PKI, agentID); err != nil {
		return FleetEnrollment{}, err
	}
	email, err := pki.AgentEmail(agentID)
	if err != nil {
		return FleetEnrollment{}, err
	}
	ont := ri.Ontology()
	layout, ok := ont.PKILayout()
	if !ok || !layout.HasLeaves() {
		return FleetEnrollment{}, errors.New("fleet enroll: the fleet ontology declares no pki leaf")
	}
	issued, err := artifactPattern(ont, fact.PKIIssuance)
	if err != nil {
		return FleetEnrollment{}, err
	}
	is, err := pki.IssueLeaf(root, pub, email, fleetLeafValidity, m.renewalIssueTime(ri, agentID))
	if err != nil {
		return FleetEnrollment{}, err
	}
	certPEM, rec := is.CertPEM, is.Record
	if err := m.writeFleetArtifacts(ctx, ri, map[string]string{
		layout.LeafPath(agentID):                    string(certPEM),
		strings.Replace(issued, "*", rec.Serial, 1): string(certPEM),
	}, "fleet: "+agentID+"'s certificate (serial "+rec.Serial+")"); err != nil {
		return FleetEnrollment{}, err
	}
	// The audit line only for a leaf the fleet repository holds (revoke
	// finds the serial in certs/<id>.pem on main, not in this log).
	if err := pki.RecordIssuance(m.fleetRootDir(), is); err != nil {
		log.Warn().Err(err).Str("agent", agentID).Str("serial", rec.Serial).Msg("fleet enroll: the local issuance log was not written")
	}
	rootID, err := pki.RootID(root.Cert)
	if err != nil {
		return FleetEnrollment{}, err
	}
	var bundle bytes.Buffer
	bundle.Write(certPEM)
	bundle.Write(certPEMOf(root.Cert))
	bundle.Write(crlPEMOf(f.PKI.CRL))
	doc, err := pki.FleetBundle(bundle.Bytes(), fleetURL)
	if err != nil {
		return FleetEnrollment{}, err
	}
	out := FleetEnrollment{AgentID: agentID, Serial: rec.Serial, Bundle: string(doc),
		FleetURL: fleetURL, RootFingerprint: rootID, Published: true}
	if perr := m.publishMasterWrite(ctx, ri); perr != nil {
		log.Warn().Err(perr).Str("agent", agentID).Msg("fleet enroll: main does not hold the leaf yet; the sync loop publishes it")
		out.Published, out.PublishError = false, perr.Error()
	}
	log.Info().Str("agent", agentID).Str("serial", rec.Serial).Msg("fleet: member enrolled")
	return out, nil
}

// refuseRenewalWithoutRecord refuses (409 never_joined) an enroll that would
// REPLACE the leaf main holds for agentID while the master's agent branch
// holds no member record for that id (exactly one is required): an id that
// was enrolled and revoked but never joined. The fleet ontology's
// replace_when: leaf-renewal admits a replaced leaf only for the one member
// record of its id, and the judge holds the master's own commits to it
// before they reach main (judgeOwnRange), so writing this leaf would leave a
// commit on the master's branch that main can never take. The §1.5 rejoin,
// of an id whose record is on the branch, is unaffected.
func (m *Manager) refuseRenewalWithoutRecord(ri *RepoInstance, onMain *fleetrec.PKI, agentID string) error {
	if leaf, err := onMain.Cert(agentID); err != nil || leaf == nil {
		return err
	}
	n := 0
	var rerr error
	if err := ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			rerr = errors.New("fleet repository is not open")
			return
		}
		f, err := svc.FleetAt(ri.AgentBranch())
		if err != nil {
			rerr = err
			return
		}
		for _, mem := range f.Members {
			if mem.Agent == agentID {
				n++
			}
		}
	}); err != nil {
		return err
	}
	if rerr != nil {
		return rerr
	}
	if n != 1 {
		return fleetErr(http.StatusConflict, "never_joined", "agent "+agentID+" has a revoked certificate but no member record: its certificate can be replaced only for a member that joined (a rejoin); enroll it under a new agent id")
	}
	return nil
}

// renewalIssueTime is the issue time of a leaf for agentID: now, or, when the
// master's agent branch already holds a leaf for that id that the new one
// replaces, late enough that the new leaf's NotBefore is strictly after the
// old one's (pki.IssueTimeAfter). The judge refuses the master's own commit
// otherwise (artifact-replace, replace_when: leaf-renewal) and main would
// not move: a revoke and a re-enroll within one second issue two leaves with
// the same NotBefore.
func (m *Manager) renewalIssueTime(ri *RepoInstance, agentID string) time.Time {
	now := time.Now()
	var old *x509.Certificate
	_ = ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		if f, err := svc.FleetAt(ri.AgentBranch()); err == nil && f.PKI != nil {
			old, _ = f.PKI.Cert(agentID)
		}
	})
	if old == nil {
		return now
	}
	if after := pki.IssueTimeAfter(old.NotBefore); after.After(now) {
		return after
	}
	return now
}

// fleetURLForBundle is the fleet repository URL an enroll's bundle carries:
// the one members use.
//   - The fleet repository here syncs with an origin (a forge, or the hub
//     when this master is not it): that origin's URL. address is refused
//     (address_not_applicable): there is nothing to choose.
//   - This instance is the hub: https://<external address>/git/<repo>, at the
//     external address the caller chose. With exactly one configured, it
//     needs no choice; with several and none given, address_required lists
//     them (Choices); one that is not configured is address_unknown; one
//     that is not https:// is address_not_https (members reach the hub's
//     mTLS listener).
func (m *Manager) fleetURLForBundle(ri *RepoInstance, address string) (string, error) {
	o, err := m.originOf(ri)
	if err != nil {
		return "", err
	}
	if o != nil {
		// Without its userinfo: a joiner supplies its own credential, and a
		// token in the origin URL is the master's own (#527 F3).
		u := credentialFree(o.URL)
		if address != "" {
			return "", fleetErr(http.StatusUnprocessableEntity, "address_not_applicable", "this fleet's repository syncs with "+u+", the URL members use: enroll without an address")
		}
		return u, nil
	}
	addrs := m.deps.Cfg.ExternalAddresses
	switch {
	case address != "":
		known := false
		for _, a := range addrs {
			known = known || a == address
		}
		if !known {
			return "", &FleetError{Status: http.StatusUnprocessableEntity, Code: "address_unknown",
				Msg: "address " + address + " is not one of this instance's external_addresses (knomit.toml)", Choices: append([]string{}, addrs...)}
		}
	case len(addrs) == 1:
		address = addrs[0]
	case len(addrs) == 0:
		return "", fleetErr(http.StatusUnprocessableEntity, "addresses_required", "a fleet hosted here needs external_addresses in knomit.toml: they are how members reach this instance")
	default:
		return "", &FleetError{Status: http.StatusUnprocessableEntity, Code: "address_required",
			Msg: "this instance has several external_addresses: choose the one the member reaches it at (address)", Choices: append([]string{}, addrs...)}
	}
	if !strings.HasPrefix(strings.ToLower(address), "https://") {
		return "", fleetErr(http.StatusUnprocessableEntity, "address_not_https", "address "+address+" is not https://: members reach the hub's mTLS listener ([tls].addr) over https")
	}
	return strings.TrimSuffix(address, "/") + "/git/" + ri.Name(), nil
}

// refuseEnrolledID refuses an enroll of agentID while main holds a leaf for
// it that a revoke could still reach: the enroll would replace certs/<id>.pem,
// revoke finds the serial only there, so the old leaf could never be revoked.
// A different key under an id that has a leaf is refused always (§1.5: a lost
// or rotated key is a fresh start under a new agent id). The same key with
// its old leaf on main's CRL is the §1.5 Rejoin.
func refuseEnrolledID(p *fleetrec.PKI, agentID string, pub ed25519.PublicKey) error {
	leaf, err := p.Cert(agentID)
	if err != nil {
		return err
	}
	if leaf == nil {
		return nil
	}
	if !pub.Equal(leaf.PublicKey) {
		return fleetErr(http.StatusConflict, "rekey_unsupported", "agent "+agentID+" already has a certificate for another key: a lost or rotated key is a fresh start under a new agent id (a same-id rekey is not supported)")
	}
	if !pki.IsRevoked(p.CRL, leaf.SerialNumber) {
		return fleetErr(http.StatusConflict, "enrolled", "agent "+agentID+" is enrolled (serial "+leaf.SerialNumber.Text(16)+" is not revoked): revoke it first (Fleet → Members → Revoke certificate)")
	}
	return nil
}
