package store

import (
	"crypto/ed25519"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"golang.org/x/crypto/ssh"

	"knomit/internal/pki"
)

// Verification modes (the ontology's verify_signatures), ordered:
// off < log < enforce.
const (
	VerifyOff     = "off"
	VerifyLog     = "log"
	VerifyEnforce = "enforce"
)

// Refusal is one commit the acceptance gate did not accept.
type Refusal struct {
	Commit   string `json:"commit"`
	Rule     string `json:"rule"`
	SignerFP string `json:"signer_fp,omitempty"`
	Reason   string `json:"reason"`
}

// Refusal rules of the acceptance gate (CheckRange).
const (
	RuleSignature    = "signature"     // unsigned, or an SSH signature that does not verify
	RuleNoRecord     = "no-record"     // the author's agent id has no member record
	RuleAmbiguous    = "ambiguous"     // more than one member record at the author's id
	RuleKeyMismatch  = "key-mismatch"  // the signing key is not the record's current key
	RuleDuplicateKey = "duplicate-key" // the signing key is the current key of more than one record
	RuleInactive     = "inactive"      // the record's state is not active
	RuleMerge        = "merge"         // an unsigned merge that fails M3
)

// walkHistory visits every commit reachable from start and not from stop,
// parents before children (a depth-first post-order), once each.
func walkHistory(st storer.EncodedObjectStorer, start plumbing.Hash, stop map[plumbing.Hash]bool, visit func(*object.Commit)) error {
	seen := map[plumbing.Hash]bool{}
	type frame struct {
		c    *object.Commit
		next int
	}
	root, err := object.GetCommit(st, start)
	if err != nil {
		return fmt.Errorf("verify: commit %s: %w", start, err)
	}
	stack := []*frame{{c: root}}
	seen[start] = true
	for len(stack) > 0 {
		f := stack[len(stack)-1]
		if f.next < len(f.c.ParentHashes) {
			p := f.c.ParentHashes[f.next]
			f.next++
			if seen[p] || stop[p] {
				continue
			}
			seen[p] = true
			pc, err := object.GetCommit(st, p)
			if err != nil {
				return fmt.Errorf("verify: commit %s: %w", p, err)
			}
			stack = append(stack, &frame{c: pc})
			continue
		}
		stack = stack[:len(stack)-1]
		visit(f.c)
	}
	return nil
}

// keyFingerprint is pki.Fingerprint of an ssh-ed25519 public key.
func keyFingerprint(pk ssh.PublicKey) (string, error) {
	cpk, ok := pk.(ssh.CryptoPublicKey)
	if !ok {
		return "", ErrNotEd25519Key
	}
	ed, ok := cpk.CryptoPublicKey().(ed25519.PublicKey)
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrNotEd25519Key, pk.Type())
	}
	return pki.Fingerprint(ed), nil
}

// Accept is one entry of the operator's accept list.
type Accept struct {
	Commit  string `json:"commit"`
	RepoUID string `json:"repo_uid,omitempty"` // "" = any repository
	Note    string `json:"note,omitempty"`
}

// AcceptList is the operator's per-INSTANCE accept list (control.db). Its only
// reader is E4: an accept waives the own-lineage refusal for one commit on
// origin's copy of this instance's agent branch (for example commits made
// before signing existed). The repo-database table verify_accepted (repo
// migration 000027) is never read or written.
type AcceptList interface {
	Lookup(commit plumbing.Hash) (Accept, bool)
}

// CommitExists reports whether this repository's object store holds commit h.
func (s *Service) CommitExists(h plumbing.Hash) bool {
	_, err := object.GetCommit(s.rh.gits, h)
	return err == nil
}

// SetAcceptList installs the accept list, bound to this repository. Like
// SetSigner it must be re-applied on every store reopen.
func (s *Service) SetAcceptList(a AcceptList) { s.rh.acceptList = a }

// acceptedCommit reports whether h is on this instance's accept list, which
// waives E4 for that one commit.
func (rh *repoHandler) acceptedCommit(h plumbing.Hash) bool {
	if rh.acceptList == nil {
		return false
	}
	_, ok := rh.acceptList.Lookup(h)
	return ok
}

// SetOwnKeys installs the source of this instance's historical keys (its
// fleet member record's versions) for E4. Re-applied on every store reopen,
// like SetSigner.
func (s *Service) SetOwnKeys(f func() []ssh.PublicKey) { s.rh.ownKeys = f }
