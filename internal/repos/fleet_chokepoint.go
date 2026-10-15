package repos

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"knomit/internal/fact"
	"knomit/internal/pki"
	"knomit/internal/store"
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
//     RegisterFleet) passes (RepoInstance.fleetDoor). Any other registered
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
var ErrFleetOntologyReserved = errors.New("the ontology id \"fleet\" is reserved for the fleet repository, which only the fleet API (/api/v1/fleet/...) creates or joins")

// ErrFleetReadOnly is the one answer to an authored write (MCP, the fact
// REST routes, an experiment) on the fleet repository.
var ErrFleetReadOnly = errors.New("the fleet repository changes only through the fleet API (/api/v1/fleet/..., the Fleet page)")

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
// whose repository svc is: the fleet root key file is in <home>/fleet AND the
// root certificate beside it is THIS fleet's root, the one
// artifacts/fleet/root.crt holds on the fleet repository's main. A key file
// alone is not enough: a member that holds a stray root.key (another fleet's,
// an undecommissioned one) is not the master. The key is never read or
// opened here. Every error answers no, so an unreadable home or main never
// makes an instance the master.
func (m *Manager) isFleetMaster(svc *store.Service) bool {
	if svc == nil {
		return false
	}
	return m.holdsFleetRootAt(svc, svc.UpstreamBranch())
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

// admitFleetOntology is the Identify half: r's ontology is ont. A fleet-id
// ontology is admitted only through the fleet door or, at boot, for the
// recorded fleet repository.
func admitFleetOntology(r *RepoInstance, ont *fact.Ontology) error {
	if !fact.IsFleetOntology(ont) || r.fleetDoor {
		return nil
	}
	return fmt.Errorf("%w: repo %q holds a fleet repository; it is not mounted", ErrFleetOntologyReserved, r.Name())
}
