package store

import "context"

// ensureOKFBranches regenerates OKF bundles for the branches we publish: main
// plus the local agent branch. Best-effort — every failure is logged and
// swallowed so a broken mapper can never make the repo unclonable.
func (s *Service) ensureOKFBranches(ctx context.Context) {
	branches := []string{"main"}
	if owner, err := s.rh.AgentBranchOwner(ctx); err != nil {
		s.rh.logOKFSkip("main", "AgentBranchOwner: "+err.Error())
	} else if owner != "" && owner != "main" {
		branches = append(branches, owner)
	}
	for _, b := range branches {
		if _, err := s.EnsureOKF(ctx, b); err != nil {
			s.rh.logOKFSkip(b, "ensure failed: "+err.Error())
		}
	}
}
