package repos

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"database/sql"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"golang.org/x/crypto/ssh"

	"knomit/internal/fact"
	"knomit/internal/pki"
	"knomit/internal/platform/privdir"
	"knomit/internal/store"
)

// Fleet membership (POST /api/v1/fleet/enroll | join | leave | revoke |
// decommission, GET|PUT|DELETE /api/v1/fleet/root; every one served on the
// local listener only). Each runs under fleetOpMu, like the create, because
// each reads the fleet state and then changes it.
//
//   - ENROLL (master): issue a leaf for (agent id, ssh-ed25519 key), commit it
//     and its issuance record on the master's agent branch, publish main, and
//     hand back the bundle document (pki.FleetBundle): the fleet repository
//     URL members use (fleetURLForBundle), then leaf + root + CRL.
//   - JOIN (member): check the bundle is for THIS instance's key and id,
//     install it, clone the fleet repository at the bundle's URL through the
//     fleet door, check its root is the bundle's, record it, write the own
//     record and push it ONCE.
//     Any refusal, the push's included, undoes all of it: no repository, no
//     installed material, no record, state standalone.
//   - LEAVE (member): write `left`, push once, then remove the fleet here
//     whatever the push did (works offline); the master's revoke is the
//     authoritative half.
//   - REVOKE (master): a new CRL from the CURRENT one on main plus the leaf's
//     serial, its revocation statement, committed and published; the CRL is
//     installed here at once.
//   - DECOMMISSION (master): remove the fleet here and delete <home>/fleet.
//   - ROOT (master switch): GET returns the files of <home>/fleet, PUT
//     installs them where there are none, DELETE stands down (deletes them,
//     keeps the membership), refused while this copy is the hub. One master at
//     a time is the user's job: nothing here checks the other side.

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

// FleetJoinSpec is what POST /fleet/join takes: the bundle (which names the
// fleet repository URL) and, for a forge-hosted fleet only, the joiner's own
// forge credential.
type FleetJoinSpec struct {
	AuthMethod, AuthToken string
	Bundle                []byte
}

// FleetLeft is what POST /fleet/leave answers: the state after the leave,
// and whether the `left` record reached the fleet.
type FleetLeft struct {
	FleetStatus
	Pushed    bool   `json:"pushed"`
	PushError string `json:"push_error,omitempty"`
}

// FleetRevocation is what POST /fleet/revoke answers.
type FleetRevocation struct {
	AgentID      string `json:"agent_id"`
	Serial       string `json:"serial"`
	CRLNumber    string `json:"crl_number"`
	Published    bool   `json:"published"`
	PublishError string `json:"publish_error,omitempty"`
	InstallError string `json:"install_error,omitempty"`
}

// FleetRoot is the master's root directory as GET /fleet/root returns it and
// PUT /fleet/root takes it: file name -> contents, for the names in
// fleetRootFiles only.
type FleetRoot struct {
	Dir             string            `json:"dir"`
	RootFingerprint string            `json:"root_fingerprint,omitempty"`
	Files           map[string]string `json:"files"`
}

// fleetRootFiles is every file a master's <home>/fleet holds: the key, its
// certificate, the CRL and the two local audit logs.
var fleetRootFiles = []string{pki.RootKeyFile, pki.RootCertFile, pki.CRLFile, pki.IssuedLogFile, pki.RevokedLogFile}

// masterFleet is the fleet repository when this instance is its master, else
// the refusal.
func (m *Manager) masterFleet() (*RepoInstance, error) {
	ri := m.fleetRepo()
	if ri == nil {
		return nil, fleetErr(http.StatusConflict, "not_registered", "this instance is standalone: no fleet repository is mounted")
	}
	master := false
	if err := ri.WithRead(func(svc *store.Service) { master = ri.isFleetMaster(svc) }); err != nil {
		return nil, err
	}
	if !master {
		return nil, fleetErr(http.StatusConflict, "not_master", "this instance is not the fleet's master: "+filepath.Join(m.fleetRootDir(), pki.RootKeyFile)+" is not here, or its root.crt is not the root on the fleet's main")
	}
	return ri, nil
}

// hasOrigin reports whether ri's copy here syncs with an origin.
func (m *Manager) hasOrigin(ri *RepoInstance) (bool, error) {
	o, err := m.originOf(ri)
	if err != nil {
		return false, err
	}
	return o != nil, nil
}

// publishMasterWrite publishes a commit the master just made on its agent
// branch: with an origin, push the agent branch and then main (pushFleet);
// without one (the hub), fast-forward main here.
func (m *Manager) publishMasterWrite(ctx context.Context, ri *RepoInstance) error {
	origin, err := m.hasOrigin(ri)
	if err != nil {
		return err
	}
	if origin {
		return m.pushFleet(ctx, ri)
	}
	return m.publishFleetMainNow(ctx, ri)
}

// refuseNoHubListener is what a copy of the fleet repository needs to be the
// hub (no origin): members push to it over the mTLS listener at the addresses
// its record advertises. Create without an origin and the master's detach of
// its origin both check it.
func (m *Manager) refuseNoHubListener() error {
	if m.deps.Cfg.TLS.Addr == "" {
		return fleetErr(http.StatusUnprocessableEntity, "listener_required", "a fleet hosted here needs the mTLS listener: set [tls].addr in knomit.toml (members push to it), then run `knomit reload`")
	}
	if len(m.deps.Cfg.ExternalAddresses) == 0 {
		return fleetErr(http.StatusUnprocessableEntity, "addresses_required", "a fleet hosted here needs external_addresses in knomit.toml: they are how members reach this instance")
	}
	return nil
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
	is, err := pki.IssueLeaf(root, pub, email, fleetLeafValidity, time.Now())
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
func refuseEnrolledID(p *store.FleetPKI, agentID string, pub ed25519.PublicKey) error {
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

// JoinFleet joins the fleet at the bundle's URL with the bundle the master's
// enroll returned (see the file comment). It answers registered: the record is on
// the fleet's origin, and counts once the master admits it into main. The
// answer's root_fingerprint is the root the bundle installed (§1.5 Join step
// 2: supplying the bundle is the operator's consent to that root).
func (m *Manager) JoinFleet(ctx context.Context, spec FleetJoinSpec) (FleetStatus, error) {
	defer m.lockFleetOp()()
	if err := m.recoverFleetCreate(ctx); err != nil {
		return FleetStatus{}, err
	}
	db := m.ControlDB()
	if db == nil {
		return FleetStatus{}, errors.New("fleet join: control.db is not open")
	}
	b, err := m.joinPreflight(db, spec.Bundle)
	if err != nil {
		return FleetStatus{}, err
	}
	if _, err := pki.InstallBundle(m.deps.Cfg.TLS.Dir, m.deps.KeyPath, spec.Bundle, pki.InstallOptions{}); err != nil {
		return FleetStatus{}, fmt.Errorf("fleet join: install the bundle: %w", err)
	}
	refuse := func(ri *RepoInstance, ferr *FleetError) (FleetStatus, error) {
		if uerr := m.undoJoin(context.WithoutCancel(ctx), ri); uerr != nil {
			ferr.Msg += "; undoing the join failed (" + uerr.Error() + ")"
		}
		_ = recordFleetAttempt(db, ferr, time.Now())
		return FleetStatus{}, ferr
	}
	// The clone decides the transport (ResolveOrigin) against the trust just
	// installed: the hub's listener chains to the bundle's root -> cert; a
	// forge is not a peer and takes the joiner's own credential.
	ri, err := m.Create(ctx, CreateSpec{Name: m.freeFleetName(b.FleetURL), Mode: "clone",
		Origin: &OriginSpec{URL: b.FleetURL, AuthMethod: spec.AuthMethod, AuthToken: spec.AuthToken}, fleetDoor: true}, func(Event) {})
	if errors.Is(err, ErrPeerCredential) {
		return refuse(nil, fleetErr(http.StatusUnprocessableEntity, "auth_invalid", err.Error()))
	}
	// On the clone path ErrOriginUnreachable comes only from ResolveOrigin's
	// classification, which join:check makes too: the same cause answers the
	// same code on both routes (joinReachErr).
	if errors.Is(err, ErrOriginUnreachable) {
		return refuse(nil, joinReachErr(originHost(b.FleetURL), err))
	}
	if err != nil {
		return refuse(nil, fleetErr(http.StatusBadGateway, "clone_failed", err.Error()))
	}
	if !fact.IsFleetOntology(ri.Ontology()) {
		return refuse(ri, fleetErr(http.StatusUnprocessableEntity, "not_a_fleet", "the repository's ontology is not the fleet ontology"))
	}
	if f, ferr := m.fleetAtMain(ri); ferr != nil || f.PKI == nil || f.PKI.Root == nil || !f.PKI.Root.Equal(b.Root) {
		return refuse(ri, fleetErr(http.StatusUnprocessableEntity, "root_differs", "the fleet repository's root (artifacts/fleet/root.crt on its main) is not the bundle's root: the bundle is for another fleet"))
	}
	if err := recordFleetRepo(db, ri.UID()); err != nil {
		return refuse(ri, fleetErr(http.StatusInternalServerError, "record_failed", err.Error()))
	}
	if err := m.writeOwnRecord(ctx, ri, fact.MemberActive); err != nil {
		return refuse(ri, fleetErr(http.StatusInternalServerError, "record_failed", err.Error()))
	}
	if err := m.pushAgentBranch(ctx, ri); err != nil {
		return refuse(ri, fleetErr(http.StatusConflict, "join_refused", "the fleet did not take this instance's record: "+err.Error()+". Nothing is kept; join again (a leaf the master issued just now reaches the fleet's main on its next round)"))
	}
	if err := recordFleetAttempt(db, nil, time.Now()); err != nil {
		return FleetStatus{}, err
	}
	if err := setFleetState(db, FleetRegistered, time.Now()); err != nil {
		return FleetStatus{}, err
	}
	log.Info().Str("fleet", ri.Name()).Msg("fleet: joined; the record counts once the master admits it into main")
	return m.FleetStatus(ctx)
}

// joinPreflight is every refusal a join (and its check) makes before
// anything is installed: this instance is in no fleet (fleet_present) and
// holds no trust material (trust_installed); the bundle is a bundle document
// naming the fleet URL (bundle_invalid), for this instance's key
// (key_mismatch) and agent id (agent_mismatch).
func (m *Manager) joinPreflight(db *sql.DB, raw []byte) (pki.Bundle, error) {
	if err := m.refuseFleetPresent(db); err != nil {
		return pki.Bundle{}, err
	}
	if err := m.refuseInstalledTrust(); err != nil {
		return pki.Bundle{}, err
	}
	b, err := pki.SplitBundle(raw)
	if err != nil {
		return pki.Bundle{}, fleetErr(http.StatusUnprocessableEntity, "bundle_invalid", err.Error())
	}
	if b.FleetURL == "" {
		return pki.Bundle{}, fleetErr(http.StatusUnprocessableEntity, "bundle_invalid", "the bundle names no fleet repository URL: enroll this instance again on the master")
	}
	if why := bundleURLProblem(b.FleetURL); why != "" {
		return pki.Bundle{}, fleetErr(http.StatusUnprocessableEntity, "bundle_invalid", "the bundle's fleet repository URL "+credentialFree(b.FleetURL)+" "+why+": enroll this instance again on the master")
	}
	if _, err := pki.PreviewBundle(m.deps.KeyPath, raw); err != nil {
		if errors.Is(err, pki.ErrKeyMismatch) {
			return pki.Bundle{}, fleetErr(http.StatusUnprocessableEntity, "key_mismatch", err.Error())
		}
		return pki.Bundle{}, fleetErr(http.StatusUnprocessableEntity, "bundle_invalid", err.Error())
	}
	var leafID string
	if len(b.Leaf.EmailAddresses) == 1 {
		leafID = pki.AgentIDOfEmail(b.Leaf.EmailAddresses[0])
	}
	if leafID != m.AgentID() {
		return pki.Bundle{}, fleetErr(http.StatusUnprocessableEntity, "agent_mismatch", fmt.Sprintf("the bundle's certificate is for agent %q, not this instance's %q", leafID, m.AgentID()))
	}
	return b, nil
}

// bundleURLProblem is why a bundle's url: line is not a fleet repository URL
// a join can clone, or "": the line is unauthenticated (SplitBundle checks
// only the agent, the root and the serial), so the join checks its shape
// before anything dials it, and join and join:check refuse a bad one with
// the same code (bundle_invalid; PR #527 N1). A master's enroll writes one of
// two shapes: a hub's plain https://<address>/git/<repo> (an https URL with a
// host, never the internal fleet scheme pki.GitURL derives), or a forge-hosted fleet's
// origin URL, which the origin routes accept as https, ssh:// or scp-style
// git@host:path, or file:// under a local origin root. Anything else (another
// scheme, plain http, a URL with no host) is refused.
func bundleURLProblem(u string) string {
	if !strings.Contains(u, "://") {
		if at, colon := strings.Index(u, "@"), strings.Index(u, ":"); at > 0 && colon > at+1 && colon < len(u)-1 {
			return "" // scp-style git@host:path, a forge's ssh origin
		}
		return "is not a URL"
	}
	p, err := url.Parse(u)
	if err != nil {
		return "is not a URL"
	}
	switch p.Scheme {
	case "https", "ssh":
		if p.Hostname() == "" {
			return "names no host"
		}
		return ""
	case "file":
		if p.Path == "" {
			return "names no path"
		}
		return ""
	}
	return "is not https:// (a hub's) or a forge origin's (https://, ssh://, git@host:path, file://)"
}

// refuseFleetOrigin refuses an origin for the fleet repository that no
// member's join could clone (#527 F1): the bundle carries the master's origin
// URL, and a join refuses every shape bundleURLProblem refuses (plain http,
// another scheme, no host). Asked where the choice is made, a forge create
// and an attach to the fleet repository, so the operator is not sent to
// re-enroll joiners for a scheme the master's own origin has.
func refuseFleetOrigin(u string) error {
	if why := bundleURLProblem(u); why != "" {
		return fleetErr(http.StatusUnprocessableEntity, "origin_invalid",
			"the fleet repository's origin "+credentialFree(u)+" "+why+
				": members join only an https://, ssh://, git@host:path or file:// origin, so a fleet cannot live there")
	}
	return nil
}

// The two FleetJoinCheck.Hosting values.
const (
	HostingMaster = "master" // a role, not a branch (consensus_branch_literals_test allows this name)
	HostingForge  = "forge"
)

// FleetJoinCheck is what POST /fleet/join:check answers: where the bundle's
// fleet lives and the root the operator confirms BEFORE the join installs it.
type FleetJoinCheck struct {
	URL string `json:"url"`
	// Hosting is "master" (the URL is an instance of the bundle's fleet,
	// reached over the fleet transport with the bundle's certificate) or
	// "forge" (anything else: the joiner's own credential is used).
	Hosting         string `json:"hosting"`
	Host            string `json:"host"`
	RootFingerprint string `json:"root_fingerprint"`
	AgentID         string `json:"agent_id"`
	// AuthRequired: a forge refused the credential given (or none).
	AuthRequired bool   `json:"auth_required"`
	Detail       string `json:"detail,omitempty"`
}

// CheckFleetJoin is the join's check (POST /fleet/join:check): the join's
// pre-flights, then the bundle's URL is reached presenting the BUNDLE's
// certificate and judged against the BUNDLE's root, without installing
// anything (the material is written to a private temporary directory that is
// removed before returning). An instance of that fleet is hosting "master"
// (a credential for it is auth_invalid); anything else is a forge, probed
// with the joiner's credential (cert as input is auth_invalid). A host that
// refuses the bundle, or whose certificate this bundle refuses, is
// bundle_refused (422); any other failure to classify is unreachable (502),
// as the join says for the same cause (joinReachErr). It writes nothing.
func (m *Manager) CheckFleetJoin(ctx context.Context, spec FleetJoinSpec) (FleetJoinCheck, error) {
	defer m.lockFleetOp()()
	db := m.ControlDB()
	if db == nil {
		return FleetJoinCheck{}, errors.New("fleet join check: control.db is not open")
	}
	b, err := m.joinPreflight(db, spec.Bundle)
	if err != nil {
		return FleetJoinCheck{}, err
	}
	if spec.AuthMethod == "cert" {
		return FleetJoinCheck{}, fleetErr(http.StatusUnprocessableEntity, "auth_invalid", ErrCertNotAnInput.Error())
	}
	agent := pki.AgentIDOfEmail(b.Leaf.EmailAddresses[0]) // joinPreflight checked it is this instance's
	rootID, _ := pki.RootID(b.Root)
	out := FleetJoinCheck{URL: b.FleetURL, Host: originHost(b.FleetURL), RootFingerprint: rootID, AgentID: agent}

	tmp, err := os.MkdirTemp("", "knomit-join-check-")
	if err != nil {
		return FleetJoinCheck{}, err
	}
	defer os.RemoveAll(tmp)
	for name, body := range map[string][]byte{pki.InstanceCertFile: certPEMOf(b.Leaf), pki.RootCertFile: b.RootPEM, pki.CRLFile: b.CRLPEM} {
		if err := os.WriteFile(filepath.Join(tmp, name), body, 0o600); err != nil {
			return FleetJoinCheck{}, err
		}
	}
	cctx := ctx
	if d := m.deps.Cfg.Git.NetworkTimeout; d > 0 {
		var cancel context.CancelFunc
		cctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	peer, err := pki.ClassifyOrigin(cctx, tmp, m.deps.KeyPath, b.FleetURL)
	if err != nil {
		return FleetJoinCheck{}, joinReachErr(out.Host, err)
	}
	if peer {
		if spec.AuthMethod != "" && spec.AuthMethod != "none" || spec.AuthToken != "" {
			return FleetJoinCheck{}, fleetErr(http.StatusUnprocessableEntity, "auth_invalid", ErrPeerCredential.Error())
		}
		out.Hosting = HostingMaster
		return out, nil
	}
	out.Hosting = HostingForge
	// Not marked decided: ResolveOrigin judges the joiner's own auth like any
	// probe's (trust_installed guarantees this instance holds no fleet root,
	// so it dials nothing here).
	res, err := m.ProbeOriginRefs(ctx, OriginSpec{URL: b.FleetURL, AuthMethod: spec.AuthMethod, AuthToken: spec.AuthToken})
	if err != nil {
		return FleetJoinCheck{}, err
	}
	switch {
	case res.AuthRequired:
		out.AuthRequired, out.Detail = true, res.Detail
	case !res.Reachable:
		return FleetJoinCheck{}, fleetErr(http.StatusBadGateway, "unreachable", "couldn't reach "+out.Host+": "+res.Detail)
	}
	return out, nil
}

// joinReachErr names a failed classification of the bundle's fleet URL, for
// join and join:check alike. Only a genuine refusal is bundle_refused (422):
// the host refused this bundle's certificate (pki.ErrRefusedByPeer: typically
// a bundle revoked since it was issued), or our verifier refused the
// certificate the host presents as one of this fleet's
// (pki.ErrOriginCertRefused). Everything else is unreachable (502): no TCP
// connection, a timeout or a reset after the handshake, this instance's own
// trust material unusable. Those are no verdict on the bundle, and "ask for a
// new bundle" would send the operator the wrong way.
//
// The text is the classification's own: the join's ResolveOrigin wrap
// (ErrOriginUnreachable, "origin was not attached") is dropped (reachCause),
// so both routes say the same words for the same cause.
func joinReachErr(host string, err error) *FleetError {
	cause := reachCause(err).Error()
	if errors.Is(err, pki.ErrRefusedByPeer) || errors.Is(err, pki.ErrOriginCertRefused) {
		return fleetErr(http.StatusUnprocessableEntity, "bundle_refused", "the fleet at "+host+" refused this bundle ("+cause+"): ask the fleet's master for a new bundle")
	}
	return fleetErr(http.StatusBadGateway, "unreachable", "couldn't reach "+host+": "+cause)
}

// reachCause is err without ResolveOrigin's wrap (fmt.Errorf("%w: %w",
// ErrOriginUnreachable, cause)): the cause. Any other error is itself.
func reachCause(err error) error {
	m, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return err
	}
	var rest []error
	wrapped := false
	for _, e := range m.Unwrap() {
		if e == ErrOriginUnreachable {
			wrapped = true
			continue
		}
		rest = append(rest, e)
	}
	if !wrapped || len(rest) != 1 {
		return err
	}
	return rest[0]
}

// originHost is the host an origin URL names: host[:port] of a URL, the
// host of an scp-style git@host:path, "" for a local path.
func originHost(u string) string {
	if strings.Contains(u, "://") {
		if p, err := url.Parse(u); err == nil {
			return p.Host
		}
		return ""
	}
	if at, colon := strings.Index(u, "@"), strings.Index(u, ":"); at >= 0 && colon > at {
		return u[at+1 : colon]
	}
	return ""
}

// undoJoin takes a refused join back: the agent branch the mount may have
// pushed to a forge (with the lease, as the create's rollback does), the
// repository, the installed material. Every step runs; the errors of the
// last two are joined. The retract is best effort: a knomit host refuses
// every delete, and what the mount pushed there is main's tip, no record.
func (m *Manager) undoJoin(ctx context.Context, ri *RepoInstance) error {
	var errs []error
	if ri != nil {
		if err := m.retractFleetBranch(ctx, ri); err != nil {
			log.Info().Err(err).Str("fleet", ri.Name()).Msg("fleet join refused: the agent branch on the origin is left (it holds no record)")
		}
		if err := m.deleteRepo(ri.Name(), "fleet join refused"); err != nil {
			errs = append(errs, err)
		}
	}
	if err := pki.Uninstall(m.deps.Cfg.TLS.Dir); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// LeaveFleet leaves the fleet (member): `left` on the agent branch, ONE push
// attempt, then the fleet is removed here whatever the push did: the
// repository (dropped from every lens first), the installed material, state
// standalone. The master is refused: it decommissions, or hands the root on
// and stands down first.
func (m *Manager) LeaveFleet(ctx context.Context) (FleetLeft, error) {
	defer m.lockFleetOp()()
	row, err := m.fleetDB()
	if err != nil {
		return FleetLeft{}, err
	}
	ri := m.fleetRepo()
	if ri == nil && row.State == FleetStandalone {
		return FleetLeft{}, fleetErr(http.StatusConflict, "not_registered", "this instance is standalone")
	}
	var out FleetLeft
	if ri != nil {
		master := false
		if err := ri.WithRead(func(svc *store.Service) { master = ri.isFleetMaster(svc) }); err != nil {
			return FleetLeft{}, err
		}
		if master {
			return FleetLeft{}, fleetErr(http.StatusConflict, "is_master", "this instance is the fleet's master: decommission the fleet (Fleet → Settings → Decommission), or first hand the root key to another member (Fleet → Settings → Export root key here, Import root key there) and stand down (Fleet → Settings → Stand down)")
		}
		if err := m.writeOwnRecord(ctx, ri, fact.MemberLeft); err != nil {
			out.PushError = "the left record was not written: " + err.Error()
		} else if err := m.pushAgentBranch(ctx, ri); err != nil {
			out.PushError = err.Error()
		} else {
			out.Pushed = true
		}
		if err := m.removeFleetHere(ri, "fleet left"); err != nil {
			return FleetLeft{}, err
		}
	}
	if err := pki.Uninstall(m.deps.Cfg.TLS.Dir); err != nil {
		return FleetLeft{}, fmt.Errorf("fleet leave: the repository is removed, uninstalling the certificate failed: %w", err)
	}
	if err := setFleetState(m.ControlDB(), FleetStandalone, time.Now()); err != nil {
		return FleetLeft{}, err
	}
	log.Info().Bool("pushed", out.Pushed).Msg("fleet: left; the master revokes the leaf (a left record on main, or by hand)")
	out.FleetStatus, err = m.FleetStatus(ctx)
	return out, err
}

// removeFleetHere deletes the fleet repository here after dropping it from
// every lens that reads it (a lens may hold it as a read mount only).
func (m *Manager) removeFleetHere(ri *RepoInstance, reason string) error {
	if err := m.dropFromLenses(ri.UID()); err != nil {
		return err
	}
	return m.deleteRepo(ri.Name(), reason)
}

// dropFromLenses removes the read mount of repo uid from every lens. Under
// m.mu, as UpdateLens persists.
func (m *Manager) dropFromLenses(uid string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.registry == nil {
		return nil
	}
	names, err := m.registry.RefsRepo(uid)
	if err != nil {
		return err
	}
	for _, name := range names {
		l, ok, err := m.registry.Get(name)
		if err != nil {
			return err
		}
		if !ok || l.WriteUID == uid {
			continue
		}
		reads := make([]LensRead, 0, len(l.Reads))
		for _, r := range l.Reads {
			if r.RepoUID != uid {
				reads = append(reads, r)
			}
		}
		l.Reads, l.UpdatedAt = reads, time.Now().Unix()
		if _, err := m.registry.Update(l); err != nil {
			return fmt.Errorf("drop the fleet repository from lens %q: %w", name, err)
		}
		log.Info().Str("lens", name).Msg("fleet: the fleet repository is dropped from the lens")
	}
	return nil
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
	now := time.Now()
	rv, err := pki.RevokeSerial(root, f.PKI.CRL, pki.Revoked{Serial: leaf.SerialNumber, At: now, Reason: reason}, now, now.Add(fleetCRLValidity))
	switch {
	case errors.Is(err, pki.ErrAlreadyRevoked):
		return FleetRevocation{}, fleetErr(http.StatusConflict, "already_revoked", "agent "+agentID+"'s certificate is already on the CRL")
	case err != nil:
		return FleetRevocation{}, fleetErr(http.StatusUnprocessableEntity, "revoke_failed", err.Error())
	}
	ont := ri.Ontology()
	layout, _ := ont.PKILayout()
	statements, err := artifactPattern(ont, fact.PKIRevocation)
	if err != nil {
		return FleetRevocation{}, err
	}
	serial := leaf.SerialNumber.Text(16)
	if err := m.writeFleetArtifacts(ctx, ri, map[string]string{
		layout.CRL: string(rv.CRLPEM),
		strings.Replace(statements, "*", serial, 1): string(rv.Statement),
	}, "fleet: revoke "+agentID+" (serial "+serial+")"); err != nil {
		return FleetRevocation{}, err
	}
	out := FleetRevocation{AgentID: agentID, Serial: serial, CRLNumber: rv.Number.String(), Published: true}
	// The master's own copies: its local audit log and CRL (what GET
	// /fleet/root hands a new master), and its installed CRL.
	if err := pki.RecordRevocation(m.fleetRootDir(), rv); err != nil {
		log.Warn().Err(err).Msg("fleet revoke: the local revocation log or CRL copy was not written")
	}
	if _, err := pki.InstallCRL(m.deps.Cfg.TLS.Dir, root.Cert, rv.CRLPEM); err != nil {
		out.InstallError = err.Error()
	}
	if perr := m.publishMasterWrite(ctx, ri); perr != nil {
		out.Published, out.PublishError = false, perr.Error()
	}
	log.Info().Str("agent", agentID).Str("serial", serial).Msg("fleet: member revoked")
	return out, nil
}

// DecommissionFleet ends the fleet (master only): the fleet repository here
// (dropped from every lens first; a forge keeps its copy, inert), the
// installed material and <home>/fleet, root key included. State standalone.
// Members' syncs then fail, and each leaves (which works offline).
func (m *Manager) DecommissionFleet(ctx context.Context) (FleetStatus, error) {
	defer m.lockFleetOp()()
	ri, err := m.masterFleet()
	if err != nil {
		return FleetStatus{}, err
	}
	if err := m.removeFleetHere(ri, "fleet decommissioned"); err != nil {
		return FleetStatus{}, err
	}
	if err := pki.Uninstall(m.deps.Cfg.TLS.Dir); err != nil {
		return FleetStatus{}, fmt.Errorf("fleet decommission: the repository is removed, uninstalling the certificate failed: %w", err)
	}
	if err := os.RemoveAll(m.fleetRootDir()); err != nil {
		return FleetStatus{}, fmt.Errorf("fleet decommission: the repository is removed, deleting %s failed: %w", m.fleetRootDir(), err)
	}
	if err := setFleetState(m.ControlDB(), FleetStandalone, time.Now()); err != nil {
		return FleetStatus{}, err
	}
	log.Info().Msg("fleet: decommissioned; the root key is deleted")
	return m.FleetStatus(ctx)
}

// rootKeyHere reports whether <home>/fleet/root.key exists.
func (m *Manager) rootKeyHere() (bool, error) {
	_, err := os.Lstat(filepath.Join(m.fleetRootDir(), pki.RootKeyFile))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	}
	return false, err
}

// FleetRootFiles returns the files of <home>/fleet (GET /fleet/root): what a
// master switch copies to the new master.
func (m *Manager) FleetRootFiles() (FleetRoot, error) {
	defer m.lockFleetOp()()
	dir := m.fleetRootDir()
	if ok, err := m.rootKeyHere(); err != nil {
		return FleetRoot{}, err
	} else if !ok {
		return FleetRoot{}, fleetErr(http.StatusNotFound, "no_root", "this instance holds no fleet root ("+filepath.Join(dir, pki.RootKeyFile)+" is absent)")
	}
	out := FleetRoot{Dir: dir, Files: map[string]string{}}
	for _, name := range fleetRootFiles {
		p := filepath.Join(dir, name)
		st, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return FleetRoot{}, err
		}
		if !st.Mode().IsRegular() {
			return FleetRoot{}, fmt.Errorf("fleet root: %s is not a regular file", p)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return FleetRoot{}, err
		}
		out.Files[name] = string(b)
	}
	if c, err := pki.LoadRootCert(filepath.Join(dir, pki.RootCertFile)); err == nil {
		out.RootFingerprint, _ = pki.RootID(c)
	}
	return out, nil
}

// InstallFleetRoot installs the files of another master's <home>/fleet here
// (PUT /fleet/root), refused while a root key is here. The files land in a
// private staging directory, the key and certificate are checked to be one
// root (pki.LoadRoot), then the directory is moved into place.
func (m *Manager) InstallFleetRoot(files map[string]string) (FleetRoot, error) {
	defer m.lockFleetOp()()
	dir := m.fleetRootDir()
	present := func() error {
		return fleetErr(http.StatusConflict, "root_present", "a fleet root is already here ("+dir+"): stand down or decommission first")
	}
	// Anything in <home>/fleet, the root key first of all, refuses: the
	// files land by a rename, which never replaces a directory with content.
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return FleetRoot{}, present()
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return FleetRoot{}, err
	}
	known := map[string]bool{}
	for _, n := range fleetRootFiles {
		known[n] = true
	}
	for name := range files {
		if !known[name] {
			return FleetRoot{}, fleetErr(http.StatusUnprocessableEntity, "unknown_file", fmt.Sprintf("%q is not a fleet root file (want any of %s)", name, strings.Join(fleetRootFiles, ", ")))
		}
	}
	if files[pki.RootKeyFile] == "" || files[pki.RootCertFile] == "" {
		return FleetRoot{}, fleetErr(http.StatusUnprocessableEntity, "root_incomplete", pki.RootKeyFile+" and "+pki.RootCertFile+" are required")
	}
	staging := dir + ".receiving"
	if err := os.RemoveAll(staging); err != nil {
		return FleetRoot{}, err
	}
	if err := privdir.Ensure(staging); err != nil {
		return FleetRoot{}, err
	}
	if err := privdir.Verify(staging); err != nil {
		_ = os.RemoveAll(staging)
		return FleetRoot{}, fmt.Errorf("fleet root: refusing to write the root key: %w", err)
	}
	fail := func(err error) (FleetRoot, error) {
		_ = os.RemoveAll(staging)
		return FleetRoot{}, err
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(staging, name), []byte(body), 0o600); err != nil {
			return fail(err)
		}
	}
	root, err := pki.LoadRoot(staging)
	if err != nil {
		return fail(fleetErr(http.StatusUnprocessableEntity, "root_invalid", err.Error()))
	}
	if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fail(err)
	}
	if err := os.Rename(staging, dir); err != nil {
		return fail(err)
	}
	id, _ := pki.RootID(root.Cert)
	log.Info().Str("dir", dir).Str("root", id).Msg("fleet: root installed; this instance is the master once that root is on the fleet's main")
	return FleetRoot{Dir: dir, RootFingerprint: id, Files: map[string]string{}}, nil
}

// StandDown deletes <home>/fleet (DELETE /fleet/root) and keeps the
// membership: the user's way to remove the key from the old master after a
// switch. Refused while this copy of the fleet repository is the hub (no
// origin): it would leave the hub without a master and the members without a
// host.
func (m *Manager) StandDown(ctx context.Context) error {
	defer m.lockFleetOp()()
	dir := m.fleetRootDir()
	if ok, err := m.rootKeyHere(); err != nil {
		return err
	} else if !ok {
		return fleetErr(http.StatusNotFound, "no_root", "this instance holds no fleet root ("+filepath.Join(dir, pki.RootKeyFile)+" is absent)")
	}
	if ri := m.fleetRepo(); ri != nil {
		origin, err := m.hasOrigin(ri)
		if err != nil {
			return err
		}
		if !origin {
			return fleetErr(http.StatusConflict, "hub", "this instance hosts the fleet (its copy of "+ri.Name()+" has no origin): host the fleet on the new master and point this copy at it first (Fleet → Settings → Point at the new host), or the fleet has no master and no host")
		}
		if err := m.refuseNotFollowingHost(ri); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	log.Info().Str("dir", dir).Msg("fleet: stood down as master; the root files are deleted here")
	return nil
}

// refuseNotFollowingHost is the stand-down's check that this copy follows
// the fleet's host (#528 N4): its origin must be one of the URLs the host
// record on its main names (GET /fleet's follows_host). A copy that follows
// a peer which does not host the fleet (a hub pointed at a plain member) is
// refused: standing down there leaves the fleet with no master at its host.
// A main with no host record (a fleet that predates it) decides nothing.
func (m *Manager) refuseNotFollowingHost(ri *RepoInstance) error {
	o, err := m.originOf(ri)
	if err != nil || o == nil {
		return err
	}
	hosted, err := m.fleetHostAtMain(ri)
	if err != nil || len(hosted) == 0 {
		return err
	}
	for _, u := range hosted {
		if sameFleetURL(o.URL, u) {
			return nil
		}
	}
	return fleetErr(http.StatusConflict, "not_following_host",
		"this copy follows "+credentialFree(o.URL)+", but the fleet's main says it is hosted at "+strings.Join(credentialFreeAll(hosted), ", ")+
			": standing down now would leave the fleet without a master where it is hosted. Point this copy at the fleet's host (Fleet → Settings → Point at the new host); right after pointing it, wait for its first sync, then stand down")
}

// FleetIndexRebuild is what POST /fleet/index-rebuild answers: the index job
// rebuilding the fleet repository's agent branch (Absorbed: an identical
// rebuild already ran, and this is it).
type FleetIndexRebuild struct {
	JobID    string `json:"id"`
	Repo     string `json:"repo"`
	Branch   string `json:"branch"`
	Absorbed bool   `json:"absorbed"`
}

// RebuildFleetIndex is POST /fleet/index-rebuild (user ruling 2026-10-16):
// the repository Settings' Rebuild for the fleet repository, whose generic
// route (POST /repos/{repo}/branches/{b}/index-rebuilds) is refused like
// every generic mutation of it. The same lifecycle event (Rebuild) on the
// agent branch, the one the generic Rebuild names; its progress reads through
// the generic job routes. Under fleetOpMu like every fleet operation.
func (m *Manager) RebuildFleetIndex(ctx context.Context) (FleetIndexRebuild, error) {
	defer m.lockFleetOp()()
	ri := m.fleetRepo()
	if ri == nil {
		return FleetIndexRebuild{}, fleetErr(http.StatusConflict, "not_registered", "this instance is in no fleet")
	}
	branch := ri.AgentBranch()
	rep, err := m.Send(ctx, ri, Rebuild(branch))
	if err != nil {
		return FleetIndexRebuild{}, err
	}
	return FleetIndexRebuild{JobID: rep.JobID, Repo: ri.Name(), Branch: branch, Absorbed: rep.Absorbed}, nil
}

// HostFleetHere is POST /fleet/host, "Host the fleet here": this instance's
// copy of the fleet repository drops its origin and becomes the hub. The
// check (guardFleetDetach) and the detach run under fleetOpMu as one step, so
// a stand-down (DELETE /fleet/root, which passes while the copy has an
// origin) cannot remove the key between the master check and the detach and
// leave a hub with no master.
//
// A detach that succeeded makes this master the host, so it records that on
// main: the host record (fleet_host.go) with the hub's URLs, committed on the
// agent branch and published (main fast-forwards here: there is no origin
// any more; a fast-forward that fails now is the local reconcile's to retry).
// The detach stands when the write fails, and it is answered as done: the
// hub's local reconcile re-asserts the record every round
// (RepoInstance.fleetHostRound), so the next round writes it.
func (m *Manager) HostFleetHere(ctx context.Context) (FleetStatus, error) {
	if err := m.hostFleetHere(ctx); err != nil {
		return FleetStatus{}, err
	}
	return m.FleetStatus(ctx)
}

func (m *Manager) hostFleetHere(ctx context.Context) error {
	defer m.lockFleetOp()()
	ri := m.fleetRepo()
	if ri == nil {
		return fleetErr(http.StatusConflict, "not_registered", "this instance is in no fleet")
	}
	if origin, err := m.hasOrigin(ri); err != nil {
		return err
	} else if !origin {
		return fleetErr(http.StatusConflict, "hub", "this instance already hosts the fleet: its copy of "+ri.Name()+" has no origin")
	}
	if err := m.guardFleetDetach(ri); err != nil {
		return err
	}
	if _, err := m.Send(ctx, ri, DetachOrigin()); err != nil {
		return err
	}
	if err := m.writeFleetHost(ctx, ri, m.hubFleetURLs(ri)); err != nil {
		log.Warn().Err(err).Str("fleet", ri.Name()).
			Msg("fleet: this instance hosts the fleet now, but the host record was not written; the local reconcile's next round writes it")
		return nil
	}
	if err := m.publishFleetMainNow(ctx, ri); err != nil {
		log.Warn().Err(err).Str("fleet", ri.Name()).Msg("fleet: main did not follow the host record yet; the local reconcile retries")
	}
	return nil
}

// FollowFleetHost is POST /fleet/follow: this instance's copy of the fleet
// repository follows the fleet from url, or changes its credential for the
// forge it follows. It is the one way an origin is attached to the fleet
// repository, under fleetOpMu like every fleet operation, and it runs the
// attach's own checks (guardAttach): an origin no member's join could clone
// is origin_invalid (#527 F1), an unreachable one is refused, and the
// remote's ontology must be the fleet's.
//
// What it does is decided by the URL against the stored origin (PR #529
// S3), never by which page asked:
//
//   - the stored forge, or no url, on a forge copy: a CREDENTIAL change. The
//     new credential is probed and stored, and that is all: no push, no host
//     record, nothing a member sees. The stored credential unchanged is
//     answered FleetFollowUnchanged and nothing runs;
//   - an instance of THIS fleet (its certificate chains to this fleet's
//     root): this copy follows it (a master switch). A credential is refused
//     (ErrPeerCredential): the certificate is the credential;
//   - another forge URL: followForge. The master MOVES the fleet there; a
//     member follows the forge the fleet moved to.
//
// Every error text it answers is credential-free (credentialFreeErr): a
// token in a URL's user slot is quoted by the HTTP client and by go-git.
func (m *Manager) FollowFleetHost(ctx context.Context, o OriginSpec) (FleetFollowed, error) {
	change, err := m.followFleetHost(ctx, o)
	if err != nil {
		return FleetFollowed{}, credentialFreeErr(err)
	}
	st, err := m.FleetStatus(ctx)
	return FleetFollowed{FleetStatus: st, Change: change}, err
}

// FleetFollowed is what POST /fleet/follow answers: the new state and what
// the follow changed.
type FleetFollowed struct {
	FleetStatus
	Change string `json:"change"`
}

// The FleetFollowed.Change values.
const (
	FleetFollowFollowed   = "followed"   // this copy follows the fleet from the URL now
	FleetFollowMoved      = "moved"      // the master moved the fleet onto the forge
	FleetFollowCredential = "credential" // the same forge with a new credential
	FleetFollowUnchanged  = "unchanged"  // the stored forge and credential: nothing ran
)

func (m *Manager) followFleetHost(ctx context.Context, o OriginSpec) (string, error) {
	defer m.lockFleetOp()()
	ri := m.fleetRepo()
	if ri == nil {
		return "", fleetErr(http.StatusConflict, "not_registered", "this instance is in no fleet")
	}
	prev, err := m.originOf(ri)
	if err != nil {
		return "", err
	}
	forgeCopy := prev != nil && prev.AuthMethod != "cert"
	o.URL = strings.TrimSpace(o.URL)
	if o.URL == "" {
		// A credential change on a forge copy: the stored URL stays.
		if !forgeCopy {
			return "", fleetErr(http.StatusBadRequest, "url_required", "the body must carry the URL this copy follows the fleet from")
		}
		o.URL = prev.URL
	}
	if err := refuseFleetOrigin(o.URL); err != nil {
		return "", err
	}
	if forgeCopy && sameFleetURL(prev.URL, o.URL) {
		return m.changeFleetCredential(ctx, ri, o, prev)
	}
	decided, err := m.ResolveOrigin(ctx, o)
	if err != nil {
		return "", err
	}
	if decided.AuthMethod != "cert" {
		return m.followForge(ctx, ri, decided, prev)
	}
	if err := ri.WithRead(func(svc *store.Service) { decided.Branch = svc.UpstreamBranch() }); err != nil {
		return "", err
	}
	if _, err = m.Send(ctx, ri, AttachOrigin(decided)); err != nil {
		return "", err
	}
	return FleetFollowFollowed, nil
}

// changeFleetCredential is a follow of the forge this copy already follows
// ("Update access", on the master and on a member alike). The URL stays the
// stored one. A credential not given keeps the stored one: no method, or
// the stored method with no token (the page prefills the method and never
// holds the secret, #529 N-b); auth_method none is the explicit reset to
// anonymous. The new credential must reach the forge (the probe the attach's
// guard asks for) before it is stored. Then it is stored, and that is all: on
// the master no seed, no host record, no publish (#529 S3, N-a).
func (m *Manager) changeFleetCredential(ctx context.Context, ri *RepoInstance, o OriginSpec, prev *Origin) (string, error) {
	o.URL = prev.URL
	if o.AuthToken == "" && (o.AuthMethod == "" || o.AuthMethod == prev.AuthMethod) {
		o.AuthMethod, o.AuthToken = prev.AuthMethod, prev.AuthToken
	}
	if err := refuseMethodForURL(o.URL, o.AuthMethod); err != nil {
		return "", err
	}
	if o.AuthMethod == prev.AuthMethod && o.AuthToken == prev.AuthToken {
		return FleetFollowUnchanged, nil
	}
	decided, err := m.ResolveOrigin(ctx, o)
	if err != nil {
		return "", err
	}
	if decided.AuthMethod == "cert" {
		return "", fleetErr(http.StatusConflict, "not_this_fleet",
			credentialFree(o.URL)+" now answers as an instance of this fleet, not as a forge, and takes no credential. Point this copy at it (Fleet → Settings → Point at the new host)")
	}
	if err := ri.WithRead(func(svc *store.Service) { decided.Branch = svc.UpstreamBranch() }); err != nil {
		return "", err
	}
	if err := m.probeFleetForge(ctx, decided); err != nil {
		return "", err
	}
	if _, err := m.Send(ctx, ri, AttachOrigin(decided)); err != nil {
		return "", err
	}
	log.Info().Str("fleet", ri.Name()).Str("forge", credentialFree(o.URL)).Str("auth_method", decided.AuthMethod).
		Msg("fleet: this copy's credential for the forge changed")
	return FleetFollowCredential, nil
}

// refuseMethodForURL is validateURLAuth's scheme check for a follow whose
// URL is the stored one (no url in the body, so the REST edge had nothing to
// check the method against, #529 N-d): an ssh method on an http(s) URL, or
// a token or basic credential on an ssh URL, is 422 auth_invalid.
func refuseMethodForURL(u, method string) error {
	isSSH := strings.HasPrefix(u, "git@") || strings.HasPrefix(u, "ssh://")
	isHTTP := strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")
	switch {
	case isHTTP && method == "ssh":
		return fleetErr(http.StatusUnprocessableEntity, "auth_invalid", "SSH auth cannot be used with an https forge URL: use a token or basic auth instead")
	case isSSH && (method == "token" || method == "basic"):
		return fleetErr(http.StatusUnprocessableEntity, "auth_invalid", "token or basic auth cannot be used with an SSH forge URL: use SSH auth instead")
	}
	return nil
}

// guardFleetDetach is the fleet repository's detach check (R3.3): a member's
// copy keeps its origin (without one it has no main and nothing to push to);
// the master's detach makes its copy the hub, which needs the mTLS listener
// and an address to advertise.
func (m *Manager) guardFleetDetach(ri *RepoInstance) error {
	master := false
	if err := ri.WithRead(func(svc *store.Service) { master = ri.isFleetMaster(svc) }); err != nil {
		return err
	}
	if !master {
		return fleetErr(http.StatusConflict, "member_keeps_origin", "a member's fleet copy keeps its origin; only the master hosts the fleet")
	}
	return m.refuseNoHubListener()
}
