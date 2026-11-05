package repos

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/rs/zerolog/log"

	"knomit/internal/corpus/repos/control"
	"knomit/internal/domain/fact"
	"knomit/internal/engine/store"
	"knomit/internal/foundation/pki"
)

// The fleet is a product feature, not a template: its ontology is built into
// knomit (fact.FleetOntology), and the ontology id `fleet` is RESERVED. A
// fleet repository is mounted only by knomit's fleet code, and every generic
// door refuses a fleet-id ontology with ErrFleetOntologyReserved:
//
//   - CREATE, before anything is written, wherever the request names the
//     ontology: a preset by name, YAML (custom, or initialize), a template
//     (ResolveTemplate, mode template or initialize). refuseReservedOntology,
//     from CreatePreflight and again in Create.
//   - IDENTIFY, for every mount, where the ontology is read: clone, subscribe,
//     restore, a swapped store and boot. Only a mount opened by the fleet
//     code (CreateSpec.fleetDoor) or, at boot, THIS instance's recorded
//     fleet repository (control.db instance_fleet_repo, written by
//     CreateFleet and JoinFleet) passes (RepoInstance.fleetDoor). Any other registered
//     repo whose ontology is or became the fleet's (an upstream switch that
//     arrived by sync) is refused and listed unavailable with
//     CauseFleetConflict; it is not the fleet repository, so it archives and
//     purges like any other. A store swap is refused earlier still, in
//     guardSwap, before its point of no return.
//
// Once mounted, the fleet repository is read-only for MCP and the fact REST
// routes (RepoInstance.WritableBranch), may not be a lens's write repo
// (ErrLensWriteFleet), and is never archived or deleted through the generic
// doors (guardFleetRemoval).

// ErrFleetOntologyReserved refuses a fleet-id ontology at a generic door.
var ErrFleetOntologyReserved = errors.New("the ontology id \"fleet\" is reserved for the fleet repository, which only creating or joining a fleet makes (Fleet → Create or join a fleet)")

// ErrFleetReadOnly is the one answer to an authored write (MCP, the fact
// REST routes, an experiment) on the fleet repository.
var ErrFleetReadOnly = errors.New("the fleet repository changes only through the fleet's own actions (the Fleet page)")

// CauseFleetConflict: a registered repository holds an ontology with id
// `fleet` but is not this instance's recorded fleet repository (an upstream
// that switched its ontology, a second fleet from an older build or a
// hand-edited control.db). Boot leaves it unmounted and listed unavailable
// with this cause; nothing is archived, and archive and purge treat it as an
// ordinary repo. Reason "unopenable".
const CauseFleetConflict UnavailableCause = "fleet_conflict"

// fleetRootDir is <home>/fleet (pki.FleetRootDirName): the master's root
// key, root certificate, CRL and issuance log.
func (m *Manager) fleetRootDir() string {
	return filepath.Join(m.deps.Cfg.Home, pki.FleetRootDirName)
}

// isFleetMaster reports whether this instance is the master of the fleet
// whose repository svc is (user ruling 2026-10-16, "whoever the merge sets in
// main, that is the true master"): it holds the fleet's root
// (holdsFleetRoot) AND the master marker on the fleet repository's main
// (fact.FleetMasterPath) names this instance. A root holder that main does
// not name is a member: a CANDIDATE while main names no master (it may claim,
// claimFleetMaster), demoted while main names another instance. Every error
// answers no, so an unreadable home or main never makes an instance the
// master.
func (m *Manager) isFleetMaster(svc *store.Service) bool {
	if svc == nil || !m.holdsFleetRoot(svc) {
		return false
	}
	named, err := fleetMasterOn(svc, svc.Remote().UpstreamBranch())
	return err == nil && named != "" && named == m.AgentID()
}

// holdsFleetRoot reports whether this instance holds the root of the fleet
// whose repository svc is: the fleet root key file is in <home>/fleet AND the
// root certificate beside it is THIS fleet's root, the one
// artifacts/fleet/root.crt holds on the fleet repository's main. A key file
// alone is not enough: a member that holds a stray root.key (another fleet's,
// an undecommissioned one) holds nothing. The key is never read or opened
// here. Holding the root is not being the master (isFleetMaster): main must
// name this instance too.
func (m *Manager) holdsFleetRoot(svc *store.Service) bool {
	if svc == nil {
		return false
	}
	return m.holdsFleetRootAt(svc, svc.Remote().UpstreamBranch())
}

// fleetMasterOn is the agent id the master marker at branch names, "" when
// there is none.
func fleetMasterOn(svc *store.Service, branch string) (string, error) {
	raw, ok, err := svc.FileAt(branch, fact.FleetMasterPath, maxFleetMasterSize)
	if err != nil || !ok {
		return "", err
	}
	return fact.ParseFleetMaster(raw)
}

// holdsFleetRootAt is isFleetMaster's test against any branch of the fleet
// repository: the root key file is in <home>/fleet AND root.crt beside it is
// the root that artifacts/fleet/root.crt holds at branch.
// RepoInstance.publishesFleetMain asks it of the agent branch, before main
// has ever been published.
func (m *Manager) holdsFleetRootAt(svc *store.Service, branch string) bool {
	if m == nil || m.deps.Cfg.Home == "" || svc == nil || branch == "" {
		return false
	}
	st, err := os.Stat(filepath.Join(m.fleetRootDir(), pki.RootKeyFile))
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	local, err := pki.LoadRootCert(filepath.Join(m.fleetRootDir(), pki.RootCertFile))
	if err != nil {
		return false
	}
	f, err := svc.FleetAt(branch)
	if err != nil || f.PKI == nil || f.PKI.Root == nil {
		return false
	}
	return local.Equal(f.PKI.Root)
}

// refuseReservedOntology is the create half for an ontology the request
// names before any I/O: a preset by name, or YAML (custom, initialize). YAML
// that does not parse is not this rule's business: the create reports that
// itself. A template is refused by ResolveTemplate, a clone or subscribe at
// Identify.
func refuseReservedOntology(spec CreateSpec) error {
	if spec.fleetDoor {
		return nil
	}
	if spec.OntologyPreset == fact.FleetOntologyID {
		return fmt.Errorf("%w: ontology_preset %q", ErrFleetOntologyReserved, spec.OntologyPreset)
	}
	if spec.OntologyYAML != "" && (spec.Mode == "custom" || spec.Mode == "initialize") {
		if o, err := fact.ParseOntology([]byte(spec.OntologyYAML)); err == nil && fact.IsFleetOntology(o) {
			return fmt.Errorf("%w: ontology_yaml has id %q", ErrFleetOntologyReserved, o.ID)
		}
	}
	return nil
}

// retireFleetConflict is the live half of admitFleetOntology (#429, user
// decision 2): a mounted repo that is not the fleet repository, whose read
// branch's consensus ontology took the fleet id, is unmounted, then leaves
// the live maps and is listed unavailable with CauseFleetConflict — the
// state a mount of it gives (Start: mountExisting fails at Identify,
// unavailableReason maps it). It runs on its own goroutine (reloadOntology
// starts it through startFleetConflictRetire): the unmount drains the Sync
// stage, and the reload may be running inside that stage.
//
// ORDER: UNMOUNT FIRST, PUBLISH SECOND. The repo is listed unavailable only
// once its machine is closed and its store with it. Listing it first (as
// this function did at bf1fe514) opened a window in which the repo looked
// gone while its store was still open: an operator acting on the listing —
// a remount, a restore, a purge — then raced the teardown. The old store's
// Close deletes its session DB files (Service.Close), so a fresh store.Open
// of the same database in that window failed with "init session schema:
// disk I/O error: no such file or directory" (CI run 42032962603), or
// opened and then lost its session DB. Until the unmount completes the repo
// stays in the live maps, unwritable (OntologyError) and winding down. The
// two map changes then happen under one lock, so GET /repos never shows it
// both absent and not unavailable.
//
// A repo the maps no longer hold as ri (archived, removed, replaced
// meanwhile) is left alone, before the unmount and again after it.
func (m *Manager) retireFleetConflict(ri *RepoInstance, err error) {
	rec := control.RepoRecord{UID: ri.uid, Name: ri.Name()}
	if reg := m.Repos(); reg != nil {
		if got, ok, gerr := reg.Get(ri.uid); gerr == nil && ok {
			rec = got
		}
	}
	reason, cause, detail := unavailableReason(&StageError{Stage: StageIdentify, Err: err})
	m.mu.Lock()
	if m.repos[rec.Name] != ri {
		m.mu.Unlock()
		return
	}
	// A later reload may already have applied a FIXED tip (a usable,
	// non-fleet ontology) before this goroutine ran: then the repo is not a
	// fleet conflict any more and stays mounted. Checked under m.mu, against
	// the identity as it stands now; a reload that lands after this check
	// meets an unmounting repo, which is what the conflict it saw called for.
	if !errors.Is(ri.OntologyError(), ErrFleetOntologyReserved) {
		m.mu.Unlock()
		ri.fleetConflictRetired.Store(false)
		log.Info().Str("repo", rec.Name).Msg("fleet conflict cleared by a later reload before the retire ran; the repo stays mounted")
		return
	}
	m.mu.Unlock()
	// Bounded like Close's own Unmounts (unmountDrainBound): Close waits for
	// this goroutine (retireWg), so an unbounded drain — a Sync round parked
	// on a slow fetch, a stage that will not let go — would hold Close past
	// the cap it promises for every other repo. A drain that runs out is
	// logged, as Close logs its own; the machine's root context is already
	// cancelled by the Send, so it still winds down (and is then listed
	// unavailable anyway: it serves nothing).
	if ri.machine != nil {
		bound := ri.machine.opts.unmountDrainBound()
		ctx, cancel := context.WithTimeout(context.Background(), bound)
		defer cancel()
		if _, uerr := ri.machine.Send(ctx, Unmount("fleet conflict")); uerr != nil {
			log.Error().Err(uerr).Str("repo", rec.Name).Dur("bound", bound).
				Msg("fleet conflict: repo did not finish unmounting within the drain bound; giving up waiting")
		}
	}
	m.mu.Lock()
	if m.repos[rec.Name] != ri {
		m.mu.Unlock()
		return
	}
	delete(m.repos, rec.Name)
	delete(m.byUID, ri.uid)
	m.unavailable[ri.uid] = Unavailable{Record: rec, Reason: reason, Detail: m.redactHome(detail), Cause: cause}
	m.mu.Unlock()
	log.Warn().Str("repo", rec.Name).Str("uid", rec.UID).Str("reason", reason).Str("cause", string(cause)).Str("detail", detail).
		Msg("repo is unavailable: its consensus ontology took the reserved fleet id (fleet_conflict); it is unmounted, as a mount of it would be refused")
}

// startFleetConflictRetire runs retireFleetConflict on its own goroutine,
// tracked by retireWg so Close waits for it. It reports whether it started
// one: never once Close has begun (retireClosing), when Close's own Unmount
// of every mounted repo covers this one. Add happens under m.mu, before Close
// can set retireClosing and Wait, so the WaitGroup is never Added to while
// it is being waited on.
func (m *Manager) startFleetConflictRetire(ri *RepoInstance, err error) bool {
	m.mu.Lock()
	if m.retireClosing {
		m.mu.Unlock()
		return false
	}
	m.retireWg.Add(1)
	m.mu.Unlock()
	hook := m.retireHook
	go func() {
		defer m.retireWg.Done()
		if hook != nil {
			hook()
		}
		m.retireFleetConflict(ri, err)
	}()
	return true
}

// admitFleetOntology is the Identify half: r's ontology is ont. A fleet-id
// ontology is admitted only through the fleet door or, at boot, for the
// recorded fleet repository.
func admitFleetOntology(r *RepoInstance, ont *fact.Ontology) error {
	if !fact.IsFleetOntology(ont) || r.fleetDoor {
		return nil
	}
	return fmt.Errorf("%w: repo %q holds a fleet repository; it is not mounted", ErrFleetOntologyReserved, r.Name())
}
