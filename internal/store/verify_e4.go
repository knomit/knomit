package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// ErrForeignLineage is E4's refusal: origin's copy of this instance's agent
// branch holds commits, beyond the verified upstream, that this instance did
// not sign. Match with errors.Is; errors.As a *ForeignLineageError for the
// commits.
var ErrForeignLineage = errors.New("origin's copy of this instance's agent branch holds commits this instance did not sign")

// ForeignLineageError lists what E4 refused and how to proceed. E4 never
// silently discards commits: the create FAILS, no local agent branch is made,
// and the remote branch is left exactly as it is.
type ForeignLineageError struct {
	Branch  string
	Refused []string // "<short hash>: <reason>"
}

func (e *ForeignLineageError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: origin's %s holds %d commit(s) not signed by this instance, and they are not on the verified upstream:",
		ErrForeignLineage.Error(), e.Branch, len(e.Refused))
	for _, r := range e.Refused {
		b.WriteString("\n  " + r)
	}
	b.WriteString("\nNothing was discarded and no local agent branch was created. Either merge " + e.Branch +
		" into the upstream on the forge and clone again, or, if these commits are yours from before signing, " +
		"accept each one on this instance (knomit verify accept <commit>) and clone again.")
	return b.String()
}

func (e *ForeignLineageError) Is(target error) bool { return target == ErrForeignLineage }

// checkOwnLineage is F09's E4 check, run at clone time in EVERY mode (off
// included): the input an attacker cannot choose. Every commit reachable from
// agentTip and not from upstream (the VERIFIED upstream, so refused upstream
// commits merged into the branch count as foreign) must carry a valid SSHSIG
// by this instance's own key, compared by full fingerprint, or be on this
// instance's accept list. A store that has no signer cannot know its own key:
// every such commit is refused.
func (rh *repoHandler) checkOwnLineage(ctx context.Context, branch string, agentTip, upstream plumbing.Hash) error {
	own := ""
	if rh.signer != nil {
		fp, err := keyFingerprint(rh.signer.PublicKey())
		if err != nil {
			return fmt.Errorf("E4: own key: %w", err)
		}
		own = fp
	}
	v := rh.verifier(ctx)
	below := map[plumbing.Hash]bool{}
	if upstream != plumbing.ZeroHash {
		if err := v.walk(upstream, nil, func(c *object.Commit) { below[c.Hash] = true }); err != nil {
			return err
		}
	}
	var refused []string
	err := v.walk(agentTip, below, func(c *object.Commit) {
		if v.accepted != nil && v.accepted(c.Hash) {
			return
		}
		s, serr := verifyCommitSignature(c)
		switch {
		case own == "":
			refused = append(refused, shortRefHash(c.Hash)+": this store has no signer to know its own key")
		case serr != nil:
			refused = append(refused, shortRefHash(c.Hash)+": "+serr.Error())
		case s.Fingerprint != own:
			refused = append(refused, shortRefHash(c.Hash)+": signed by "+s.Fingerprint[:8])
		}
	})
	if err != nil {
		return err
	}
	if len(refused) > 0 {
		return &ForeignLineageError{Branch: branch, Refused: refused}
	}
	return nil
}
