package store

import (
	"fmt"
	"io"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"golang.org/x/crypto/ssh"

	storegit "knomit/internal/store/git"
)

// signCommitInPlace reads the commit at hash, signs it, and stores the signed
// version. Returns the new hash (signing changes the commit hash).
// No-op if signer is nil, returning the original hash.
func signCommitInPlace(s *storegit.Storer, signer ssh.Signer, commitHash plumbing.Hash) (plumbing.Hash, error) {
	if signer == nil {
		return commitHash, nil
	}

	// Load the commit.
	commitObj, err := object.GetCommit(s, commitHash)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("SignCommitInPlace: get commit: %w", err)
	}

	// Get the signable payload (commit content without signature header).
	payloadObj := s.NewEncodedObject()
	if err := commitObj.EncodeWithoutSignature(payloadObj); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("SignCommitInPlace: encode payload: %w", err)
	}
	reader, err := payloadObj.Reader()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("SignCommitInPlace: payload reader: %w", err)
	}
	payload, err := io.ReadAll(reader)
	reader.Close()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("SignCommitInPlace: read payload: %w", err)
	}

	// Sign.
	signature, err := signCommit(signer, payload)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("SignCommitInPlace: sign: %w", err)
	}

	// Set signature and re-encode.
	commitObj.PGPSignature = signature
	signedObj := s.NewEncodedObject()
	if err := commitObj.Encode(signedObj); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("SignCommitInPlace: encode signed: %w", err)
	}
	newHash, err := s.SetEncodedObject(signedObj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("SignCommitInPlace: store signed: %w", err)
	}

	return newHash, nil
}

// CommitHasSignature returns true if the commit at hash has a PGPSignature.
func CommitHasSignature(s *storegit.Storer, commitHashStr string) (bool, error) {
	hash := plumbing.NewHash(commitHashStr)
	commit, err := object.GetCommit(s, hash)
	if err != nil {
		return false, err
	}
	return commit.PGPSignature != "", nil
}
