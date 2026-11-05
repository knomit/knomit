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
// ORDER: UNMOUNT FIRST, PUBLISH ONLY ON A CLOSED STORE. The repo is listed
// unavailable only once its unmount has COMPLETED — Send returned nil, which
// means every stage exited and the store is closed. Listing it before that
// (as this function did at bf1fe514) opened a window in which the repo
// looked gone while its store was still open: an operator acting on the
// listing — a remount, a restore, a purge — raced the teardown. The old
// store's Close deletes its session DB files (Service.Close), so a fresh
// store.Open of the same database in that window failed with "init session
// schema: disk I/O error: no such file or directory" (CI run 42032962603),
// or opened and then lost its session DB. Until the unmount completes the
// repo stays in the live maps, unwritable (OntologyError) and winding down.
//
// A DRAIN THAT RUNS OUT (the Send is bounded by unmountDrainBound, like
// Close's own Unmounts, because Close waits for this goroutine) publishes
// NOTHING: the store may still be open, so the repo stays in the live maps,
// unwritable, its machine's root context cancelled, and the timeout is
// logged at Error. Close's own bounded Unmount still covers it (it is in
// m.repos), and the next mount refuses it with fleet_conflict as always.
//
// THE PUBLISH re-checks the instance by UID, not by name: RenameRepo only
// moves map and registry entries, so it can succeed while the unmount runs,
// and a name-keyed re-check would then skip the publish and strand the
// repo live under its new name with a closed machine. Archive and Remove
// drop the instance from BOTH maps, so they are still skipped. The entry is
// deleted and published under the instance's CURRENT name, in one critical
// section, so GET /repos never shows it both absent and not unavailable.
func (m *Manager) retireFleetConflict(ri *RepoInstance, err error) {
	reason, cause, detail := unavailableReason(&StageError{Stage: StageIdentify, Err: err})
	m.mu.Lock()
	if m.byUID[ri.uid] != ri {
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
		log.Info().Str("repo", ri.Name()).Msg("fleet conflict cleared by a later reload before the retire ran; the repo stays mounted")
		return
	}
	m.mu.Unlock()
	if ri.machine != nil {
		bound := ri.machine.opts.unmountDrainBound()
		ctx, cancel := context.WithTimeout(context.Background(), bound)
		defer cancel()
		if _, uerr := ri.machine.Send(ctx, Unmount("fleet conflict")); uerr != nil {
			log.Error().Err(uerr).Str("repo", ri.Name()).Str("uid", ri.uid).Dur("bound", bound).
				Msg("fleet conflict: the repo did not finish unmounting within the drain bound; its store may still be open, so it is NOT listed unavailable: it stays mounted, unwritable, winding down, and its next mount refuses it (fleet_conflict)")
			return
		}
	}
	// The registry row, read now: it carries the name a rename gave it
	// during the unmount. Falls back to what the instance knows.
	rec := control.RepoRecord{UID: ri.uid, Name: ri.Name()}
	if reg := m.Repos(); reg != nil {
		if got, ok, gerr := reg.Get(ri.uid); gerr == nil && ok {
			rec = got
		}
	}
	m.mu.Lock()
	if m.byUID[ri.uid] != ri {
		m.mu.Unlock()
		return
	}
	rec.Name = ri.Name()
	if m.repos[rec.Name] == ri {
		delete(m.repos, rec.Name)
	}
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
