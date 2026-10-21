package vcs

import (
	"errors"
)

// ErrNoDropBranchRowsHook is returned by DropBranch / DropBranchLocked on a
// Repo whose builder never called SetOnDropBranchRows. The drop is refused
// before the git ref or any row is removed: without the hook the per-branch
// rows in non-cascading tables would be left behind and the branches delete
// would fail after the ref was already gone.
var ErrNoDropBranchRowsHook = errors.New("drop branch: no SetOnDropBranchRows hook registered")
