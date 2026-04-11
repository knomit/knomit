package testenv

// Snapshot is a named (branch, commit) pair. Returned by every BranchHandle
// mutation method. The Name field is auto-generated as "C1", "C2", ...
// unless overridden via WriteAs / UpdateAs / DeleteAs.
//
// Snapshots are the DSL's handle for reading a fact at a specific historical
// commit: bh.At(snap).Fact(path) reads that path as it was at snap.Commit.
// They are the core vocabulary of the temporal graph traversal tests in
// Category C.
type Snapshot struct {
	// Name is a human-readable label, either auto-generated (C1, C2, ...) or
	// caller-assigned via WriteAs.
	Name string
	// Commit is the git commit hash produced by this mutation.
	Commit string
	// Branch is the BranchHandle this snapshot was produced against.
	Branch *BranchHandle
}

// CommitHash returns the git commit hash for this snapshot.
func (s *Snapshot) CommitHash() string { return s.Commit }

// Fact returns a FactHandle for path resolved at this snapshot's commit.
// The handle carries state (Exists / Missing / Broken / External) so
// callers can branch on it explicitly via MustExist / MustNotExist etc.
func (s *Snapshot) Fact(path string) *FactHandle {
	return resolveFactAtCommit(s.Branch.repo.sb.t, s.Branch, s.Commit, path)
}
