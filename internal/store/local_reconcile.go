// Local reconcile: keeping the consensus branch meaningful on a repo that has
// no origin to follow.
//
// A repo WITH an origin gets its consensus branch from the remote —
// reconcileMain fast-forwards refs/heads/<upstream> to
// refs/remotes/origin/<upstream> on every sync tick. A repo WITHOUT one never
// starts that loop, so its main sat at the root commit forever while every
// fact accumulated on the agent branch. That is invisible locally (readers
// follow the agent branch) and fatal to a peer: a knomit instance subscribing
// to this one over /git reads the consensus branch, and would have seen one
// empty commit.
package store

// UpstreamBranch is the repo's consensus branch name: the configurable
// Remote.Branch when an origin row exists, else "main" (what local init
// creates). Served HEAD and the local reconcile both key off this.
func (s *Service) UpstreamBranch() string {
	if r, err := s.Remote().GetRemote("origin"); err == nil && r != nil && r.Branch != "" {
		return r.Branch
	}
	return "main"
}
