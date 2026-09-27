package store

import (
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"golang.org/x/crypto/ssh"

	storegit "knomit/internal/store/git"
)

// ErrNoSigner is returned by every authored write on a Service that has no
// commit signer. It used to be a silent no-op: a Service opened without
// SetSigner committed UNSIGNED, and those commits were pushed as this
// instance's agent branch (the origin wizard's replay store and the
// initialize flow did exactly that). Under F09 an unsigned commit is refused
// by every verifying peer, so the store refuses to make one.
//
// The init commits of a new repository (README and ontology, repo.go) are not
// affected: they are written by the plumbing layer, which never signs. They
// precede any agent-branch work and lie below every F09 anchor, so they are
// never verified.
var ErrNoSigner = errors.New("store has no commit signer: refusing to write an unsigned commit")

// commitSigner returns the signer an AUTHORED commit must carry: this store's
// signer, else (inside a test binary only) the test fallback, else
// ErrNoSigner. Callers ask for it BEFORE writing any object, so a refused write
// leaves nothing behind in the object store.
func (rh *repoHandler) commitSigner() (ssh.Signer, error) {
	if rh.signer != nil {
		return rh.signer, nil
	}
	if fb := testFallbackSigner(); fb != nil {
		return fb, nil
	}
	return nil, ErrNoSigner
}

// storeCommit encodes c and stores it as exactly ONE object. When signer is
// non-nil the commit is signed first, in memory, over the payload without the
// signature header (EncodeWithoutSignature), so no unsigned pre-image is left
// unreachable in the store — the old sign-after-store path left one orphan per
// write. A nil signer stores the commit unsigned: only the init commits of a
// new repository do that (plumbing, repo.go), by design.
func storeCommit(s *storegit.Storer, signer ssh.Signer, c *object.Commit) (plumbing.Hash, error) {
	if signer != nil {
		payloadObj := s.NewEncodedObject()
		if err := c.EncodeWithoutSignature(payloadObj); err != nil {
			return plumbing.ZeroHash, fmt.Errorf("storeCommit: encode payload: %w", err)
		}
		reader, err := payloadObj.Reader()
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("storeCommit: payload reader: %w", err)
		}
		payload, err := io.ReadAll(reader)
		reader.Close()
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("storeCommit: read payload: %w", err)
		}
		signature, err := signCommit(signer, payload)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("storeCommit: sign: %w", err)
		}
		c.PGPSignature = signature
	}
	obj := s.NewEncodedObject()
	if err := c.Encode(obj); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("storeCommit: encode: %w", err)
	}
	h, err := s.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("storeCommit: store: %w", err)
	}
	return h, nil
}

// testFallback holds the signer SetTestFallbackSigner installed. It is read
// only inside a test binary; see testFallbackSigner.
var testFallback atomic.Pointer[ssh.Signer]

// SetTestFallbackSigner installs the signer a signer-less Service uses INSIDE
// A GO TEST BINARY, so the hundreds of test fixtures that open a store without
// SetSigner keep working after ErrNoSigner. Each test package installs it from
// its TestMain (internal/testsupport/testsigner). Pass nil to remove it.
//
// It panics outside a test binary (testing.Testing), and commitSigner
// ignores it there too, so a production knomit can never sign with it: a
// Service without a signer is ErrNoSigner, always. A test that must prove a
// path wires the REAL signer asserts the key in the signature, which the
// fallback key would not match.
func SetTestFallbackSigner(signer ssh.Signer) {
	if !testing.Testing() {
		panic("store.SetTestFallbackSigner called outside a test binary")
	}
	if signer == nil {
		testFallback.Store(nil)
		return
	}
	testFallback.Store(&signer)
}

func testFallbackSigner() ssh.Signer {
	if !testing.Testing() {
		return nil
	}
	if p := testFallback.Load(); p != nil {
		return *p
	}
	return nil
}
