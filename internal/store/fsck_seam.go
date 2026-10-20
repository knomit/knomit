package store

import "knomit/internal/store/fsck"

// This file is the parent's seam to the fsck child (internal/store/fsck): the
// integrity checker behind Service.Verify and Service.PruneGeneratedRefs (git
// object reachability, SQLite-vs-git parity, database soundness, fact format,
// the embedding model identity). The child reads a store through the one
// shared *vcs.Repo and the facts and index sub-indices; nothing in it reaches
// the Service. Service.integrity (service_fsck.go) builds one per call.
//
// The names callers outside store use are re-exported below unchanged, so the
// public surface of package store does not move. repoHandler embeds no fsck
// handle: nothing in the parent calls a method of the child.

// Types.
type (
	IntegrityIssue  = fsck.IntegrityIssue
	IntegrityReport = fsck.IntegrityReport
	PruneResult     = fsck.PruneResult
	Severity        = fsck.Severity
	VerifyOpts      = fsck.VerifyOpts
)

// Constants.
const (
	CategoryBranchesTable      = fsck.CategoryBranchesTable
	CategoryCommitLog          = fsck.CategoryCommitLog
	CategoryCommitParents      = fsck.CategoryCommitParents
	CategoryDatabase           = fsck.CategoryDatabase
	CategoryDerivedTables      = fsck.CategoryDerivedTables
	CategoryEmbeddingIdentity  = fsck.CategoryEmbeddingIdentity
	CategoryEmbeddingsCoverage = fsck.CategoryEmbeddingsCoverage
	CategoryFactFormat         = fsck.CategoryFactFormat
	CategoryFactsCoherence     = fsck.CategoryFactsCoherence
	CategoryGeneratedRefs      = fsck.CategoryGeneratedRefs
	CategoryGitReachability    = fsck.CategoryGitReachability
	CategoryGraphCoherence     = fsck.CategoryGraphCoherence
	CategoryOrphanObjects      = fsck.CategoryOrphanObjects
	CategorySchemaVersion      = fsck.CategorySchemaVersion
	SeverityError              = fsck.SeverityError
	SeverityWarning            = fsck.SeverityWarning
)
