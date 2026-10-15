package repos

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"golang.org/x/crypto/ssh"

	"knomit/internal/fact"
	"knomit/internal/pki"
	"knomit/internal/store"
)

// Fleet create (POST /api/v1/fleet/create, local listener only): this
// instance founds a fleet with a FRESH root and is its master and first
// member. Nothing is written until every pre-flight has passed: no fleet
// here (state standalone, nothing mounted or recorded), no fleet trust
// material installed in [tls].dir, an empty fleet root directory
// (<home>/fleet), this instance's ssh-ed25519 key, and the hosting's own
// needs (none: [tls].addr and external_addresses, or members could never
// reach the master; a forge: not a knomit+https URL, reachable and not yet
// initialized). Then, in order:
//
//  1. mint the root: <home>/fleet/root.key (plain, 0600), root.crt and an
//     empty crl.pem (pki.NewRoot, pki.IssueCRL);
//  2. create the fleet repository from the built-in fleet ontology through
//     the fleet door, and record it as this instance's fleet repository
//     (recordFleetRepo, so it mounts again after a restart);
//  3. on the agent branch, as separate commits: ROOT FIRST root.crt and
//     crl.pem (kb/conventions/fleet/setup/root-first); this instance's leaf
//     and its one issued/<serial>.pem; its member record, state active;
//  4. install the bundle (leaf, root, CRL) as this instance's TLS identity;
//  5. publish main: without an origin main fast-forwards to the agent branch
//     here; with one the agent branch is pushed, then main = that tip
//     (PublishUpstream, never forced), and local main follows.
//
// A failure in 1-4 rolls everything back: the repository (and its record),
// <home>/fleet, the installed material and the fleet state. Step 5 is past
// the point of no return: a failed push leaves the state registering and the
// sync loop retries the agent branch.

const (
	fleetRootValidity = 10 * 365 * 24 * time.Hour
	fleetLeafValidity = 90 * 24 * time.Hour
	fleetCRLValidity  = 30 * 24 * time.Hour
)

// FleetCreateSpec is what CreateFleet takes.
type FleetCreateSpec struct {
	// Name is the fleet repository's local name ("" = "fleet").
	Name string
	// Origin, when set, hosts the fleet repository on a forge (mode
	// initialize onto this URL). nil: this instance hosts it (no origin).
	Origin *OriginSpec
}

// CreateFleet founds a fleet with this instance as its master and first
// member (see the file comment). 201 at the REST layer when main holds the
// record (state registered); 202 when a forge push is pending.
func (m *Manager) CreateFleet(ctx context.Context, spec FleetCreateSpec) (FleetStatus, error) {
	m.fleetOpMu.Lock()
	defer m.fleetOpMu.Unlock()

	cs, err := m.fleetCreatePreflight(ctx, spec)
	if err != nil {
		return FleetStatus{}, err
	}
	pub, err := m.ownEd25519Key()
	if err != nil {
		return FleetStatus{}, err
	}
	email, err := pki.AgentEmail(m.AgentID())
	if err != nil {
		return FleetStatus{}, err
	}

	rb := &fleetRollback{m: m}
	ri, err := m.foundFleet(ctx, cs, pub, email, rb)
	if err != nil {
		if rerr := rb.run(); rerr != nil {
			err = fmt.Errorf("%w; the rollback was incomplete: %v", err, rerr)
		}
		return FleetStatus{}, err
	}

	if spec.Origin == nil {
		_ = ri.WithRead(func(svc *store.Service) {
			if svc == nil {
				return
			}
			if _, err := svc.AdvanceLocalUpstream(ctx, ri.AgentBranch(), svc.UpstreamBranch()); err != nil {
				// The local reconcile loop retries this.
				log.Warn().Err(err).Str("fleet", ri.Name()).Msg("fleet create: main did not follow the agent branch yet")
			}
		})
	} else {
		m.publishFleetMain(ctx, ri)
	}
	log.Info().Str("fleet", ri.Name()).Bool("forge", spec.Origin != nil).
		Msg("fleet created: fresh root, this instance's leaf and record; this instance is the master")
	return m.FleetStatus(ctx)
}

// fleetCreatePreflight runs every refusal that needs nothing written and
// returns the repo create to run.
func (m *Manager) fleetCreatePreflight(ctx context.Context, spec FleetCreateSpec) (CreateSpec, error) {
	db := m.ControlDB()
	if db == nil {
		return CreateSpec{}, errors.New("fleet create: control.db is not open")
	}
	row, err := readFleetRow(db)
	if err != nil {
		return CreateSpec{}, err
	}
	if row.State != FleetStandalone {
		return CreateSpec{}, fleetErr(http.StatusConflict, "fleet_present", "this instance is already in a fleet (state "+row.State+")")
	}
	if ri := m.fleetRepo(); ri != nil {
		return CreateSpec{}, fleetErr(http.StatusConflict, "fleet_present", fmt.Sprintf("this instance already has a fleet repository, %q", ri.Name()))
	}
	if uid, err := recordedFleetRepo(db); err != nil {
		return CreateSpec{}, err
	} else if uid != "" {
		return CreateSpec{}, fleetErr(http.StatusConflict, "fleet_present", "this instance has a recorded fleet repository (uid "+uid+") that is not mounted; archive and purge it first")
	}
	if err := m.refuseInstalledTrust(); err != nil {
		return CreateSpec{}, err
	}
	if err := m.refuseFleetRootDir(); err != nil {
		return CreateSpec{}, err
	}
	if err := pki.ValidAgentID(m.AgentID()); err != nil {
		return CreateSpec{}, err
	}
	if _, err := m.ownEd25519Key(); err != nil {
		return CreateSpec{}, err
	}
	y, err := fact.FleetOntology().Serialize()
	if err != nil {
		return CreateSpec{}, err
	}
	name := spec.Name
	if name == "" {
		name = "fleet"
	}
	cs := CreateSpec{Name: name, Mode: "custom", OntologyYAML: string(y), fleetDoor: true}
	if spec.Origin == nil {
		// Without an origin this instance is the hub: members push to it
		// over the mTLS listener at the addresses its record advertises.
		if m.deps.Cfg.TLS.Addr == "" {
			return CreateSpec{}, fleetErr(http.StatusUnprocessableEntity, "listener_required", "a fleet hosted here needs the mTLS listener: set [tls].addr in knomit.toml (members push to it)")
		}
		if len(m.deps.Cfg.ExternalAddresses) == 0 {
			return CreateSpec{}, fleetErr(http.StatusUnprocessableEntity, "addresses_required", "a fleet hosted here needs external_addresses in knomit.toml: they are how members reach this instance")
		}
	} else {
		if pki.IsFleetURL(spec.Origin.URL) {
			return CreateSpec{}, fleetErr(http.StatusUnprocessableEntity, "knomit_origin", "a knomit-hosted fleet is hosted by its master: create it without an origin, or give a forge URL")
		}
		o := *spec.Origin
		cs.Mode, cs.Origin = "initialize", &o
	}
	// The repo create's own refusals (name, origin in use, reachability, an
	// already initialized branch), with no I/O but the origin probe.
	if err := m.CreatePreflight(ctx, cs); err != nil {
		return CreateSpec{}, err
	}
	return cs, nil
}

// refuseInstalledTrust refuses while any fleet trust material is installed in
// [tls].dir: this instance is (or was) enrolled in a fleet and must leave it
// first. Installing the new fleet's leaf would otherwise move it silently.
func (m *Manager) refuseInstalledTrust() error {
	dir := m.deps.Cfg.TLS.Dir
	if dir == "" {
		return errors.New("fleet create: [tls].dir is not set")
	}
	for _, f := range []string{pki.RootCertFile, pki.CRLFile, pki.InstanceCertFile} {
		p := filepath.Join(dir, f)
		if _, err := os.Lstat(p); err == nil {
			return fleetErr(http.StatusConflict, "trust_installed", "fleet trust material is installed ("+p+"): this instance is enrolled in a fleet; leave it first")
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("fleet create: %w", err)
		}
	}
	return nil
}

// refuseFleetRootDir refuses unless <home>/fleet is absent or empty: a fleet
// is founded with a fresh root, and a leftover root key is an
// undecommissioned fleet. An empty directory is all the rollback may remove.
func (m *Manager) refuseFleetRootDir() error {
	dir := m.fleetRootDir()
	entries, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("fleet create: %w", err)
	case len(entries) > 0:
		return fleetErr(http.StatusConflict, "root_present", "the fleet root directory "+dir+" is not empty (it holds "+entries[0].Name()+"): a fleet is created with a fresh root; decommission the previous fleet first")
	}
	return nil
}

// ownEd25519Key is this instance's signing key, the key its leaf names.
func (m *Manager) ownEd25519Key() (ed25519.PublicKey, error) {
	if m.deps.Signer == nil {
		return nil, store.ErrNoSigner
	}
	cpk, ok := m.deps.Signer.PublicKey().(ssh.CryptoPublicKey)
	if !ok {
		return nil, errors.New("fleet create: this instance's key is not usable for a certificate")
	}
	pub, ok := cpk.CryptoPublicKey().(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("fleet create: this instance's key is %s, want ssh-ed25519", m.deps.Signer.PublicKey().Type())
	}
	return pub, nil
}

// fleetRollback undoes what foundFleet did, newest first.
type fleetRollback struct {
	m     *Manager
	undos []func() error
}

func (rb *fleetRollback) add(f func() error) { rb.undos = append(rb.undos, f) }

func (rb *fleetRollback) run() error {
	var errs []error
	for i := len(rb.undos) - 1; i >= 0; i-- {
		if err := rb.undos[i](); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// foundFleet is steps 1-4 of the file comment; every step registers its undo
// on rb before the next one runs.
func (m *Manager) foundFleet(ctx context.Context, cs CreateSpec, pub ed25519.PublicKey, email string, rb *fleetRollback) (*RepoInstance, error) {
	dir := m.fleetRootDir()
	rb.add(func() error { return os.RemoveAll(dir) })
	root, err := pki.NewRoot(dir, "knomit-fleet-root-"+m.AgentID(), fleetRootValidity)
	if err != nil {
		return nil, err
	}
	crlPEM, err := pki.IssueCRL(dir, root, nil, big.NewInt(1), time.Now().Add(fleetCRLValidity))
	if err != nil {
		return nil, err
	}
	rootPEM, err := os.ReadFile(filepath.Join(dir, pki.RootCertFile))
	if err != nil {
		return nil, err
	}

	ri, err := m.Create(ctx, cs, func(Event) {})
	if err != nil {
		return nil, err
	}
	// Deleting the repo also drops its instance_fleet_repo row (cascade).
	rb.add(func() error {
		return m.deleteRepo(ri.Name(), "fleet create failed after the repository was created")
	})
	if err := recordFleetRepo(m.ControlDB(), ri.UID()); err != nil {
		return nil, err
	}

	ont := ri.Ontology()
	layout, ok := ont.PKILayout()
	if !ok || layout.CRL == "" || !layout.HasLeaves() {
		return nil, errors.New("fleet create: the fleet ontology declares no pki root, crl and leaf")
	}
	issuedPattern, err := issuancePattern(ont)
	if err != nil {
		return nil, err
	}
	if err := m.writeFleetArtifacts(ctx, ri, map[string]string{
		layout.Root: string(rootPEM),
		layout.CRL:  string(crlPEM),
	}, "fleet: root certificate and CRL (root first)"); err != nil {
		return nil, err
	}
	certPEM, rec, err := pki.IssueInstance(dir, root, pub, email, fleetLeafValidity)
	if err != nil {
		return nil, err
	}
	if err := m.writeFleetArtifacts(ctx, ri, map[string]string{
		layout.LeafPath(m.AgentID()):                       string(certPEM),
		strings.Replace(issuedPattern, "*", rec.Serial, 1): string(certPEM),
	}, "fleet: "+m.AgentID()+"'s certificate (serial "+rec.Serial+")"); err != nil {
		return nil, err
	}
	if err := m.writeOwnRecord(ctx, ri, fact.MemberActive); err != nil {
		return nil, err
	}

	state := FleetRegistered
	if cs.Origin != nil {
		state = FleetRegistering
	}
	db := m.ControlDB()
	rb.add(func() error { return setFleetState(db, FleetStandalone, time.Now()) })
	if err := setFleetState(db, state, time.Now()); err != nil {
		return nil, err
	}

	// Last: an install that fails part way (an I/O error between its
	// renames) leaves files behind, which the undo removes.
	var bundle bytes.Buffer
	bundle.Write(certPEM)
	bundle.Write(rootPEM)
	bundle.Write(crlPEM)
	rb.add(func() error { return m.uninstallFreshTrust(root) })
	if _, err := pki.InstallBundle(m.deps.Cfg.TLS.Dir, m.deps.KeyPath, bundle.Bytes(), pki.InstallOptions{}); err != nil {
		return nil, fmt.Errorf("fleet create: install this instance's certificate: %w", err)
	}
	if afterFleetInstall != nil {
		if err := afterFleetInstall(); err != nil {
			return nil, err
		}
	}
	return ri, nil
}

// afterFleetInstall is a test seam: nil in production. A test sets it to fail
// a create after its last step, to see the whole rollback.
var afterFleetInstall func() error

// uninstallFreshTrust removes the trust material a failed create installed:
// only when the root installed in [tls].dir is this create's root, so
// nothing installed by anyone else is touched (the pre-flight found none).
func (m *Manager) uninstallFreshTrust(root pki.Root) error {
	dir := m.deps.Cfg.TLS.Dir
	installed, err := pki.InstalledRoot(dir)
	if err != nil || installed.Fingerprint == "" {
		return nil
	}
	if id, err := pki.RootID(root.Cert); err != nil || id != installed.Fingerprint {
		return nil
	}
	var errs []error
	for _, f := range []string{pki.InstanceCertFile, pki.CRLFile, pki.RootCertFile} {
		if err := os.Remove(filepath.Join(dir, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// publishFleetMain is a forge create's step 5: push the agent branch, then
// main = its tip without force, then local main follows. Registered once
// main is on the forge; any failure is recorded as the fleet's last error and
// leaves the state registering.
func (m *Manager) publishFleetMain(ctx context.Context, ri *RepoInstance) {
	db := m.ControlDB()
	err := func() error {
		var perr error
		if werr := ri.WithRead(func(svc *store.Service) {
			if svc == nil {
				perr = errors.New("fleet repository is not open")
				return
			}
			remote, err := svc.Remote().GetRemote("origin")
			if err != nil || remote == nil {
				perr = fmt.Errorf("fleet repository has no origin: %v", err)
				return
			}
			auth, err := makeRemoteAuthFn(m.deps.Cfg.Remote, m.deps.KeyPath)(remote)
			if err != nil {
				perr = err
				return
			}
			if _, err := svc.Remote().Push(ctx, ri.AgentBranch(), auth); err != nil {
				perr = err
				return
			}
			if _, err := svc.Remote().PublishUpstream(ctx, ri.AgentBranch(), auth); err != nil {
				perr = err
				return
			}
			if _, err := svc.AdvanceLocalUpstream(ctx, ri.AgentBranch(), svc.UpstreamBranch()); err != nil {
				log.Warn().Err(err).Str("fleet", ri.Name()).Msg("fleet create: local main did not follow the published main yet")
			}
		}); werr != nil && perr == nil {
			perr = werr
		}
		return perr
	}()
	if rerr := recordFleetAttempt(db, err, time.Now()); rerr != nil {
		log.Warn().Err(rerr).Msg("fleet create: recording the push attempt failed")
	}
	if err != nil {
		log.Warn().Err(err).Str("fleet", ri.Name()).Msg("fleet create: publishing the fleet to its origin failed; the sync loop retries the agent branch")
		return
	}
	if err := setFleetState(db, FleetRegistered, time.Now()); err != nil {
		log.Warn().Err(err).Msg("fleet create: entering registered failed")
	}
}

// issuancePattern is the repo path pattern of the ontology's pki: issuance
// rule (one '*' where the file name goes).
func issuancePattern(ont *fact.Ontology) (string, error) {
	var found []string
	for pat, block := range ont.Artifacts {
		if v, _ := block[fact.ArtifactKeyPKI].(string); v == fact.PKIIssuance {
			found = append(found, pat)
		}
	}
	if len(found) != 1 || strings.Count(found[0], "*") != 1 {
		return "", fmt.Errorf("fleet create: the fleet ontology must declare exactly one pki: issuance pattern with one '*', found %v", found)
	}
	return fact.ArtifactsRoot + "/" + found[0], nil
}

// writeFleetArtifacts commits files as ONE commit on ri's agent branch,
// through the ordinary fact/artifact write path.
func (m *Manager) writeFleetArtifacts(ctx context.Context, ri *RepoInstance, files map[string]string, message string) error {
	var werr error
	if err := ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			werr = errors.New("fleet repository is not open")
			return
		}
		_, _, werr = svc.Facts().BatchWriteFacts(ctx, ri.AgentBranch(), files, nil, message, "created")
	}); err != nil {
		return err
	}
	return werr
}
