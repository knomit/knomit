package fleetrec

import "testing"

// MalformedLoggedForTest is how many distinct (fleet, path, blob) skips the
// process has latched so far: the parent's white-box tests read it to prove a
// malformed record is logged once per blob, not once per run. It panics
// outside a test binary, like the vcs child's seams, so production cannot
// lean on it.
func MalformedLoggedForTest() int {
	if !testing.Testing() {
		panic("fleetrec: test seam called outside a test binary")
	}
	n := 0
	malformedLogged.Range(func(_, _ any) bool { n++; return true })
	return n
}
