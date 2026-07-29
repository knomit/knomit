//go:build !unix

package homelock

import "os"

// tryLock always reports success on the platforms without flock(2) — windows,
// plan9 and js. Everything knomit is deployed or developed on (linux, darwin,
// the BSDs, solaris, aix) is covered by the `unix` tag and gets the real
// implementation in flock_unix.go.
//
// A no-op rather than an error, because the alternative is worse: failing
// Acquire here would make `knomit serve` refuse to start on a platform where
// nothing is actually wrong, purely because knomit cannot answer a question it
// is asking as a precaution.
//
// What that costs, stated plainly rather than implied: on these platforms
// `serve` cannot detect a second server on the same KNOMIT_HOME, and `knomit
// restore` cannot detect a running one — so restore's refusal never fires and
// its protection is absent, not merely weakened. cmd/restore.go's help text
// carries that caveat for operators; see also the package comment.
func tryLock(f *os.File) (bool, error) { return true, nil }

func unlock(f *os.File) error { return nil }
