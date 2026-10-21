package store

import (
	"context"

	"knomit/internal/store/facts"
)

// This file is the parent's seam to the facts child (internal/store/facts):
// reading and writing fact files, the system-file / template / skill readers
// over a commit's tree, and the trigger dispatcher's store half. The child
// reads a store only through *vcs.Repo (wrapped as facts.Repo) and verify.
//
// repoHandler embeds *facts.Repo next to *vcs.Repo (see vcs_seam.go), so the
// methods that used to be declared on repoHandler in the facts files
// (SkillsAt, TemplateFilesAt, SystemFileAt, the TriggerIndex methods, ...)
// are still reached as rh.<Name> by promotion, and the interface assertions
// below hold for *repoHandler as before. Embedding is not virtual: the child
// never calls a method declared on the parent's repoHandler.
//
// The names callers outside store use are re-exported below unchanged, so
// the public surface of package store does not move.

// factsRepo is the embedded facts.Repo; the alias gives the embedded field a
// name that does not collide with the embedded *vcs.Repo's `Repo`.
type factsRepo = facts.Repo

// Types.
type (
	CommitInfo          = facts.CommitInfo
	DirEntry            = facts.DirEntry
	DueCandidate        = facts.DueCandidate
	DueKey              = facts.DueKey
	DueMark             = facts.DueMark
	FactInvalidError    = facts.FactInvalidError
	FactRecord          = facts.FactRecord
	FactWithBody        = facts.FactWithBody
	ReadFactOpts        = facts.ReadFactOpts
	ReadFactResult      = facts.ReadFactResult
	RetractMissingError = facts.RetractMissingError
	SkillEntry          = facts.SkillEntry
	SkillIndex          = facts.SkillIndex
	SystemFile          = facts.SystemFile
	SystemFileIndex     = facts.SystemFileIndex
	TemplateIndex       = facts.TemplateIndex
	TouchResult         = facts.TouchResult
	TriggerFire         = facts.TriggerFire
	TriggerIndex        = facts.TriggerIndex
	TriggerRun          = facts.TriggerRun
	TriggerTrees        = facts.TriggerTrees
	WriteFactResult     = facts.WriteFactResult
)

// Constants.
const (
	RecipeSourceLocal           = facts.RecipeSourceLocal
	RecipeSourceRepo            = facts.RecipeSourceRepo
	TriggerFireRetention        = facts.TriggerFireRetention
	TriggerOutcomeBusy          = facts.TriggerOutcomeBusy
	TriggerOutcomeDelivered     = facts.TriggerOutcomeDelivered
	TriggerOutcomeDone          = facts.TriggerOutcomeDone
	TriggerOutcomeEmitted       = facts.TriggerOutcomeEmitted
	TriggerOutcomeIfError       = facts.TriggerOutcomeIfError
	TriggerOutcomeIfFalse       = facts.TriggerOutcomeIfFalse
	TriggerOutcomeIfTimeout     = facts.TriggerOutcomeIfTimeout
	TriggerOutcomeKicked        = facts.TriggerOutcomeKicked
	TriggerOutcomeRan           = facts.TriggerOutcomeRan
	TriggerOutcomeRateLimited   = facts.TriggerOutcomeRateLimited
	TriggerOutcomeRecipeError   = facts.TriggerOutcomeRecipeError
	TriggerOutcomeRecipeTimeout = facts.TriggerOutcomeRecipeTimeout
	TriggerOutcomeRun           = facts.TriggerOutcomeRun
	TriggerOutcomeScriptError   = facts.TriggerOutcomeScriptError
	TriggerOutcomeScriptTimeout = facts.TriggerOutcomeScriptTimeout
	TriggerOutcomeSpawned       = facts.TriggerOutcomeSpawned
	TriggerOutcomeStarted       = facts.TriggerOutcomeStarted
	TriggerOutcomeUnparseable   = facts.TriggerOutcomeUnparseable
	TriggerOutcomeUnreachable   = facts.TriggerOutcomeUnreachable
)

// Sentinel errors: the same values, so errors.Is matches across the seam.
var (
	ErrFactChanged        = facts.ErrFactChanged
	ErrFactInvalid        = facts.ErrFactInvalid
	ErrGuidanceUnusable   = facts.ErrGuidanceUnusable
	ErrInvalidPath        = facts.ErrInvalidPath
	ErrNoGuidanceAtCommit = facts.ErrNoGuidanceAtCommit
	ErrNoOntologyAtCommit = facts.ErrNoOntologyAtCommit
	ErrNoRecipeAtCommit   = facts.ErrNoRecipeAtCommit
	ErrNoScriptAtCommit   = facts.ErrNoScriptAtCommit
	ErrRepoReadOnly       = facts.ErrRepoReadOnly
	ErrSystemTreePath     = facts.ErrSystemTreePath
	ErrTemplateLayout     = facts.ErrTemplateLayout
	ErrTemplateNotFound   = facts.ErrTemplateNotFound
	ErrTemplateNotRegular = facts.ErrTemplateNotRegular
	ErrTemplateTooLarge   = facts.ErrTemplateTooLarge
)

// IsInvalidPath reports whether err is a refusal of the path itself (see
// facts.IsInvalidPath).
func IsInvalidPath(err error) bool { return facts.IsInvalidPath(err) }

// IsRecipeFailure reports whether a trigger fire outcome is a recipe failure.
func IsRecipeFailure(outcome string) bool { return facts.IsRecipeFailure(outcome) }

// UTCStamp renders Unix seconds as RFC 3339 UTC with an explicit Z.
func UTCStamp(unix int64) string { return facts.UTCStamp(unix) }

// WithCarriedOver marks ctx as carrying commits whose content was already
// validated at the source (see facts.WithCarriedOver).
func WithCarriedOver(ctx context.Context) context.Context { return facts.WithCarriedOver(ctx) }

// SetBatchWriteUnlockedHookForTest installs f as the pre-lock hook of every
// batch write in this process. The child's function panics outside a test
// binary.
func SetBatchWriteUnlockedHookForTest(f func()) (restore func()) {
	return facts.SetBatchWriteUnlockedHookForTest(f)
}
