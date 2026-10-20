package vcs

import (
	"errors"
)

// ErrBranchNotFound is returned by ref-resolving code paths (ListDir, ReadFact,
// HeadCommit, etc.) when the requested branch has no matching git ref. Handlers
// in internal/web detect this via errors.Is and map it to HTTP 404 instead of
// 500. The underlying go-git plumbing.ErrReferenceNotFound is preserved in the
// error chain for introspection.
var ErrBranchNotFound = errors.New("branch not found")

// ErrPathNotFound is returned by ReadFact (and the underlying readFile* helpers)
// when the requested path does not exist at the requested anchor. This is the
// "true 404" condition — distinct from a git/database error reading a path that
// should exist. Callers use errors.Is to surface a 404 while propagating real
// errors as 500.
var ErrPathNotFound = errors.New("path not found")

// ErrNoDropBranchRowsHook is returned by DropBranch / DropBranchLocked on a
// Repo whose builder never called SetOnDropBranchRows. The drop is refused
// before the git ref or any row is removed: without the hook the per-branch
// rows in non-cascading tables would be left behind and the branches delete
// would fail after the ref was already gone.
var ErrNoDropBranchRowsHook = errors.New("drop branch: no SetOnDropBranchRows hook registered")
