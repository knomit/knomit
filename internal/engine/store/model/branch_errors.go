package model

import "errors"

// ErrBranchNotFound is returned by ref-resolving code paths (ListDir, ReadFact,
// HeadCommit, etc.) when the requested branch has no matching git ref. Handlers
// in internal/edge/web detect this via errors.Is and map it to HTTP 404 instead of
// 500. The underlying go-git plumbing.ErrReferenceNotFound is preserved in the
// error chain for introspection.
var ErrBranchNotFound = errors.New("branch not found")

// ErrPathNotFound is returned by ReadFact (and the underlying readFile* helpers)
// when the requested path does not exist at the requested anchor. This is the
// "true 404" condition — distinct from a git/database error reading a path that
// should exist. Callers use errors.Is to surface a 404 while propagating real
// errors as 500.
var ErrPathNotFound = errors.New("path not found")

// ErrCommitNotFound is returned when a caller names a commit this repository
// does not hold: a commit-anchored read at a hash from ANOTHER repo (a lens
// keeps one mount's asOf commit while it reads another mount's fact), a
// commit-log cursor naming such a hash, or a value that is no hash at all.
// It is a client error: the web layer answers it 404 with the problem code
// commit_not_in_repo, never 500 (user ruling 2026-11-03). A commit this repo
// HAS where the requested path is absent is ErrPathNotFound instead.
var ErrCommitNotFound = errors.New("commit not found in this repo")
