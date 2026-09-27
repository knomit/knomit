package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"golang.org/x/crypto/ssh"
)

// SignerSeen is one key found signing commits in the reported history.
type SignerSeen struct {
	Fingerprint string    `json:"fingerprint"`      // full, 64 hex
	Key         string    `json:"key"`              // the authorized-key line, ready for verify_signers
	Claims      []string  `json:"claims,omitempty"` // author fp8 claims seen on its commits (should be its own fp8)
	FirstCommit string    `json:"first_commit"`     // earliest commit it signed, in history order
	FirstDate   time.Time `json:"first_date"`       // that commit's committer date
	Count       int       `json:"count"`            // commits it signed
	Listed      bool      `json:"listed"`           // in verify_signers at the tip, as the fold computes it
	Operator    bool      `json:"operator"`         // the configured [verify].operator_key
}

// SignatureReport is F09's dry run over one upstream: what the fold decides
// for the current history (whatever the repository's mode, off included), and
// every key that signed a commit in it, so an operator admits keys they
// RECOGNISE rather than whatever a list printed.
type SignatureReport struct {
	Upstream    string       `json:"upstream"`
	Tip         string       `json:"tip"`              // the commit reported on (origin/<upstream> when fetched, else the local upstream)
	Anchor      string       `json:"anchor,omitempty"` // the current verified anchor, if any
	Mode        string       `json:"mode"`             // the mode the fold arrives at
	WouldAnchor string       `json:"would_anchor,omitempty"`
	Unrooted    bool         `json:"unrooted,omitempty"`
	Commits     int          `json:"commits"`       // commits walked
	Unsigned    int          `json:"unsigned"`      // no signature at all
	NotSSH      int          `json:"not_ssh"`       // a non-SSH (e.g. PGP web-flow) signature
	BadSig      int          `json:"bad_signature"` // an SSH signature that does not verify
	Signers     []SignerSeen `json:"signers"`
	Refused     []Refusal    `json:"refused,omitempty"`
	Reported    []Refusal    `json:"reported,omitempty"`
	// Accepted lists the accept-list entries that matched a commit in this
	// history, with the scope they matched by (repo_uid "" = any repository).
	Accepted []Accept `json:"accepted,omitempty"`
}

// SignatureReport folds the history of upstream from the root, as a fresh
// clone would, WITHOUT moving or writing anything, and tallies every signer.
// from, when non-zero, limits the signer tally to commits not reachable from
// it (the fold itself always runs from the root: it is what decides).
func (s *Service) SignatureReport(ctx context.Context, upstream string, from plumbing.Hash) (SignatureReport, error) {
	rh := s.rh
	if upstream == "" {
		upstream = "main"
	}
	rep := SignatureReport{Upstream: upstream}
	tipRef, err := rh.gits.Reference(plumbing.NewRemoteReferenceName("origin", upstream))
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		tipRef, err = rh.gits.Reference(plumbing.NewBranchReferenceName(upstream))
	}
	if err != nil {
		return rep, fmt.Errorf("signature report: resolve %s: %w", upstream, err)
	}
	tip := tipRef.Hash()
	rep.Tip = tip.String()
	if a, err := rh.gits.Reference(verifiedRefName(upstream)); err == nil {
		rep.Anchor = a.Hash().String()
	}

	v := rh.verifier(ctx)
	res, err := v.fold(plumbing.ZeroHash, verifyContext{Mode: VerifyOff}, tip)
	if err != nil {
		return rep, fmt.Errorf("signature report: fold: %w", err)
	}
	rep.Mode, rep.Unrooted, rep.Refused, rep.Reported = res.Final.Mode, res.Unrooted, res.Refused, res.Reported
	if res.NewAnchor != plumbing.ZeroHash && res.Final.Mode != VerifyOff {
		rep.WouldAnchor = res.NewAnchor.String()
	}
	listed := res.Final.fingerprints()
	root := rh.rootOfTrust()

	stop := map[plumbing.Hash]bool{}
	if from != plumbing.ZeroHash {
		if err := v.walk(from, nil, func(c *object.Commit) { stop[c.Hash] = true }); err != nil {
			return rep, fmt.Errorf("signature report: --from %s: %w", from, err)
		}
	}
	seen := map[string]*SignerSeen{}
	err = v.walk(tip, stop, func(c *object.Commit) {
		rep.Commits++
		if rh.acceptList != nil {
			if a, ok := rh.acceptList.Lookup(c.Hash); ok {
				rep.Accepted = append(rep.Accepted, a)
			}
		}
		signer, serr := verifyCommitSignature(c)
		switch {
		case errors.Is(serr, ErrUnsigned):
			rep.Unsigned++
			return
		case errors.Is(serr, ErrNotSSHSIG):
			rep.NotSSH++
			return
		case serr != nil:
			rep.BadSig++
			return
		}
		e := seen[signer.Fingerprint]
		if e == nil {
			e = &SignerSeen{
				Fingerprint: signer.Fingerprint,
				Key:         strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.Key))),
				FirstCommit: c.Hash.String(),
				FirstDate:   c.Committer.When,
				Listed:      listed[signer.Fingerprint],
				Operator:    root.IsOperator(signer),
			}
			seen[signer.Fingerprint] = e
		}
		e.Count++
		if m := authorClaimRe.FindStringSubmatch(c.Author.Email); m != nil {
			claim := strings.ToLower(m[1])
			found := false
			for _, x := range e.Claims {
				found = found || x == claim
			}
			if !found {
				e.Claims = append(e.Claims, claim)
			}
		}
	})
	if err != nil {
		return rep, fmt.Errorf("signature report: walk: %w", err)
	}
	for _, e := range seen {
		rep.Signers = append(rep.Signers, *e)
	}
	sort.Slice(rep.Signers, func(i, j int) bool { return rep.Signers[i].FirstDate.Before(rep.Signers[j].FirstDate) })
	return rep, nil
}
