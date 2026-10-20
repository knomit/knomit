package store

import (
	"crypto/ed25519"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
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
	// RuleMalformedTree: the commit's tree, or a parent's, is not well formed
	// (checkTreeShape): a directory entry that is not a tree object, a file
	// entry that is not a blob, an object that does not exist, a name go-git
	// will not read, a duplicate name, or a tree too deep. Judged before every
	// other rule, in every repository.
	RuleMalformedTree = "malformed-tree"
	// RuleNotOwner: the fleet repository only — a member commit that changes
	// a path outside artifacts/ that is not under members/<its own id>/, or
	// deletes a file there (the record-owner rule, recordOwner).
	RuleNotOwner = "not-owner"
	// RuleHostRecord: the fleet repository, a member's branch entering it
	// only (receive-pack, the master's peer judge; not CheckRange) — a signed
	// commit that is not a clean merge changes artifacts/fleet/host, which
	// only the fleet's master writes (the host-record rule, hostRecord).
	RuleHostRecord = "host-record"
	// RuleRevoked: the leaf artifact for the author's id at the fleet revision
	// is for the signing key, chains to the fleet root, and its serial is on
	// the fleet CRL (rule B; or a self-registration on a revoked leaf).
	RuleRevoked = "revoked"
	// RuleNoCert: the fleet revision holds its declared root (the fleet has a
	// PKI) but there is no valid leaf for the author's id naming its record's
	// key: none at the id's leaf path, one the root did not sign, one issued to
	// another agent id, or a root or CRL that cannot be read (rule B fails
	// closed).
	RuleNoCert = "no-cert"
	// RuleNotPusher: receive-pack only — a new commit signed by a key other
	// than the pushing instance's certificate key. CheckRange has no pusher.
	RuleNotPusher = "not-pusher"
	// The artifact rules (R7): a commit that changes a path under artifacts/
	// against the judged repository's ontology at main (artifactRules).
	RuleArtifactWriteOnce  = "artifact-write-once"  // a write-once file modified or deleted
	RuleArtifactAppendOnly = "artifact-append-only" // an append-only file not extended by an exact prefix, or deleted
	RuleArtifactReplace    = "artifact-replace"     // a replace file deleted, or replaced without its replace_when
	RuleArtifactSize       = "artifact-size"        // a blob larger than its max_size
	RuleArtifactPKI        = "artifact-pki"         // a pki file added that does not hold as its role
	RuleArtifactRules      = "artifact-rules"       // the rules for a path are ambiguous, or a blob cannot be read
	RuleArtifactNotAFile   = "artifact-not-a-file"  // a tree, symlink or submodule at a declared path, or anything under a directory a pattern declares a file (fact.Ontology.ArtifactFileAt)
)

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

// CommitExists reports whether this repository's object store holds commit h.
func (s *Service) CommitExists(h plumbing.Hash) bool {
	_, err := object.GetCommit(s.rh.gits, h)
	return err == nil
}

// CommitAgent is the agent id in the author address of commit h (the lookup
// the commit judge makes, agentIDOfAuthor): "" when this repository does not
// hold h or its author is not an agent address. Nothing is derived from a
// branch name.
func (s *Service) CommitAgent(h plumbing.Hash) string {
	c, err := object.GetCommit(s.rh.gits, h)
	if err != nil {
		return ""
	}
	return agentIDOfAuthor(c.Author.Email)
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
