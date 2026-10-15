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
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
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
//     hand back the bundle: leaf + root + CRL, nothing else.
//   - JOIN (member): check the bundle is for THIS instance's key and id,
//     install it, clone the fleet repository through the fleet door, check its
//     root is the bundle's, record it, write the own record and push it ONCE.
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
	AgentID         string `json:"agent_id"`
	Serial          string `json:"serial"`
	Bundle          string `json:"bundle"` // leaf, root and CRL, PEM
	RootFingerprint string `json:"root_fingerprint"`
	// Published says main holds the leaf now (a joiner is admitted only
	// then); PublishError is why not. The sync loop publishes it again.
	Published    bool   `json:"published"`
	PublishError string `json:"publish_error,omitempty"`
}

// FleetJoinSpec is what POST /fleet/join takes.
type FleetJoinSpec struct {
	URL, AuthMethod, AuthToken string
	Bundle                     []byte
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
// returns the bundle the joiner installs with POST /fleet/join.
func (m *Manager) EnrollFleetMember(ctx context.Context, agentID, keyLine string) (FleetEnrollment, error) {
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
	certPEM, rec, err := pki.IssueInstance(m.fleetRootDir(), root, pub, email, fleetLeafValidity)
	if err != nil {
		return FleetEnrollment{}, err
	}
	if err := m.writeFleetArtifacts(ctx, ri, map[string]string{
		layout.LeafPath(agentID):                    string(certPEM),
		strings.Replace(issued, "*", rec.Serial, 1): string(certPEM),
	}, "fleet: "+agentID+"'s certificate (serial "+rec.Serial+")"); err != nil {
		return FleetEnrollment{}, err
	}
	rootID, err := pki.RootID(root.Cert)
	if err != nil {
		return FleetEnrollment{}, err
	}
	var bundle bytes.Buffer
	bundle.Write(certPEM)
	bundle.Write(certPEMOf(root.Cert))
	bundle.Write(crlPEMOf(f.PKI.CRL))
	out := FleetEnrollment{AgentID: agentID, Serial: rec.Serial, Bundle: bundle.String(), RootFingerprint: rootID, Published: true}
	if perr := m.publishMasterWrite(ctx, ri); perr != nil {
		log.Warn().Err(perr).Str("agent", agentID).Msg("fleet enroll: main does not hold the leaf yet; the sync loop publishes it")
		out.Published, out.PublishError = false, perr.Error()
	}
	log.Info().Str("agent", agentID).Str("serial", rec.Serial).Msg("fleet: member enrolled")
	return out, nil
}

// JoinFleet joins the fleet at spec.URL with the bundle the master's enroll
// returned (see the file comment). It answers registered: the record is on
// the fleet's origin, and counts once the master admits it into main.
func (m *Manager) JoinFleet(ctx context.Context, spec FleetJoinSpec) (FleetStatus, error) {
	defer m.lockFleetOp()()
	if err := m.recoverFleetCreate(ctx); err != nil {
		return FleetStatus{}, err
	}
	db := m.ControlDB()
	if db == nil {
		return FleetStatus{}, errors.New("fleet join: control.db is not open")
	}
	if err := m.refuseFleetPresent(db); err != nil {
		return FleetStatus{}, err
	}
	if err := m.refuseInstalledTrust(); err != nil {
		return FleetStatus{}, err
	}
	b, err := pki.SplitBundle(spec.Bundle)
	if err != nil {
		return FleetStatus{}, fleetErr(http.StatusUnprocessableEntity, "bundle_invalid", err.Error())
	}
	if _, err := pki.PreviewBundle(m.deps.KeyPath, spec.Bundle); err != nil {
		if errors.Is(err, pki.ErrKeyMismatch) {
			return FleetStatus{}, fleetErr(http.StatusUnprocessableEntity, "key_mismatch", err.Error())
		}
		return FleetStatus{}, fleetErr(http.StatusUnprocessableEntity, "bundle_invalid", err.Error())
	}
	var leafID string
	if len(b.Leaf.EmailAddresses) == 1 {
		leafID = pki.AgentIDOfEmail(b.Leaf.EmailAddresses[0])
	}
	if leafID != m.AgentID() {
		return FleetStatus{}, fleetErr(http.StatusUnprocessableEntity, "agent_mismatch", fmt.Sprintf("the bundle's certificate is for agent %q, not this instance's %q", leafID, m.AgentID()))
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
	ri, err := m.Create(ctx, CreateSpec{Name: m.freeFleetName(spec.URL), Mode: "clone",
		Origin: &OriginSpec{URL: spec.URL, AuthMethod: spec.AuthMethod, AuthToken: spec.AuthToken}, fleetDoor: true}, func(Event) {})
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
			return FleetLeft{}, fleetErr(http.StatusConflict, "master", "this instance is the fleet's master: decommission the fleet (POST /api/v1/fleet/decommission), or hand the root to another member (GET and PUT /api/v1/fleet/root) and stand down (DELETE /api/v1/fleet/root) first")
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
	if err := appendFleetLog(filepath.Join(m.fleetRootDir(), pki.RevokedLogFile), rv.Line); err != nil {
		log.Warn().Err(err).Msg("fleet revoke: the local revocation log was not written")
	}
	if err := os.WriteFile(filepath.Join(m.fleetRootDir(), pki.CRLFile), rv.CRLPEM, 0o644); err != nil {
		log.Warn().Err(err).Msg("fleet revoke: the local CRL copy was not written")
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

func appendFleetLog(path string, line []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(line); err != nil {
		f.Close()
		return err
	}
	return f.Close()
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
	if ok, err := m.rootKeyHere(); err != nil {
		return FleetRoot{}, err
	} else if ok {
		return FleetRoot{}, present()
	}
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
			return fleetErr(http.StatusConflict, "hub", "this instance hosts the fleet (its copy of "+ri.Name()+" has no origin): point it at the new master first (PUT /api/v1/repos/"+ri.Name()+"/origin), or the fleet has no master and no host")
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	log.Info().Str("dir", dir).Msg("fleet: stood down as master; the root files are deleted here")
	return nil
}

// GuardFleetDetach is DELETE /repos/{fleet}/origin's check (R3.3): a member's
// copy keeps its origin (without one it has no main and nothing to push to);
// the master's detach makes its copy the hub, which needs the mTLS listener
// and an address to advertise. Any other repository: nil.
func (m *Manager) GuardFleetDetach(ri *RepoInstance) error {
	if ri == nil || !ri.IsFleet() {
		return nil
	}
	master := false
	if err := ri.WithRead(func(svc *store.Service) { master = ri.isFleetMaster(svc) }); err != nil {
		return err
	}
	if !master {
		return fleetErr(http.StatusConflict, "member_keeps_origin", "a member's fleet copy keeps its origin; only the master hosts the fleet")
	}
	return m.refuseNoHubListener()
}

// ErrFleetUpstreamFixed refuses PATCH /repos/{fleet}/origin/upstream (R3.3).
var ErrFleetUpstreamFixed = errors.New("the fleet repository's consensus branch is fixed at create")

// FleetMergeRefusal is the check of an operator's merge of the pushed branch
// at tip on the fleet repository (R3.3): only the master merges, and it
// merges only what the fleet's judge admits against main NOW (tip ∖ main,
// the core the master's merger re-judges with), so a branch admitted at entry
// and refused since (a revocation, an edit of another member's folder) is not
// merged by hand either. Any other repository: nil.
func (ri *RepoInstance) FleetMergeRefusal(ctx context.Context, svc *store.Service, branch string, tip plumbing.Hash) error {
	if !ri.IsFleet() {
		return nil
	}
	if !ri.isFleetMaster(svc) {
		return fleetErr(http.StatusConflict, "not_master", "only the fleet's master merges")
	}
	reason, err := svc.JudgePeerBranch(ctx, branch, ri.AgentBranch(), tip, nil)
	if err != nil {
		return err
	}
	if reason != "" {
		return fleetErr(http.StatusConflict, "judge_refused", "the fleet's judge refuses "+branch+" at "+shortHash(tip.String())+" against main: "+reason)
	}
	return nil
}
