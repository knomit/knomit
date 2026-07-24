package store

import (
	"fmt"
	"strings"
)

// okfRefPrefix is the branch namespace generated OKF bundles live under.
// Nothing may push a ref in this namespace to a remote (invariant:
// kb/invariants/okf/refs-never-pushed).
const okfRefPrefix = "okf/"

// assertNotOKFRef panics if branch is empty or an OKF-namespaced ref. Push
// sites call this on the branch they are about to push: OKF refs reaching a
// push is a programming error, not a runtime condition to recover from.
func assertNotOKFRef(branch string) {
	if branch == "" || strings.HasPrefix(branch, okfRefPrefix) {
		panic(fmt.Sprintf("okf: refusing to push protected ref %q", branch))
	}
}

// RefspecTouchesOKF reports whether a git refspec could push into the okf/*
// namespace on either side. Used by the ref-safety test to fail if a new push
// site ever constructs an okf-matching refspec.
func RefspecTouchesOKF(refspec string) bool {
	return strings.Contains(refspec, "refs/heads/"+okfRefPrefix) ||
		strings.Contains(refspec, ":"+okfRefPrefix) ||
		strings.HasPrefix(refspec, okfRefPrefix)
}
