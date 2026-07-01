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
	"knomit/internal/testenv",   // + /gitserver and any subpackage (prefix match)
	"knomit/internal/storytests",
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
			"knomit/tools/bridge",
			"knomit/tools/calibrate",
			"knomit/tools/drone",
			"knomit/tools/fetchlibs",
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
