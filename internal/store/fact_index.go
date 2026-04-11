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
// All shared git-level plumbing lives on repoHandler — factIndex reaches up
// via fi.rh.*. In particular, index sync after a commit happens inside
// rh.notifyCommit (which is called from writeFile / deleteFile / batchWrite),
// so factIndex no longer holds its own IndexManager back-reference.
type factIndex struct {
	rh   *repoHandler
	auth transport.AuthMethod
}
