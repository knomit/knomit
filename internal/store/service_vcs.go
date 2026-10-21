package store

import (
	"context"

	"github.com/go-git/go-git/v5/plumbing"
)

// DiffFacts is the two-tree diff of the whole ontology root between two
// commits with NO ancestry check: the trigger dispatcher's advance may be a
// rewind replay, which ChangesUnder refuses. It is a read of the git layer
// (vcs.Repo.DiffFacts), so it is the store's own operation and not a facts
// forward: the facts handles hold no vcs methods.
func (s *Service) DiffFacts(ctx context.Context, from, to plumbing.Hash) ([]PathChange, error) {
	return s.rh.DiffFacts(ctx, from, to)
}

// SetAgentID sets the agent id every commit this store AUTHORS names: the
// author <id>+<operation>@agents.knomit.io and the committer
// <id>@agents.knomit.io. Every authored commit is signed by this instance's
// key, so it is this instance's id whichever branch the commit lands on (the
// agent branch, an experiment forked from it, the replay store of the origin
// wizard). Like SetSigner it must be re-applied on every store reopen.
func (s *Service) SetAgentID(id string) { s.rh.SetAgentID(id) }
