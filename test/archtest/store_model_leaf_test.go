package archtest

import (
	"slices"
	"strings"
	"testing"
)

const (
	storeModelPkg    = "knomit/internal/store/model"
	storeImportsRoot = "knomit/internal/store"
)

// storeImportsIn returns the members of a transitive import closure that live
// in the store or any of its children, other than the model package itself.
func storeImportsIn(closure []string) []string {
	var bad []string
	for _, d := range closure {
		// `go list -test` names the package's own test variants
		// "<pkg> [<pkg>.test]" and "<pkg>.test".
		if d == storeModelPkg || d == storeModelPkg+".test" || strings.HasPrefix(d, storeModelPkg+" [") {
			continue
		}
		if d == storeImportsRoot || strings.HasPrefix(d, storeImportsRoot+"/") {
			bad = append(bad, d)
		}
	}
	return bad
}

// TestStoreModel_IsALeaf pins internal/store/model as a leaf: nothing in its
// transitive import closure, production or test, is the store or one of its
// children.
//
// The package holds the plain data and sentinel errors that vcs, sessions and
// sqlite define and every caller outside the store uses. Those callers and
// the three children all import it, so one import back into the store (even
// through a helper package) would close a cycle, or worse, make a sentinel
// reachable by two paths with a re-export between them. Once model is allowed
// to import a child, the child's types come back through it and the re-export
// layer the package exists to delete returns.
//
// It is not vacuous: the closure must contain model itself and a third-party
// package, so an empty or failed listing cannot pass, and
// TestStoreModelLeafDetectorSees feeds the detector a closure that does reach
// the store.
func TestStoreModel_IsALeaf(t *testing.T) {
	for _, withTests := range []bool{false, true} {
		args := []string{"list", "-deps"}
		if withTests {
			args = append(args, "-test")
		}
		args = append(args, storeModelPkg)
		closure := goListLines(t, args...)
		if !slices.Contains(closure, storeModelPkg) {
			t.Fatalf("closure of %s (tests=%v) does not contain the package: the scan listed nothing useful", storeModelPkg, withTests)
		}
		if !slices.Contains(closure, "github.com/go-git/go-git/v5/plumbing") {
			t.Fatalf("closure of %s (tests=%v) lacks go-git/plumbing, which model imports: the scan is not reading the real imports", storeModelPkg, withTests)
		}
		if bad := storeImportsIn(closure); len(bad) > 0 {
			t.Errorf("%s (tests=%v) must be a leaf but its import closure reaches %v", storeModelPkg, withTests, bad)
		}
	}
}

// TestStoreModelLeafDetectorSees is the falsifiability check for
// TestStoreModel_IsALeaf: a closure that reaches the store or a child is
// reported, the model package itself and unrelated packages are not.
func TestStoreModelLeafDetectorSees(t *testing.T) {
	closure := []string{
		storeModelPkg, storeModelPkg + " [" + storeModelPkg + ".test]", storeModelPkg + ".test", "fmt", "knomit/internal/domain/fact",
		"knomit/internal/store", "knomit/internal/store/internal/vcs", "knomit/internal/storefront",
	}
	got := storeImportsIn(closure)
	want := []string{"knomit/internal/store", "knomit/internal/store/internal/vcs"}
	if !slices.Equal(got, want) {
		t.Fatalf("storeImportsIn = %v, want %v", got, want)
	}
}
