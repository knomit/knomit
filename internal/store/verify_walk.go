package store

import (
	"context"
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

// acceptedCommit reports whether h is on this instance's accept list
// (`knomit verify accept`), which waives E4 for that one commit.
func (rh *repoHandler) acceptedCommit(ctx context.Context, h plumbing.Hash) bool {
	var one int
	err := conn(ctx, rh.db).QueryRowContext(ctx,
		`SELECT 1 FROM verify_accepted WHERE commit_hash = ?`, h.String()).Scan(&one)
	return err == nil
}
