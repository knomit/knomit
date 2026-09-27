package store

import (
	"errors"
	"fmt"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// CheckoutVerdict is `knomit verify ci`'s answer for one candidate branch in
// a plain git checkout (a forge CI job): what the fold decides for the
// candidate's history, restricted to the commits the candidate would ADD to
// the upstream.
type CheckoutVerdict struct {
	Upstream  string    `json:"upstream"`
	Candidate string    `json:"candidate"`
	Mode      string    `json:"mode"`     // the fold's final mode for the candidate's history
	New       int       `json:"new"`      // commits reachable from the candidate and not from the upstream
	Refused   []Refusal `json:"refused"`  // refused commits among the new ones
	Reported  []Refusal `json:"reported"` // notes among the new ones
	Unrooted  bool      `json:"unrooted"` // a policy change needed an operator key the job was not given
}

// Blocked reports whether the candidate must not be merged: verification is
// on (log or enforce) and a new commit is refused, or a policy change could
// not be judged. With verification off nothing is checked.
func (v CheckoutVerdict) Blocked() bool {
	return v.Unrooted || (v.Mode != VerifyOff && len(v.Refused) > 0)
}

// VerifyCheckout runs F09's fold over a plain git repository at dir (no knomit
// store), folding the CANDIDATE's history from the root exactly as a fresh
// knomit clone would. It never reads a policy from the head's ontology file:
// the mode and signers are the fold's context, so an unauthorised change in
// the candidate cannot switch its own check off.
//
// upstream and candidate are revisions (a branch name, a remote ref such as
// origin/main, or a hash). accepted may be nil: a CI job has no accept list,
// so a merge or commit that needs one is left to the operator.
func VerifyCheckout(dir, upstream, candidate string, root RootOfTrust, accepted func(plumbing.Hash) bool) (CheckoutVerdict, error) {
	out := CheckoutVerdict{Upstream: upstream, Candidate: candidate}
	// The directory itself (a bare repository, or a work tree's root), else
	// the enclosing work tree's .git.
	repo, err := gogit.PlainOpen(dir)
	if err != nil {
		repo, err = gogit.PlainOpenWithOptions(dir, &gogit.PlainOpenOptions{DetectDotGit: true})
	}
	if err != nil {
		return out, fmt.Errorf("verify ci: open %s: %w", dir, err)
	}
	resolve := func(rev string) (plumbing.Hash, error) {
		h, err := repo.ResolveRevision(plumbing.Revision(rev))
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("verify ci: resolve %q: %w", rev, err)
		}
		return *h, nil
	}
	up, err := resolve(upstream)
	if err != nil {
		return out, err
	}
	cand, err := resolve(candidate)
	if err != nil {
		return out, err
	}
	if root == nil {
		root = StaticRoot{}
	}
	v := &verifier{st: repo.Storer, root: root, accepted: accepted}
	res, err := v.fold(plumbing.ZeroHash, verifyContext{Mode: VerifyOff}, cand)
	if err != nil {
		return out, fmt.Errorf("verify ci: fold: %w", err)
	}
	out.Mode, out.Unrooted = res.Final.Mode, res.Unrooted

	onUpstream := map[plumbing.Hash]bool{}
	w := &verifier{st: repo.Storer}
	if err := w.walk(up, nil, func(c *object.Commit) { onUpstream[c.Hash] = true }); err != nil {
		return out, fmt.Errorf("verify ci: walk %s: %w", upstream, err)
	}
	newCommits := map[string]bool{}
	if err := w.walk(cand, onUpstream, func(c *object.Commit) { newCommits[c.Hash.String()] = true }); err != nil {
		return out, fmt.Errorf("verify ci: walk %s: %w", candidate, err)
	}
	out.New = len(newCommits)
	out.Refused, out.Reported = []Refusal{}, []Refusal{}
	for _, r := range res.Refused {
		if newCommits[r.Commit] {
			out.Refused = append(out.Refused, r)
		}
	}
	for _, r := range res.Reported {
		if newCommits[r.Commit] {
			out.Reported = append(out.Reported, r)
		}
	}
	if out.Unrooted && out.New == 0 {
		return out, errors.New("verify ci: the upstream itself holds a policy change this job cannot judge: set the operator key")
	}
	return out, nil
}
