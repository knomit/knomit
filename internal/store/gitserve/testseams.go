package gitserve

import (
	"context"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/storer"

	"knomit/internal/store/internal/vcs"
	"knomit/internal/store/remotesync"
)

// The parent's white-box tests reach three internals of the handler: the
// object-set build counter, the pause point inside register, and the pieces
// the one-ref rules and the new-commit walk are built from. Each door below
// panics outside a test binary, so no production caller can take it.

func mustBeTest(name string) {
	if !testing.Testing() {
		panic("gitserve." + name + " called outside a test binary")
	}
}

// SetPushRegisterHookForTest installs f (nil clears it) as the hook that runs
// inside register after the old-hash check, on the uncancellable context.
func SetPushRegisterHookForTest(f func()) {
	mustBeTest("SetPushRegisterHookForTest")
	pushRegisterHook = f
}

// ObjectBuildsForTest is how many times a pack's object set has been built in
// this process (objectBuilds).
func ObjectBuildsForTest() int64 {
	mustBeTest("ObjectBuildsForTest")
	return objectBuilds.Load()
}

// NoPushPolicyTextForTest is the refusal text of a request that carries no
// push policy (errNoPushPolicy).
func NoPushPolicyTextForTest() string {
	mustBeTest("NoPushPolicyTextForTest")
	return errNoPushPolicy
}

// NewCommitsForTest puts objs into a quarantine over hostStore and runs the
// new-commit walk from tip, returning the new commits and how many commits the
// walk visited.
func NewCommitsForTest(hostStore storer.EncodedObjectStorer, objs []plumbing.EncodedObject, tip plumbing.Hash) ([]*object.Commit, int, error) {
	mustBeTest("NewCommitsForTest")
	q := newQuarantine(hostStore)
	for _, o := range objs {
		if _, err := q.SetEncodedObject(o); err != nil {
			return nil, 0, err
		}
	}
	return q.newCommits(tip, nil)
}

// JudgeRequestForTest runs the whole-request rules (one ref, the upstream and
// own-branch refusals, the agent/ prefix) of a receive-pack over r and sy.
func JudgeRequestForTest(ctx context.Context, r *vcs.Repo, sy *remotesync.Repo, upstream func() string, req *packp.ReferenceUpdateRequest) (*packp.Command, string) {
	mustBeTest("JudgeRequestForTest")
	rp := &receivePack{rh: &host{Repo: r, sy: sy}, upstream: upstream}
	return rp.judgeRequest(ctx, req)
}
