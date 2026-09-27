package store

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"golang.org/x/crypto/ssh"

	"knomit/internal/pki"
)

// Reasons a commit's signature does not verify. Each is a distinct error so a
// refusal names what was wrong, not only that something was.
var (
	ErrUnsigned       = errors.New("commit is unsigned")
	ErrNotSSHSIG      = errors.New("commit signature is not an SSH signature")
	ErrMalformedSig   = errors.New("SSH signature is malformed")
	ErrWrongNamespace = errors.New("SSH signature namespace is not \"git\"")
	ErrWrongHashAlgo  = errors.New("SSH signature hash is not sha512")
	ErrNotEd25519Key  = errors.New("SSH signature key is not ssh-ed25519")
	ErrBadSignature   = errors.New("SSH signature does not verify")
)

// CommitSigner is the verified signer of one commit.
type CommitSigner struct {
	Key         ssh.PublicKey
	Fingerprint string // pki.Fingerprint: hex(sha256(SSH wire key)), 64 hex
}

// verifyCommitSignature checks c's SSHSIG signature over the commit payload
// without its signature header, exactly as signCommit (sshsig.go) produced it:
// magic, namespace "git", sha512, an Ed25519 key. It returns the key the
// signature was made with; WHETHER that key is admitted is the caller's
// question (the fold's signer set), never this function's.
//
// A PGP-armored signature (GitHub's web-flow merges) is ErrNotSSHSIG: F09
// treats it as unsigned, and a merge then goes to merge rule M3.
func verifyCommitSignature(c *object.Commit) (CommitSigner, error) {
	if strings.TrimSpace(c.PGPSignature) == "" {
		return CommitSigner{}, ErrUnsigned
	}
	if !strings.Contains(c.PGPSignature, "-----BEGIN SSH SIGNATURE-----") {
		return CommitSigner{}, ErrNotSSHSIG
	}
	var b64 strings.Builder
	for _, l := range strings.Split(c.PGPSignature, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "-----") {
			continue
		}
		b64.WriteString(l)
	}
	env, err := base64.StdEncoding.DecodeString(b64.String())
	if err != nil {
		return CommitSigner{}, fmt.Errorf("%w: base64: %v", ErrMalformedSig, err)
	}
	if !bytes.HasPrefix(env, []byte(sshsigMagic)) || len(env) < len(sshsigMagic)+4 {
		return CommitSigner{}, fmt.Errorf("%w: no SSHSIG magic", ErrMalformedSig)
	}
	rest := env[len(sshsigMagic):]
	if binary.BigEndian.Uint32(rest) != sshsigVersion {
		return CommitSigner{}, fmt.Errorf("%w: version", ErrMalformedSig)
	}
	rest = rest[4:]
	var fields [5][]byte // pubkey, namespace, reserved, hash algo, signature
	for i := range fields {
		f, r, ok := readSSHString(rest)
		if !ok {
			return CommitSigner{}, fmt.Errorf("%w: truncated", ErrMalformedSig)
		}
		fields[i], rest = f, r
	}
	pubBlob, namespace, hashAlgo, sigBlob := fields[0], fields[1], fields[3], fields[4]
	if string(namespace) != sshsigNamespace {
		return CommitSigner{}, ErrWrongNamespace
	}
	if string(hashAlgo) != sshsigHashAlgo {
		return CommitSigner{}, ErrWrongHashAlgo
	}
	pub, err := ssh.ParsePublicKey(pubBlob)
	if err != nil {
		return CommitSigner{}, fmt.Errorf("%w: key: %v", ErrMalformedSig, err)
	}
	cpk, ok := pub.(ssh.CryptoPublicKey)
	if !ok {
		return CommitSigner{}, ErrNotEd25519Key
	}
	edPub, ok := cpk.CryptoPublicKey().(ed25519.PublicKey)
	if !ok {
		return CommitSigner{}, ErrNotEd25519Key
	}
	var sig ssh.Signature
	if err := ssh.Unmarshal(sigBlob, &sig); err != nil {
		return CommitSigner{}, fmt.Errorf("%w: signature blob: %v", ErrMalformedSig, err)
	}

	payload, err := commitPayload(c)
	if err != nil {
		return CommitSigner{}, err
	}
	h := sha512.Sum512(payload)
	var signed []byte
	signed = append(signed, sshsigMagic...)
	signed = appendSSHString(signed, []byte(sshsigNamespace))
	signed = appendSSHString(signed, nil)
	signed = appendSSHString(signed, []byte(sshsigHashAlgo))
	signed = appendSSHString(signed, h[:])
	if err := pub.Verify(signed, &sig); err != nil {
		return CommitSigner{}, ErrBadSignature
	}
	return CommitSigner{Key: pub, Fingerprint: pki.Fingerprint(edPub)}, nil
}

// commitPayload is the byte string a commit's signature covers: the commit
// encoded without its signature header.
func commitPayload(c *object.Commit) ([]byte, error) {
	obj := &plumbing.MemoryObject{}
	if err := c.EncodeWithoutSignature(obj); err != nil {
		return nil, fmt.Errorf("%w: encode payload: %v", ErrMalformedSig, err)
	}
	r, err := obj.Reader()
	if err != nil {
		return nil, fmt.Errorf("%w: payload reader: %v", ErrMalformedSig, err)
	}
	defer r.Close()
	return io.ReadAll(r)
}

// readSSHString reads one SSH wire string (uint32 BE length + data).
func readSSHString(b []byte) (data, rest []byte, ok bool) {
	if len(b) < 4 {
		return nil, nil, false
	}
	n := binary.BigEndian.Uint32(b)
	if uint64(len(b)-4) < uint64(n) {
		return nil, nil, false
	}
	return b[4 : 4+n], b[4+n:], true
}
