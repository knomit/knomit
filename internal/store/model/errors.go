package model

import (
	"errors"
)

// ErrNoSigner is returned by every authored write on a Service that has no
// commit signer. It used to be a silent no-op: a Service opened without
// SetSigner committed UNSIGNED, and those commits were pushed as this
// instance's agent branch (the origin wizard's replay store and the
// initialize flow did exactly that). Under F09 an unsigned commit is refused
// by every verifying peer, so the store refuses to make one.
//
// The init commits of a new repository (README and ontology, repo.go) are not
// affected: they are written by the plumbing layer, which never signs. They
// precede any agent-branch work and lie below every F09 anchor, so they are
// never verified.
var ErrNoSigner = errors.New("store has no commit signer: refusing to write an unsigned commit")

// ErrHistoryChanged is returned for a PathHistory continuation whose anchor
// has left the branch, or whose position names a change no longer on the
// branch: the history it was paging is gone, and the caller must restart.
var ErrHistoryChanged = errors.New("path history changed since this position was issued")
