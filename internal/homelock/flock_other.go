//go:build !unix

package homelock

import "os"

// tryLock always succeeds on platforms without flock(2).
//
// This is a deliberate no-op rather than an error: knomit is deployed on Linux
// and developed on macOS, and failing Acquire elsewhere would make `knomit
// serve` refuse to start on a platform where nothing is actually wrong. The
// cost is that `knomit restore` cannot detect a running server there, which its
// own error text does not promise — see the package comment.
func tryLock(f *os.File) (bool, error) { return true, nil }

func unlock(f *os.File) error { return nil }
