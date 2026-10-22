// Package fsck is the store's integrity checker: the report behind
// Checker.Verify and Checker.PruneGeneratedRefs (integrity.go). It checks git
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
// (Service.Integrity); the Service methods, the test-only tamper helpers and
// the public names stay in package store.
//
// Locking: Verify takes the read lock of every branch it checks up front and
// holds them for the whole run, so it blocks every writer on those branches
// (see "Verify" below).
//
// # Verify
//
// Verify reports structural integrity issues across the store: git object
// reachability, parity between SQLite tables and git/tree state, database-level
// soundness, and (with Deep) fact format.
//
// # WHICH BRANCHES ARE CHECKED
//
// Parity checks run ONLY for branches the index actually maintains, which is
// the set carrying a meta.last_commit:<branch> row. That is narrower than
// refs/heads/*, deliberately: repoBuilder.setupIndex maintains the agent branch
// plus upstreamMain and nothing else, so every other local ref — another
// machine's agent branch arriving by fetch, or a generated ref left by a
// removed feature — has no SQLite rows and never will. Demanding parity for
// those refs is demanding an invariant the system never promised; doing it
// produced 8128 errors on a healthy five-repo home, every one of them false.
// Unmaintained refs are reported once each, as warnings, so they stay visible
// without failing the run.
//
// # LOCKING
//
// Verify is read-only, and it is a SNAPSHOT. It acquires the read lock on every
// branch it is about to check UP FRONT and holds all of them for the whole run,
// so no writer can advance a ref or mutate branch_facts mid-report and no torn
// state can appear. The cost is the other side of that coin: for its whole
// duration Verify BLOCKS EVERY WRITER on every branch it covers, and a
// concurrent index Rebuild (which takes the write side of the same per-branch
// RWMutex) blocks Verify. It is not a cheap probe to fire at a live agent —
// budget for it, or take the repo offline.
package fsck
