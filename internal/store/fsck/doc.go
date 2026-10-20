// Package fsck is the store's integrity checker: the report behind
// Service.Verify and Service.PruneGeneratedRefs (integrity.go). It checks git
// object reachability, the SQLite commit log and branch rows against the git
// state, facts and graph coherence, the embeddings' coverage and the embedding
// model identity, the derived tables, database soundness and the schema
// version, the orphan-objects warning, the generated-refs residue and (with
// Deep) fact format. It is read-only except PruneGeneratedRefs, which the
// operator asks for.
//
// It sits at the top of the store's child order ("everything above"): it reads
// the one shared vcs handle and the facts and index sub-indices, and nothing
// below it imports it. The Checker is built once per call by the parent
// (Service.integrity); the Service methods, the test-only tamper helpers and
// the public names (re-exported by fsck_seam.go) stay in package store.
//
// Locking: Verify takes the read lock of every branch it checks up front and
// holds them for the whole run, so it blocks every writer on those branches
// (see the header comment of integrity.go).
package fsck
