package archtest

import (
	"os/exec"
	"strings"
	"testing"
)

// forbidden lists the test-only support packages that must NEVER be linked into
// a shipped binary. They import "testing" and stand up fault servers, bare git
// repos, etc.; they exist only for _test.go files to import. If a production
// package ever imports one (directly or transitively), it would bloat the real
// binary with test scaffolding — this guard fails first.
var forbidden = []string{
	"knomit/test/testenv", // + /gitserver and any subpackage (prefix match)
	"knomit/test/storytests",
	// Installs a fallback commit signer for test binaries (F09 PR 2). The hook
	// it calls refuses to work outside a test binary anyway; this keeps the
	// package itself out of every shipped binary too.
	"knomit/internal/testsupport/testsigner",
	// Module-root lookup for tests that scan source across packages; imports
	// "testing", so it stays out of every shipped binary.
	"knomit/internal/testsupport/modroot",
	// Bare-repository fixtures for tests (git's auto-maintenance off); imports
	// "testing", so it stays out of every shipped binary.
	"knomit/internal/testsupport/gitfixture",
	// The test doors into a *store.Service (corrupt an object, plant a ref,
	// commit without the index, backdate an experiment). Its init panics outside
	// a test binary; this keeps the package itself out of every shipped binary
	// too, and TestTestSeamsNotReachedFromProduction keeps production source
	// from importing it.
	"knomit/internal/engine/store/storetest",
	// The synthesize tree's shared test fixtures (store repos, embedder
	// stand-ins, environment builders). It imports "testing" and is meant for
	// _test.go files only; living under synthesize/internal/ keeps every package
	// outside the tree from importing it, and this keeps the package out of every
	// shipped binary even if a production file inside the tree ever did.
	"knomit/internal/corpus/synthesize/internal/synthtest",
}

// shippedGroups are the module's real binaries (main packages), grouped by the
// build tags needed to resolve them. The desktop app lives behind //go:build
// desktop, so it needs the tag to appear at all.
var shippedGroups = []struct {
	name string
	tags string
	pkgs []string
}{
	{
		name: "default binaries",
		pkgs: []string{
			"knomit",
			"knomit/tools/appcast",
			"knomit/tools/bridge",
			"knomit/tools/calibrate",
			"knomit/tools/fetchlibs",
			"knomit/tools/okf",
			"knomit/tools/relnotes",
		},
	},
	{
		name: "desktop app",
		tags: "desktop",
		pkgs: []string{"knomit/tools/desktop"},
	},
}

// TestHarnessNotLinkedIntoShippedBinaries walks the transitive import graph of
// every shipped binary (via `go list -deps`, which resolves imports without
// compiling, so it needs no native libs) and fails if any forbidden test-only
// package appears. This is the enforcement behind "the test harness never ships
// in the real binary": Go already excludes packages nothing imports, and this
// keeps it that way as the code evolves.
func TestHarnessNotLinkedIntoShippedBinaries(t *testing.T) {
	for _, grp := range shippedGroups {
		args := []string{"list", "-deps"}
		if grp.tags != "" {
			args = append(args, "-tags", grp.tags)
		}
		args = append(args, grp.pkgs...)

		out, err := exec.Command("go", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: go list -deps failed: %v\n%s", grp.name, err, out)
		}

		for _, line := range strings.Split(string(out), "\n") {
			dep := strings.TrimSpace(line)
			for _, f := range forbidden {
				if dep == f || strings.HasPrefix(dep, f+"/") {
					t.Errorf("%s: a shipped binary transitively imports test-only package %q — "+
						"the test harness must never ship in the real binary. Break the import "+
						"(move the helper into a _test.go file or a test-only package).", grp.name, dep)
				}
			}
		}
	}
}

// TestShippedGroupsListEveryMainPackage keeps shippedGroups complete: B1 only
// checks the binaries it lists, so a new `package main` (tools/appcast,
// tools/okf and tools/relnotes were missing for a while) would otherwise ship
// without the storetest and harness checks. It asks `go list` for every main
// package in the module under the default tags and the desktop tag and fails
// on one that no group names.
func TestShippedGroupsListEveryMainPackage(t *testing.T) {
	listed := map[string]bool{}
	for _, grp := range shippedGroups {
		for _, p := range grp.pkgs {
			listed[p] = true
		}
	}
	seen := 0
	for _, tags := range []string{"", "desktop"} {
		args := []string{"list", "-f", "{{if eq .Name \"main\"}}{{.ImportPath}}{{end}}"}
		if tags != "" {
			args = append(args, "-tags", tags)
		}
		args = append(args, "./...")
		cmd := exec.Command("go", args...)
		cmd.Dir = moduleRoot(t)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("go list (tags %q): %v", tags, err)
		}
		for _, p := range strings.Fields(string(out)) {
			seen++
			if !listed[p] {
				t.Errorf("main package %s is not in shippedGroups: add it so TestHarnessNotLinkedIntoShippedBinaries checks it", p)
			}
		}
	}
	if seen < len(listed) {
		t.Errorf("go list found %d main packages, shippedGroups names %d: the listing matches nothing", seen, len(listed))
	}
}
