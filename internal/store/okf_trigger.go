package store

import (
	"context"
	"fmt"
)

// ensureOKFBranches regenerates OKF bundles for the branches we publish: main
// plus the local agent branch. Best-effort — every failure is logged and
// swallowed so a broken mapper can never make the repo unclonable. A panic
// during generation (nil deref, slice bounds, a go-git panic on corrupt
// data) is recovered here too: it must not propagate into the net/http
// serve goroutine and fail the /info/refs advertise.
func (s *Service) ensureOKFBranches(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			s.rh.logOKFSkip("*", fmt.Sprintf("panic during OKF generation: %v", r))
		}
	}()

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
