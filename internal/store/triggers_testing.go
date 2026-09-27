package store

import (
	"fmt"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
)

// TestingSetRef moves (or creates) a git reference WITHOUT a commit and without
// notifyCommit — for tests in sibling packages that need a ref shape the write
// path never produces on demand: an agent branch rewound so its old head is no
// longer an ancestor (the replay case), or F09's verified anchor
// (refs/knomit/verified/<upstream>) without running a fold.
//
// Test-only by contract, like SetTestFallbackSigner: it panics outside a test
// binary, so no production path can move a ref behind the store's back.
func (s *Service) TestingSetRef(name, hash string) error {
	if !testing.Testing() {
		panic("store.TestingSetRef called outside a test binary")
	}
	if !plumbing.IsHash(hash) {
		return fmt.Errorf("TestingSetRef: %q is not a commit hash", hash)
	}
	return s.rh.gits.SetReference(plumbing.NewHashReference(plumbing.ReferenceName(name), plumbing.NewHash(hash)))
}

// VerifiedRefName is the F09 anchor ref for upstream, exported for the tests
// that set it through TestingSetRef.
func VerifiedRefName(upstream string) string { return verifiedRefName(upstream).String() }
