package store

import (
	"context"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/rs/zerolog"

	"knomit/internal/store/verify"
)

// This file is the parent's seam to the verify child (internal/store/verify):
// the judge. Pure predicates over commits and trees, plus the acceptance
// gate (CheckRange), the audit, the incoming-push judge and the own-lineage
// check. The child reads a store only through *vcs.Repo and the fleet
// records through fleetrec; the writers that call it (the receive-pack handler
// in gitserve, the reconcile/sync paths and the own-range judge in remotesync)
// reach it directly. The names callers outside store use are re-exported below
// unchanged, so the public surface of package store does not move.

// Types.
type (
	AuditInput             = verify.AuditInput
	AuditReport            = verify.AuditReport
	CommitSigner           = verify.CommitSigner
	ForeignLineageError    = verify.ForeignLineageError
	RangeInput             = verify.RangeInput
	RangeVerdict           = verify.RangeVerdict
	Refusal                = verify.Refusal
	ValidationRefusedError = verify.ValidationRefusedError
)

// Constants.
const (
	AuditNeverMember          = verify.AuditNeverMember
	AuditOtherAgent           = verify.AuditOtherAgent
	AuditRevoked              = verify.AuditRevoked
	AuditUnsigned             = verify.AuditUnsigned
	CommitRefusedByJudge      = verify.CommitRefusedByJudge
	CommitRefusedByValidation = verify.CommitRefusedByValidation
	RuleAmbiguous             = verify.RuleAmbiguous
	RuleArtifactAppendOnly    = verify.RuleArtifactAppendOnly
	RuleArtifactNotAFile      = verify.RuleArtifactNotAFile
	RuleArtifactPKI           = verify.RuleArtifactPKI
	RuleArtifactReplace       = verify.RuleArtifactReplace
	RuleArtifactRules         = verify.RuleArtifactRules
	RuleArtifactSize          = verify.RuleArtifactSize
	RuleArtifactWriteOnce     = verify.RuleArtifactWriteOnce
	RuleDuplicateKey          = verify.RuleDuplicateKey
	RuleHostRecord            = verify.RuleHostRecord
	RuleInactive              = verify.RuleInactive
	RuleKeyMismatch           = verify.RuleKeyMismatch
	RuleMalformedTree         = verify.RuleMalformedTree
	RuleMerge                 = verify.RuleMerge
	RuleMergeConflict         = verify.RuleMergeConflict
	RuleNoCert                = verify.RuleNoCert
	RuleNoRecord              = verify.RuleNoRecord
	RuleNotOwner              = verify.RuleNotOwner
	RuleNotPusher             = verify.RuleNotPusher
	RuleRevoked               = verify.RuleRevoked
	RuleSignature             = verify.RuleSignature
	RuleValidation            = verify.RuleValidation
	VerifyEnforce             = verify.VerifyEnforce
	VerifyLog                 = verify.VerifyLog
	VerifyOff                 = verify.VerifyOff
)

// Sentinel errors: the same values, so errors.Is matches across the seam.
var (
	ErrBadSignature      = verify.ErrBadSignature
	ErrForeignLineage    = verify.ErrForeignLineage
	ErrMalformedSig      = verify.ErrMalformedSig
	ErrNoFleet           = verify.ErrNoFleet
	ErrNotEd25519Key     = verify.ErrNotEd25519Key
	ErrNotSSHSIG         = verify.ErrNotSSHSIG
	ErrUnsigned          = verify.ErrUnsigned
	ErrValidationRefused = verify.ErrValidationRefused
	ErrWrongHashAlgo     = verify.ErrWrongHashAlgo
	ErrWrongNamespace    = verify.ErrWrongNamespace
)

// Audit re-checks a whole branch on demand (see verify.Audit).
func Audit(in AuditInput) (AuditReport, error) { return verify.Audit(in) }

// CheckRange is the acceptance gate (see verify.CheckRange).
func CheckRange(in RangeInput) (RangeVerdict, error) { return verify.CheckRange(in) }

// The methods of the parent's repoHandler that reach the verify child. They
// stay declared here for the parent's own callers (Open/Clone in repo.go and
// the Service's JudgePeerBranch in service_sync.go); each passes the embedded
// *vcs.Repo, which is all the child reads.

func (rh *repoHandler) checkOwnLineage(ctx context.Context, branch string, agentTip, upstream plumbing.Hash) error {
	return verify.CheckOwnLineage(ctx, rh.Repo, branch, agentTip, upstream)
}

func (rh *repoHandler) judgeIncoming(ctx context.Context, upstream string, fleetOf func(context.Context) (Fleet, error), signer func(*object.Commit) (Refusal, bool), tip plumbing.Hash, commits []*object.Commit, lg zerolog.Logger) string {
	return verify.JudgeIncoming(ctx, rh.Repo, upstream, fleetOf, signer, tip, commits, lg)
}

func (rh *repoHandler) privatePathClosure(ctx context.Context, st storer.EncodedObjectStorer, upstream string, commits []*object.Commit) string {
	return verify.PrivatePathClosure(ctx, rh.Repo, st, upstream, commits)
}
