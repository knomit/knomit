package store

import (
	"errors"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// ErrForeignLineage is E4's refusal: origin's copy of this instance's agent
// branch holds a commit, beyond the verified upstream, that this instance did
// not sign.
var ErrForeignLineage = errors.New("origin's copy of this instance's agent branch holds a commit this instance did not sign")

// checkOwnLineage is F09's E4 check, run at clone time in EVERY mode (off
// included): the input an attacker cannot choose. Every commit reachable from
// agentTip and not from upstream (the VERIFIED upstream, so refused upstream
// commits merged into the branch count as foreign) must carry a valid SSHSIG
// by this instance's own key, compared by full fingerprint. A store that has
// no signer cannot know its own key and refuses adoption rather than adopting
// blindly.
func (rh *repoHandler) checkOwnLineage(agentTip, upstream plumbing.Hash) error {
	if rh.signer == nil {
		return fmt.Errorf("%w: no signer on this store to know its own key", ErrForeignLineage)
	}
	own, err := keyFingerprint(rh.signer.PublicKey())
	if err != nil {
		return fmt.Errorf("E4: own key: %w", err)
	}
	v := &verifier{st: rh.gits}
	below := map[plumbing.Hash]bool{}
	if upstream != plumbing.ZeroHash {
		if err := v.walk(upstream, nil, func(c *object.Commit) { below[c.Hash] = true }); err != nil {
			return err
		}
	}
	var bad error
	err = v.walk(agentTip, below, func(c *object.Commit) {
		if bad != nil {
			return
		}
		s, serr := verifyCommitSignature(c)
		switch {
		case serr != nil:
			bad = fmt.Errorf("%w: %s: %v", ErrForeignLineage, shortRefHash(c.Hash), serr)
		case s.Fingerprint != own:
			bad = fmt.Errorf("%w: %s is signed by %s", ErrForeignLineage, shortRefHash(c.Hash), s.Fingerprint[:8])
		}
	})
	if err != nil {
		return err
	}
	return bad
}
