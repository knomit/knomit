package store

import (
	"github.com/go-git/go-git/v5/plumbing/transport"

	"knomit/internal/fact"
)

func parseFact(path, content string) (FactRecord, error) {
	f, err := fact.ParseFact(path, content)
	if err != nil {
		return FactRecord{}, err
	}
	return NewFactRecord(f, ""), nil
}

// Compile-time interface checks.
var _ FactIndex = (*factIndex)(nil)

// factIndex owns all git-backed fact operations: reading and writing.
// Shared git-level plumbing (signer, authorSig, committerSig, notifyCommit)
// lives on repoHandler — factIndex reaches up via fi.rh.*.
type factIndex struct {
	rh   *repoHandler
	auth transport.AuthMethod
	im   IndexManager // index synchronization after each commit
}
