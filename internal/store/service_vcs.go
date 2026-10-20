package store

// SetAgentID sets the agent id every commit this store AUTHORS names: the
// author <id>+<operation>@agents.knomit.io and the committer
// <id>@agents.knomit.io. Every authored commit is signed by this instance's
// key, so it is this instance's id whichever branch the commit lands on (the
// agent branch, an experiment forked from it, the replay store of the origin
// wizard). Like SetSigner it must be re-applied on every store reopen.
func (s *Service) SetAgentID(id string) { s.rh.SetAgentID(id) }
