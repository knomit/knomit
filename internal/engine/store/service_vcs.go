package store

import (
	"context"

	"knomit/internal/engine/store/model"

	"github.com/go-git/go-git/v5/plumbing"
)

// DiffFacts is the two-tree diff of the whole ontology root between two
// commits with NO ancestry check: the trigger dispatcher's advance may be a
// rewind replay, which ChangesUnder refuses. It is a read of the git layer
// (vcs.Repo.DiffFacts), so it is the store's own operation and not a facts
// forward: the facts handles hold no vcs methods.
func (s *Service) DiffFacts(ctx context.Context, from, to plumbing.Hash) ([]model.PathChange, error) {
	return s.rh.DiffFacts(ctx, from, to)
}

// The branch operations below are vcs-backed Service operations, the store's
// own API (kb/conventions/go/package-boundaries/no-facades/44ec3b17.md,
// EXCEPTION). vcs lives under store/internal/, so a caller outside the store
// cannot hold the *vcs.Repo, and nothing outside the store may create, move or
// drop a ref around the write path; the Service method is the only door and
// is the whole of its surface. They replace the BranchIndex interface that
// Service.Branches() used to hand out (deleted in E7a).

// HeadCommit is the commit hash at the tip of branch. A branch that does not
// exist, or a cancelled ctx, is an error, never an empty hash.
func (s *Service) HeadCommit(ctx context.Context, branch string) (string, error) {
	return s.rh.HeadCommit(ctx, branch)
}

// ListBranches lists every branch the store knows.
func (s *Service) ListBranches(ctx context.Context) ([]model.Branch, error) {
	return s.rh.ListBranches(ctx)
}

// CreateBranch creates branch at the tip of fromBranch.
func (s *Service) CreateBranch(ctx context.Context, branch, fromBranch string) error {
	return s.rh.CreateBranch(ctx, branch, fromBranch)
}

// DefaultBranch resolves the default branch name from the repo's HEAD ref.
// Returns an empty string if HEAD is detached.
func (s *Service) DefaultBranch(ctx context.Context) (string, error) {
	return s.rh.DefaultBranch(ctx)
}

// AgentBranchOwner is the agent branch this database was last booted as. An
// empty result means "unknown" (a database created before the key existed, or
// one that never completed a boot), not "no previous branch".
func (s *Service) AgentBranchOwner(ctx context.Context) (string, error) {
	return s.rh.AgentBranchOwner(ctx)
}

// SetAgentBranchOwner records branch as AgentBranchOwner.
func (s *Service) SetAgentBranchOwner(ctx context.Context, branch string) error {
	return s.rh.SetAgentBranchOwner(ctx, branch)
}

// BranchInfo returns all branches partitioned into regular branches, agent
// branches (prefixed "agent/"), and the agent branch matching localAgent (if
// any).
func (s *Service) BranchInfo(localAgent string) (branches, agentBranches []string, matchedAgent string) {
	return s.rh.BranchInfo(localAgent)
}

// GetExperiment reads one experiment record (a vcs read; the lifecycle itself
// is merge.Repo, reached as Service.Merge). The bool separates "no such
// experiment" from a read failure: eligibility must not treat the latter as an
// answer.
func (s *Service) GetExperiment(ctx context.Context, name string) (model.Experiment, bool, error) {
	return s.rh.GetExperiment(ctx, name)
}

// SetAgentID sets the agent id every commit this store AUTHORS names: the
// author <id>+<operation>@agents.knomit.io and the committer
// <id>@agents.knomit.io. Every authored commit is signed by this instance's
// key, so it is this instance's id whichever branch the commit lands on (the
// agent branch, an experiment forked from it, the replay store of the origin
// wizard). Like SetSigner it must be re-applied on every store reopen.
func (s *Service) SetAgentID(id string) { s.rh.SetAgentID(id) }

// OntologyAt returns the ontology file at branch's tip, trying every ontology
// path newest first: (nil, nil) when that tree has none. The consensus merger
// reads the repo's settings where the repo's owner decides them, the tip of
// the consensus branch.
//
// A symlink at the first rung present is an error wrapping
// fact.ErrSymlinkNotFollowed and naming the path, never (nil, nil).
func (s *Service) OntologyAt(ctx context.Context, branch string) ([]byte, error) {
	_, data, err := s.rh.OntologyFileAt(ctx, branch)
	return data, err
}

// OntologyFileAt is OntologyAt also returning the rung it read ("" when the
// tip has none). The repo's open (repos.loadOntology) and its create-time
// check (branchHasOntology) read the ontology through it.
func (s *Service) OntologyFileAt(ctx context.Context, branch string) (string, []byte, error) {
	return s.rh.OntologyFileAt(ctx, branch)
}

// SetReplaySkipPaths names the paths a rewind replay never carries: a
// non-merge commit of this branch that changes nothing but these paths is
// dropped from the replay instead of being re-submitted onto the new
// upstream. The fleet repository sets its master marker
// (fact.FleetMasterPath): a copy that is not the master on the new upstream
// never re-submits its own claim to the role, or its stand-down, which the
// host's judge would refuse for good (user ruling 2026-10-16: the loser
// drops its own claim). Any other repository sets nothing.
func (s *Service) SetReplaySkipPaths(paths ...string) {
	set := map[string]bool{}
	for _, p := range paths {
		set[p] = true
	}
	s.rh.SetReplaySkip(set)
}
