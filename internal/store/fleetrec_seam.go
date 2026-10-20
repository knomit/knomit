package store

import "knomit/internal/store/fleetrec"

// This file is the parent's seam to the fleetrec child
// (internal/store/fleetrec): the fleet repository's member records, its
// certificate artifacts and the history of its main, read from one commit.
// The names callers outside store use are re-exported below unchanged, so the
// public surface of package store does not move. The verify cluster (still
// in this package) reaches the child's exported helpers directly.

// Types.
type (
	Fleet             = fleetrec.Fleet
	FleetMainCommit   = fleetrec.MainCommit
	FleetMember       = fleetrec.Member
	FleetPKI          = fleetrec.PKI
	FleetRecordChange = fleetrec.RecordChange
)

// ErrNotFleet: the repository's ontology is not the fleet ontology. The same
// value as the child's, so errors.Is matches across the seam.
var ErrNotFleet = fleetrec.ErrNotFleet

// LoadFleet opens the fleet repository at dir, checks that its ontology (at
// rev) is the fleet ontology, and returns the member records at rev. root is the
// repository's fact root ("" = "kb").
func LoadFleet(dir, rev, root string) ([]FleetMember, error) {
	return fleetrec.LoadFleet(dir, rev, root)
}

// LoadFleetAt is LoadFleet also returning the certificate artifacts at rev,
// read from the same commit.
func LoadFleetAt(dir, rev, root string) (Fleet, error) {
	return fleetrec.LoadFleetAt(dir, rev, root)
}
