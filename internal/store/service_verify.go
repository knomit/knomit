package store

import (
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"golang.org/x/crypto/ssh"

	"knomit/internal/store/internal/vcs"
	"knomit/internal/store/model"
)

// CommitExists reports whether this repository's object store holds commit h.
func (s *Service) CommitExists(h plumbing.Hash) bool {
	_, err := object.GetCommit(s.rh.Storer(), h)
	return err == nil
}

// CommitAgent is the agent id in the author address of commit h (the lookup
// the commit judge makes, AgentIDOfAuthor): "" when this repository does not
// hold h or its author is not an agent address. Nothing is derived from a
// branch name.
func (s *Service) CommitAgent(h plumbing.Hash) string {
	c, err := object.GetCommit(s.rh.Storer(), h)
	if err != nil {
		return ""
	}
	return vcs.AgentIDOfAuthor(c.Author.Email)
}

// SetAcceptList installs the accept list, bound to this repository. Like
// SetSigner it must be re-applied on every store reopen.
func (s *Service) SetAcceptList(a func(commit plumbing.Hash) (model.Accept, bool)) {
	s.rh.SetAcceptList(a)
}

// SetOwnKeys installs the source of this instance's historical keys (its
// fleet member record's versions) for E4. Re-applied on every store reopen,
// like SetSigner.
func (s *Service) SetOwnKeys(f func() []ssh.PublicKey) { s.rh.SetOwnKeys(f) }
