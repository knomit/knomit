package store

import (
	"context"

	"knomit/internal/store/gitserve"
)

// This file is the parent's seam to the gitserve child (internal/store/gitserve):
// the smart-HTTP protocol handler for a store, upload-pack (the advertisement,
// negotiation and the pack) and receive-pack (the quarantine, the incoming-push
// judge, the all-or-nothing promote and the registration through the commit
// chokepoint). The child reads a store through the one shared *vcs.Repo, moves
// a pushed branch through the sync child's AdvanceBranchTo, and judges a push
// with verify; nothing in it reaches the Service. Service.Handler (service_gitserve.go)
// builds it once, from the handles repoHandler embeds.
//
// The names callers outside store use are re-exported below unchanged, so the
// public surface of package store does not move. repoHandler does not embed a
// gitserve handle: nothing in the parent calls a method of the child.

// PushPolicy is who is pushing and what the host may do about it (see
// gitserve.PushPolicy).
type PushPolicy = gitserve.PushPolicy

// WithPushPolicy attaches the per-request push policy to ctx (see
// gitserve.WithPushPolicy).
func WithPushPolicy(ctx context.Context, p PushPolicy) context.Context {
	return gitserve.WithPushPolicy(ctx, p)
}
