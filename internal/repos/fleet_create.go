package repos

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing/transport"
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
// reach the master; a forge: reachable and not yet initialized). Another
// knomit instance cannot be the origin: refuseInstalledTrust guarantees this
// instance holds no fleet trust yet, so such an origin never classifies as
// a peer and fails the reachability probe at its TLS handshake. Then, in
// order:
//
//  0. write the create marker (control.db instance_fleet_create);
//  1. mint the root, an empty CRL and this instance's leaf into a staging
//     directory beside <home>/fleet (fleetRootStagingDir), and record the
//     root's id on the marker;
//  2. create the fleet repository from the built-in fleet ontology through
//     the fleet door, and record it as this instance's fleet repository
//     (recordFleetRepo, so it mounts again after a restart);
//  3. on the agent branch, as separate commits: ROOT FIRST root.crt and
//     crl.pem (kb/conventions/fleet/setup/root-first); this instance's leaf
//     and its one issued/<serial>.pem; its member record, state active; the
//     host record (fleet_host.go: the hub's URLs, or the forge's);
//  4. install the bundle (leaf, root, CRL) as this instance's TLS identity;
//  5. move the staging directory to <home>/fleet;
//  6. set the fleet state and delete the marker, in one transaction
//     (finishFleetCreate): the point of no return;
//  7. publish main: without an origin main fast-forwards to the agent branch
//     here; with one the agent branch is pushed, then main = that tip
//     (PublishUpstream, never forced), and local main follows.
//
// A failure in 1-5 rolls ALL of it back (undoFleetCreate): on a forge the
// agent branch this create pushed (deleted with a lease on the hash it
// pushed), the repository and its record, the installed material, the
// staging directory and the root, then the marker. The rollback reads what
// to undo from the marker and from what exists, not from what the failed
// call remembers. So the same rollback also runs for a create that died part
// way (a crash or a kill): a marker found at boot, or by the next create or
// join, is undone first. A rollback that fails part way keeps the
// marker and is retried there.
//
// A failed step 7 is not rolled back: the state stays registering with
// last_error set (202), and the sync loop's next push of the agent branch
// publishes main again (fleetPushed).

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

// fleetCreateHooks are test seams for the fleet create. Every field is nil
// in production.
type fleetCreateHooks struct {
	// step runs after each named step of a create (fleetCreateSteps). An
	// error fails the create there, and the rollback runs. errFleetCreateDied
	// makes the create return at once WITHOUT a rollback, as a process
	// killed at that point would leave things.
	step func(step string) error
	// beforePublish runs before main is published (the PublishUpstream of a
	// forge create, or of fleetPushed; the AdvanceLocalUpstream of a fleet
	// hosted here). An error is the publish's result.
	beforePublish func() error
	// contended runs when a fleet operation finds fleetOpMu held and is
	// about to wait for it.
	contended func()
}

// fleetCreateSteps are the names the step hook sees, in order.
var fleetCreateSteps = []string{"marked", "root", "repo", "recorded", "committed", "installed", "rooted"}

// errFleetCreateDied is the step hook's "the process died here".
var errFleetCreateDied = errors.New("fleet create: the process died here (test seam)")

var (
	fleetHooksMu   sync.Mutex
	fleetTestHooks fleetCreateHooks
)

func currentFleetHooks() fleetCreateHooks {
	fleetHooksMu.Lock()
	defer fleetHooksMu.Unlock()
	return fleetTestHooks
}

func fleetCreateStep(step string) error {
	if h := currentFleetHooks().step; h != nil {
		return h(step)
	}
	return nil
}

// lockFleetOp takes fleetOpMu (the contended hook sees a wait).
func (m *Manager) lockFleetOp() func() {
	if !m.fleetOpMu.TryLock() {
		if h := currentFleetHooks().contended; h != nil {
			h()
		}
		m.fleetOpMu.Lock()
	}
	return m.fleetOpMu.Unlock
}

// fleetRootStagingDir is where a create mints the root before it moves it to
// <home>/fleet (step 5). Nothing but a create writes it, so the rollback
// removes it whole. Until the move, an operator command that writes
// <home>/fleet (PUT /api/v1/fleet/root) never sees this create's root,
// and nothing this create undoes is in <home>/fleet.
func (m *Manager) fleetRootStagingDir() string {
	return m.fleetRootDir() + ".creating"
}

// CreateFleet founds a fleet with this instance as its master and first
// member (see the file comment). The REST layer answers 201 when main holds
// the record (state registered, record active) and 202 otherwise. It runs
// detached from ctx's cancellation, under the Manager's create lifetime, so
// a client that hangs up does not cut a create short (Close still does).
func (m *Manager) CreateFleet(ctx context.Context, spec FleetCreateSpec) (FleetStatus, error) {
	defer m.lockFleetOp()()

	m.createWg.Add(1)
	defer m.createWg.Done()
	ctx, cancel := context.WithTimeout(m.createsCtx, m.createTimeout())
	defer cancel()

	if err := m.recoverFleetCreate(ctx); err != nil {
		return FleetStatus{}, err
	}
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
	_, statErr := os.Stat(m.deps.Cfg.TLS.Dir)
	if err := writeFleetCreateMarker(m.ControlDB(), fleetCreateMarker{
		RepoName: cs.Name, StartedAt: time.Now(), TLSDirExisted: statErr == nil,
	}); err != nil {
		return FleetStatus{}, err
	}

	ri, err := m.foundFleet(ctx, cs, pub, email)
	if err != nil {
		if errors.Is(err, errFleetCreateDied) {
			return FleetStatus{}, err
		}
		// The rollback is not cut short by the create's own deadline.
		if rerr := m.undoFleetCreate(context.WithoutCancel(ctx)); rerr != nil {
			_ = recordFleetAttempt(m.ControlDB(), fmt.Errorf("fleet create rollback incomplete: %w", rerr), time.Now())
			err = fmt.Errorf("%w; the rollback is incomplete (%v): it is retried at the next start and by the next fleet create or join", err, rerr)
		}
		return FleetStatus{}, err
	}

	if spec.Origin == nil {
		if err := m.publishFleetMainNow(ctx, ri); err != nil {
			// The local reconcile loop retries this; the record is pending.
			log.Warn().Err(err).Str("fleet", ri.Name()).Msg("fleet create: main did not follow the agent branch yet")
		}
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
	if err := m.refuseFleetPresent(db); err != nil {
		return CreateSpec{}, err
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
		// Without an origin this instance is the hub.
		if err := m.refuseNoHubListener(); err != nil {
			return CreateSpec{}, err
		}
	} else {
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

// refuseFleetPresent refuses while this instance is in a fleet, in any form:
// a fleet state other than standalone, a mounted fleet repository, or a
// recorded one that is not mounted. Create and join both start here.
func (m *Manager) refuseFleetPresent(db *sql.DB) error {
	row, err := readFleetRow(db)
	if err != nil {
		return err
	}
	if row.State != FleetStandalone {
		return fleetErr(http.StatusConflict, "fleet_present", "this instance is in a fleet (state "+row.State+"): leave the fleet first (Fleet → Settings → Leave)")
	}
	if ri := m.fleetRepo(); ri != nil {
		return fleetErr(http.StatusConflict, "fleet_present", fmt.Sprintf("this instance has a fleet repository, %q, whose join did not finish (state standalone): remove it by leaving the fleet (Fleet → Settings → Leave)", ri.Name()))
	}
	uid, err := recordedFleetRepo(db)
	if err != nil {
		return err
	}
	if uid != "" {
		name := uid
		for _, u := range m.Unavailable() {
			if u.Record.UID == uid {
				name = u.Record.Name
			}
		}
		return fleetErr(http.StatusConflict, "fleet_present", "this instance has a recorded fleet repository (uid "+uid+") that is not mounted: archive "+name+" if it is still listed (its Settings → Danger zone → Archive), then purge it from Archived")
	}
	return nil
}

// refuseInstalledTrust refuses while any fleet trust material is installed in
// [tls].dir: this instance holds another fleet's certificate, and installing
// the new fleet's leaf would replace it silently.
func (m *Manager) refuseInstalledTrust() error {
	dir := m.deps.Cfg.TLS.Dir
	if dir == "" {
		return errors.New("fleet: [tls].dir is not set")
	}
	for _, f := range []string{pki.RootCertFile, pki.CRLFile, pki.InstanceCertFile} {
		p := filepath.Join(dir, f)
		if _, err := os.Lstat(p); err == nil {
			return fleetErr(http.StatusConflict, "trust_installed", "fleet trust material is installed ("+p+"): this instance holds another fleet's certificate, which this would replace. If that fleet is no longer used, move "+pki.RootCertFile+", "+pki.CRLFile+" and "+pki.InstanceCertFile+" out of "+dir+" and try again")
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("fleet: %w", err)
		}
	}
	return nil
}

// refuseFleetRootDir refuses unless <home>/fleet is absent or empty: a fleet
// is founded with a fresh root, and a root already there is another fleet's.
func (m *Manager) refuseFleetRootDir() error {
	dir := m.fleetRootDir()
	entries, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("fleet create: %w", err)
	case len(entries) > 0:
		return fleetErr(http.StatusConflict, "root_present", "the fleet root directory "+dir+" is not empty (it holds "+entries[0].Name()+"): a fleet is created with a fresh root. If the fleet that root belongs to is no longer used, move the directory away (it may hold a root key: keep it while that fleet could be needed) and create again")
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

// foundFleet is steps 1-6 of the file comment. Any error before step 6 is
// undone by undoFleetCreate.
func (m *Manager) foundFleet(ctx context.Context, cs CreateSpec, pub ed25519.PublicKey, email string) (*RepoInstance, error) {
	db := m.ControlDB()
	if err := fleetCreateStep("marked"); err != nil {
		return nil, err
	}

	// 1. The root, its CRL and this instance's leaf, in the staging directory.
	staging := m.fleetRootStagingDir()
	if err := os.RemoveAll(staging); err != nil {
		return nil, fmt.Errorf("fleet create: %w", err)
	}
	root, err := pki.NewRoot(staging, "knomit-fleet-root-"+m.AgentID(), fleetRootValidity)
	if err != nil {
		return nil, err
	}
	rootID, err := pki.RootID(root.Cert)
	if err != nil {
		return nil, err
	}
	if err := setFleetCreateRoot(db, rootID); err != nil {
		return nil, err
	}
	crlPEM, err := pki.IssueCRL(staging, root, nil, big.NewInt(1), time.Now().Add(fleetCRLValidity))
	if err != nil {
		return nil, err
	}
	rootPEM, err := os.ReadFile(filepath.Join(staging, pki.RootCertFile))
	if err != nil {
		return nil, err
	}
	certPEM, rec, err := pki.IssueInstance(staging, root, pub, email, fleetLeafValidity)
	if err != nil {
		return nil, err
	}
	if err := fleetCreateStep("root"); err != nil {
		return nil, err
	}

	// 2. The repository, recorded as this instance's fleet repository.
	ri, err := m.Create(ctx, cs, func(Event) {})
	if err != nil {
		return nil, err
	}
	if err := fleetCreateStep("repo"); err != nil {
		return nil, err
	}
	if err := recordFleetRepo(db, ri.UID()); err != nil {
		return nil, err
	}
	if err := fleetCreateStep("recorded"); err != nil {
		return nil, err
	}

	// 3. Root first, then the leaf, then the record.
	ont := ri.Ontology()
	layout, ok := ont.PKILayout()
	if !ok || layout.CRL == "" || !layout.HasLeaves() {
		return nil, errors.New("fleet create: the fleet ontology declares no pki root, crl and leaf")
	}
	issuedPattern, err := artifactPattern(ont, fact.PKIIssuance)
	if err != nil {
		return nil, err
	}
	if err := m.writeFleetArtifacts(ctx, ri, map[string]string{
		layout.Root: string(rootPEM),
		layout.CRL:  string(crlPEM),
	}, "fleet: root certificate and CRL (root first)"); err != nil {
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
	hosted := m.hubFleetURLs(ri)
	if cs.Origin != nil {
		hosted = []string{cs.Origin.URL}
	}
	if err := m.writeFleetHost(ctx, ri, hosted); err != nil {
		return nil, err
	}
	if err := fleetCreateStep("committed"); err != nil {
		return nil, err
	}

	// 4. This instance's TLS identity.
	var bundle bytes.Buffer
	bundle.Write(certPEM)
	bundle.Write(rootPEM)
	bundle.Write(crlPEM)
	if _, err := pki.InstallBundle(m.deps.Cfg.TLS.Dir, m.deps.KeyPath, bundle.Bytes(), pki.InstallOptions{}); err != nil {
		return nil, fmt.Errorf("fleet create: install this instance's certificate: %w", err)
	}
	if err := fleetCreateStep("installed"); err != nil {
		return nil, err
	}

	// 5. The root to its place. An empty <home>/fleet (the pre-flight allows
	// one) is removed first, and only an empty one: a rename never replaces
	// a directory that holds something.
	dir := m.fleetRootDir()
	if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("fleet create: the fleet root directory %s is no longer empty: %w", dir, err)
	}
	if err := os.Rename(staging, dir); err != nil {
		return nil, fmt.Errorf("fleet create: move the fleet root into %s: %w", dir, err)
	}
	if err := fleetCreateStep("rooted"); err != nil {
		return nil, err
	}

	// 6. The point of no return.
	state := FleetRegistered
	if cs.Origin != nil {
		state = FleetRegistering
	}
	if err := finishFleetCreate(db, state, time.Now()); err != nil {
		return nil, err
	}
	return ri, nil
}

// recoverFleetCreate rolls back a create that did not finish (a marker with
// no create running: the caller holds fleetOpMu). It is called at boot and
// before every create and join. A rollback that fails part way is a
// create_unfinished refusal, and is retried at the next of those.
func (m *Manager) recoverFleetCreate(ctx context.Context) error {
	db := m.ControlDB()
	if db == nil {
		return nil
	}
	mk, err := readFleetCreateMarker(db)
	if err != nil || mk == nil {
		return err
	}
	log.Warn().Str("fleet", mk.RepoName).Time("started", mk.StartedAt).
		Msg("fleet create: an earlier create did not finish; rolling it back")
	if err := m.undoFleetCreate(ctx); err != nil {
		_ = recordFleetAttempt(db, fmt.Errorf("fleet create rollback incomplete: %w", err), time.Now())
		return fleetErr(http.StatusConflict, "create_unfinished", "an earlier fleet create did not finish, and undoing it failed: "+err.Error()+". It is retried at the next start and by the next fleet create or join. If the forge's "+m.deps.AgentBranch+" moved to a tip this instance did not push, delete that branch on the forge by hand, then retry")
	}
	log.Info().Str("fleet", mk.RepoName).Msg("fleet create: the unfinished create is rolled back")
	return nil
}

// undoFleetCreate is the rollback (see the file comment). It undoes what the
// marker and the disk show, newest first, and deletes the marker last, so
// every step is safe to run again and a failure stops it with the marker
// kept. Without a marker it does nothing.
func (m *Manager) undoFleetCreate(ctx context.Context) error {
	db := m.ControlDB()
	mk, err := readFleetCreateMarker(db)
	if err != nil || mk == nil {
		return err
	}
	// The repository, and first what it pushed to a forge: the repository
	// holds the record of that push.
	if ri := m.fleetCreateRepo(db, mk); ri != nil {
		if err := m.retractFleetBranch(ctx, ri); err != nil {
			return fmt.Errorf("delete the agent branch on the forge: %w", err)
		}
		if err := m.deleteRepo(ri.Name(), "fleet create rolled back"); err != nil {
			return err
		}
	} else if name, ok := m.unavailableFleetCreateRepo(mk); ok {
		// It never mounted again: what it pushed (if anything) cannot be
		// read, and is not deleted.
		if err := m.deleteRepo(name, "fleet create rolled back"); err != nil {
			return err
		}
	}
	if err := m.uninstallFreshTrust(mk); err != nil {
		return err
	}
	if err := os.RemoveAll(m.fleetRootStagingDir()); err != nil {
		return err
	}
	if err := m.removeCreatedRoot(mk); err != nil {
		return err
	}
	return finishFleetCreate(db, FleetStandalone, time.Now())
}

// fleetCreateRepo is the mounted repository a create made: the recorded fleet
// repository, or the repository of the marker's name, provided it holds the
// fleet ontology (a user repository can never hold it, so a later one that
// took the name is never touched).
func (m *Manager) fleetCreateRepo(db *sql.DB, mk *fleetCreateMarker) *RepoInstance {
	uid, _ := recordedFleetRepo(db)
	for _, name := range m.Names() {
		ri := m.Get(name)
		if ri == nil || !fact.IsFleetOntology(ri.Ontology()) {
			continue
		}
		if (uid != "" && ri.UID() == uid) || ri.Name() == mk.RepoName {
			return ri
		}
	}
	return nil
}

// unavailableFleetCreateRepo is a repository of the marker's name, registered
// since the create began, that did not mount (a create that died inside the
// repo create, before the store was whole).
func (m *Manager) unavailableFleetCreateRepo(mk *fleetCreateMarker) (string, bool) {
	for _, u := range m.Unavailable() {
		if u.Record.Name == mk.RepoName && u.Record.CreatedAt >= mk.StartedAt.Unix() {
			return u.Record.Name, true
		}
	}
	return "", false
}

// retractFleetBranch deletes the agent branch the create pushed to the
// fleet repository's forge, with a lease on the last tip this instance saw
// there, pushed or fetched (store RetractPushedBranch: the remote-tracking
// ref). A fleet hosted here has no origin: nothing to do.
func (m *Manager) retractFleetBranch(ctx context.Context, ri *RepoInstance) error {
	var rerr error
	if err := ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			rerr = errors.New("fleet repository is not open")
			return
		}
		remote, err := svc.Remote().GetRemote("origin")
		if err != nil {
			rerr = err
			return
		}
		if remote == nil {
			return
		}
		auth, err := makeRemoteAuthFn(m.deps.Cfg.Remote, m.deps.KeyPath)(remote)
		if err != nil {
			rerr = err
			return
		}
		rerr = svc.Remote().RetractPushedBranch(ctx, ri.AgentBranch(), auth)
	}); err != nil {
		return err
	}
	return rerr
}

// uninstallFreshTrust removes the trust material the create installed, and
// only when the root installed in [tls].dir is this create's root (the
// marker's root id). Material that someone else installed meanwhile (a
// desktop or CLI install, which then made this create's install fail with
// RootDiffers) is never touched. When the create made [tls].dir, the
// directory goes with it, including the CRL watermark (crl.number) the
// install wrote. Otherwise the watermark keeps an entry for this create's
// root: it is filed per root, and no other fleet has this root, so it
// changes nothing. The install lock file beside the directory
// (pki.LockPath) is left, as any install leaves it.
func (m *Manager) uninstallFreshTrust(mk *fleetCreateMarker) error {
	if mk.RootID == "" {
		return nil // no root yet, so nothing was installed
	}
	dir := m.deps.Cfg.TLS.Dir
	installed, err := pki.InstalledRoot(dir)
	if err != nil || installed.Fingerprint != mk.RootID {
		return nil
	}
	if !mk.TLSDirExisted {
		return os.RemoveAll(dir)
	}
	return pki.Uninstall(dir)
}

// removeCreatedRoot removes <home>/fleet when it holds THIS create's root
// (moved there in step 5 by a create that then failed or died). Any other
// root there is left alone.
func (m *Manager) removeCreatedRoot(mk *fleetCreateMarker) error {
	if mk.RootID == "" {
		return nil
	}
	dir := m.fleetRootDir()
	cert, err := pki.LoadRootCert(filepath.Join(dir, pki.RootCertFile))
	if err != nil {
		return nil // absent (not moved yet), or not a root this create made
	}
	if id, err := pki.RootID(cert); err != nil || id != mk.RootID {
		return nil
	}
	return os.RemoveAll(dir)
}

// publishFleetMain is a forge create's step 7: push the agent branch, then
// publish main (publishFleetMainNow). Registered once main is on the forge.
// A failure is recorded as the fleet's last error and the state is set to
// registering, whatever a concurrent push of the sync loop did meanwhile:
// the loop's next successful push publishes main again (fleetPushed).
func (m *Manager) publishFleetMain(ctx context.Context, ri *RepoInstance) {
	db := m.ControlDB()
	err := func() error {
		if perr := m.pushAgentBranch(ctx, ri); perr != nil {
			return perr
		}
		return m.publishFleetMainNow(ctx, ri)
	}()
	if rerr := recordFleetAttempt(db, err, time.Now()); rerr != nil {
		log.Warn().Err(rerr).Msg("fleet create: recording the push attempt failed")
	}
	state := FleetRegistered
	if err != nil {
		log.Warn().Err(err).Str("fleet", ri.Name()).Msg("fleet create: publishing the fleet to its origin failed; the sync loop's next push publishes it again")
		state = FleetRegistering
	}
	if err := setFleetState(db, state, time.Now()); err != nil {
		log.Warn().Err(err).Str("state", state).Msg("fleet create: setting the fleet state failed")
	}
}

// publishFleetMainNow publishes the fleet repository's main as the agent
// branch's tip. With an origin, that is PublishUpstream (never forced) and
// then local main follows. Without one, local main fast-forwards. The caller
// holds the fleet root: only the master writes main.
func (m *Manager) publishFleetMainNow(ctx context.Context, ri *RepoInstance) error {
	var perr error
	if werr := ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			perr = errors.New("fleet repository is not open")
			return
		}
		if h := currentFleetHooks().beforePublish; h != nil {
			if perr = h(); perr != nil {
				return
			}
		}
		remote, err := svc.Remote().GetRemote("origin")
		if err != nil {
			perr = err
			return
		}
		if remote != nil {
			auth, err := makeRemoteAuthFn(m.deps.Cfg.Remote, m.deps.KeyPath)(remote)
			if err != nil {
				perr = err
				return
			}
			_, perr = publishFleetMainOn(ctx, svc, ri.AgentBranch(), auth)
			return
		}
		if _, err := svc.AdvanceLocalUpstream(ctx, ri.AgentBranch(), svc.UpstreamBranch()); err != nil {
			perr = err
		}
	}); werr != nil && perr == nil {
		perr = werr
	}
	return perr
}

// publishFleetMainOn is THE publish of the fleet repository's main on a
// forge, shared by every caller (create's step 7 and FleetRetry's push,
// through publishFleetMainNow, and the sync loop's round, fleetPeers.publish):
// PublishUpstream (main = from's tip, never forced), then local main
// fast-forwarded to from. A local main that cannot follow yet is only
// warned: main is on the forge, and the next fetch brings it here.
func publishFleetMainOn(ctx context.Context, svc *store.Service, from string, auth transport.AuthMethod) (store.PushResult, error) {
	res, err := svc.Remote().PublishUpstream(ctx, from, auth)
	if err != nil {
		return res, fmt.Errorf("publish main: %w", err)
	}
	if _, err := svc.AdvanceLocalUpstream(ctx, from, svc.UpstreamBranch()); err != nil {
		log.Warn().Err(err).Str("from", from).Msg("fleet: local main did not follow the published main yet")
	}
	return res, nil
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
