package repos

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/rs/zerolog/log"

	"knomit/internal/fact"
	"knomit/internal/pki"
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
//     restore and a swapped store. Only a mount opened by the fleet code
//     (CreateSpec.fleetDoor) or a registered repo mounted at boot passes
//     (RepoInstance.fleetDoor). A store swap is refused earlier still, in
//     guardSwap, before its point of no return.
//
// Once mounted, the fleet repository is read-only for MCP and the fact REST
// routes (RepoInstance.WritableBranch), may not be a lens's write repo
// (ErrLensWriteFleet), and is never archived or deleted through the generic
// doors (guardFleetRemoval).

// ErrFleetOntologyReserved refuses a fleet-id ontology at a generic door.
var ErrFleetOntologyReserved = errors.New("the ontology id \"fleet\" is reserved for the fleet repository, which only `knomit fleet` creates or joins")

// ErrFleetReadOnly is the one answer to an authored write (MCP, the fact
// REST routes, an experiment) on the fleet repository.
var ErrFleetReadOnly = errors.New("the fleet repository changes only through `knomit fleet`")

// CauseFleetConflict: more than one registered repository has the ontology id
// `fleet` (possible only by hand-editing control.db or from an older build).
// Boot keeps the first by name and leaves every other one unmounted and
// listed unavailable with this cause; nothing is archived. Reason
// "unopenable".
const CauseFleetConflict UnavailableCause = "fleet_conflict"

// fleetRootDirName is the fleet root directory under the knomit home: the
// master's root key, root certificate, CRL and issuance log.
const fleetRootDirName = "fleet"

// fleetRootDir is <home>/fleet.
func (m *Manager) fleetRootDir() string {
	return filepath.Join(m.deps.Cfg.Home, fleetRootDirName)
}

// isFleetMaster reports whether this instance is the fleet's master: the
// fleet root key file is in <home>/fleet. Any stat error other than "it is a
// file" answers no, so an unreadable home never makes an instance the master.
func (m *Manager) isFleetMaster() bool {
	if m == nil || m.deps.Cfg.Home == "" {
		return false
	}
	st, err := os.Stat(filepath.Join(m.fleetRootDir(), pki.RootKeyFile))
	return err == nil && st.Mode().IsRegular()
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
// ontology is admitted only through the fleet door or at boot.
func admitFleetOntology(r *RepoInstance, ont *fact.Ontology) error {
	if !fact.IsFleetOntology(ont) || r.fleetDoor {
		return nil
	}
	return fmt.Errorf("%w: repo %q holds a fleet repository; it is not mounted", ErrFleetOntologyReserved, r.Name())
}

// leaveRivalFleetsUnmounted runs once in Start after every mount replied:
// when more than one mounted repo is a fleet repository, the first by name
// stays and every other one is unmounted and listed unavailable
// (CauseFleetConflict). Nothing is archived: the operator decides.
func (m *Manager) leaveRivalFleetsUnmounted() {
	var fleets []string
	for _, name := range m.Names() {
		if ri := m.Get(name); ri != nil && ri.IsFleet() {
			fleets = append(fleets, name)
		}
	}
	if len(fleets) < 2 {
		return
	}
	sort.Strings(fleets)
	keep := fleets[0]
	detail := fmt.Sprintf("more than one repository with ontology id \"fleet\" is registered (%s); an instance has one fleet repository, so %q is mounted and this one is not",
		strings.Join(fleets, ", "), keep)
	reg := m.Repos()
	for _, name := range fleets[1:] {
		ri := m.Get(name)
		if ri == nil {
			continue
		}
		rec := RepoRecord{UID: ri.UID(), Name: name}
		if reg != nil {
			if r, ok, err := reg.Get(ri.UID()); err == nil && ok {
				rec = r
			}
		}
		m.Remove(name)
		m.markUnavailableCause(rec, "unopenable", CauseFleetConflict, detail)
	}
	log.Warn().Str("kept", keep).Strs("fleets", fleets).Msg("fleet conflict: one fleet repository mounted, the others left unmounted")
}
