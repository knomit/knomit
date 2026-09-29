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
// branch holds commits, beyond origin's main, that this instance did
// not sign. Match with errors.Is; errors.As a *ForeignLineageError for the
// commits.
var ErrForeignLineage = errors.New("origin's copy of this instance's agent branch holds commits this instance did not sign")

// ForeignLineageError lists what E4 refused and how to proceed. E4 never
// silently discards commits: the create FAILS, no local agent branch is made,
// and the remote branch is left exactly as it is.
type ForeignLineageError struct {
	Branch  string
	Refused []string // "<short hash>: <reason>"
	Commits []string // the refused commits' full hashes, in the same order
}

func (e *ForeignLineageError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: origin's %s holds %d commit(s) not signed by this instance, beyond origin's main:",
		ErrForeignLineage.Error(), e.Branch, len(e.Refused))
	for _, r := range e.Refused {
		b.WriteString("\n  " + r)
	}
	b.WriteString("\nNothing was discarded and no local agent branch was created. Merge " + e.Branch +
		" into the upstream on the forge and clone again; or, if these commits are yours from before signing, " +
		"accept each one on this instance and clone again:")
	for _, c := range e.Commits {
		b.WriteString("\n  knomit verify accept " + c)
	}
	return b.String()
}

func (e *ForeignLineageError) Is(target error) bool { return target == ErrForeignLineage }

// checkOwnLineage is F09's E4 check, run at clone time in EVERY mode (off
// included): the input an attacker cannot choose. Every commit reachable from
// agentTip and not from upstream (origin's main, trusted since it was accepted
// at its own gate) must carry a valid SSHSIG
// by this instance's own key, compared by full fingerprint, be on this
// instance's accept list, or have been brought in by an own-signed merge
// commit (reachable from one of its non-first parents: the transitive trust
// of a peer merge made in the web UI). A store that has no signer cannot know
// its own key: every such commit is refused.
func (rh *repoHandler) checkOwnLineage(ctx context.Context, branch string, agentTip, upstream plumbing.Hash) error {
	// The own key is the key this store signs with (commitSigner: the store's
	// signer, or a test binary's fallback). No signer means no own key.
	own := map[string]bool{}
	if signer, err := rh.commitSigner(); err == nil {
		fp, err := keyFingerprint(signer.PublicKey())
		if err != nil {
			return fmt.Errorf("E4: own key: %w", err)
		}
		own[fp] = true
	}
	// Registered in a fleet: every key this instance's own member record has
	// held (its versions) is also its own, so a key rotation does not make the
	// instance's earlier commits foreign.
	if rh.ownKeys != nil {
		for _, k := range rh.ownKeys() {
			if fp, err := keyFingerprint(k); err == nil {
				own[fp] = true
			}
		}
	}
	below := map[plumbing.Hash]bool{}
	if upstream != plumbing.ZeroHash {
		if err := walkHistory(rh.gits, upstream, nil, func(c *object.Commit) { below[c.Hash] = true }); err != nil {
			return err
		}
	}
	// COLLECT first, JUDGE second. walkHistory is post-order — a commit is
	// visited after its parents — so judging inside the visit would reach a
	// merged-in commit before the own merge that vouches for it.
	type seenCommit struct {
		c      *object.Commit
		reason string // "" = own-signed or accepted
		ownSig bool   // signed by an own key (accepted alone does not vouch)
	}
	var order []seenCommit
	err := walkHistory(rh.gits, agentTip, below, func(c *object.Commit) {
		if rh.acceptedCommit(c.Hash) {
			order = append(order, seenCommit{c: c})
			return
		}
		s, serr := verifyCommitSignature(c)
		reason := ""
		switch {
		case len(own) == 0:
			reason = "this store has no signer to know its own key"
		case serr != nil:
			reason = serr.Error()
		case !own[s.Fingerprint]:
			reason = "signed by " + s.Fingerprint[:8]
		}
		order = append(order, seenCommit{c: c, reason: reason, ownSig: reason == ""})
	})
	if err != nil {
		return err
	}

	// Transitive trust (F11 UI merge, user ruling D4, "chain of trust"): a
	// merge commit signed by an OWN key vouches for every commit it brought in
	// — everything reachable from its NON-FIRST parents within this range.
	// Nothing else vouches: not its first parent (that is this instance's own
	// line, judged on its own), not an own non-merge commit, not a merge
	// signed by anyone else or by no one, and not an accept-list entry.
	vouched := map[plumbing.Hash]bool{}
	for _, sc := range order {
		if !sc.ownSig || len(sc.c.ParentHashes) < 2 {
			continue
		}
		for _, p := range sc.c.ParentHashes[1:] {
			if below[p] || vouched[p] {
				continue
			}
			if err := walkHistory(rh.gits, p, below, func(c *object.Commit) { vouched[c.Hash] = true }); err != nil {
				return err
			}
		}
	}

	var refused, commits []string
	for _, sc := range order {
		if sc.reason == "" || vouched[sc.c.Hash] {
			continue
		}
		refused = append(refused, shortRefHash(sc.c.Hash)+": "+sc.reason)
		commits = append(commits, sc.c.Hash.String())
	}
	if len(refused) > 0 {
		return &ForeignLineageError{Branch: branch, Refused: refused, Commits: commits}
	}
	return nil
}
