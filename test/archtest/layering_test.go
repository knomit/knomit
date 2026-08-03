package archtest

import (
	"os/exec"
	"strings"
	"testing"
)

// deps returns the transitive import closure of pkg via `go list -deps`, which
// resolves imports without compiling and so needs no native libs.
func deps(t *testing.T, pkg string) []string {
	t.Helper()
	out, err := exec.Command("go", "list", "-deps", pkg).CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps %s failed: %v\n%s", pkg, err, out)
	}
	var got []string
	for _, line := range strings.Split(string(out), "\n") {
		if dep := strings.TrimSpace(line); dep != "" {
			got = append(got, dep)
		}
	}
	return got
}

// TestFactStaysPure pins internal/fact as the cheap leaf that everything else
// can import freely.
//
// internal/fact is imported by store, repos, web, mcp, synthesize, okf, refs and
// the test harness — several of them precisely because it is pure: it classifies
// and formats, it does not read a corpus. The split is deliberate and recorded:
// fact.ClassifyRef answers what KIND a ref is, while whether a ref RESOLVES is
// commit-dependent and lives in internal/refs, which may import the store.
//
// Without this guard the boundary is a doc comment. The failure it prevents is
// silent rather than loud: adding a store call to internal/fact still compiles,
// and merely drags SQLite, go-git and golang-migrate into every consumer that
// wanted to parse a string. The same trap applies to a subpackage — a
// hypothetical internal/fact/<something> that imports the store would read as
// "part of fact" while carrying 2.5x its dependency weight.
func TestFactStaysPure(t *testing.T) {
	forbiddenByFact := []string{
		"knomit/internal/store",
		"knomit/internal/repos",
		"knomit/internal/embeddings",
		"knomit/internal/refs",
		"knomit/internal/web",
		"knomit/internal/mcp",
		"knomit/internal/synthesize",
	}

	for _, dep := range deps(t, "knomit/internal/fact") {
		for _, f := range forbiddenByFact {
			if dep == f || strings.HasPrefix(dep, f+"/") {
				t.Errorf("internal/fact transitively imports %q — it must stay the pure leaf that "+
					"every other layer can import cheaply. Ref classification belongs here; anything "+
					"needing corpus access or repo identity belongs in internal/refs.", dep)
			}
		}
	}
}

// TestParamsHasNoDependencies pins internal/embeddings/params as a dependency-
// free leaf.
//
// params holds the cgo-free half of the embedding-model contract — the
// calibrated cosine thresholds and the default model id — and sits UNDER
// internal/embeddings, which carries cgo (ONNX via import "C"). The nesting is
// what makes the tree honest about whose values these are, but it also puts an
// import of the parent one line away.
//
// That import would be silent and expensive: internal/store, internal/config and
// internal/synthesize all import params, so an edge upward would link the ONNX
// runtime into the store, the config loader, and every binary that touches
// either. Being importable without cgo is the entire reason the package exists,
// so the check here is the strictest available — zero non-stdlib imports, which
// forecloses the mistake rather than enumerating it.
func TestParamsHasNoDependencies(t *testing.T) {
	const pkg = "knomit/internal/embeddings/params"

	for _, dep := range deps(t, pkg) {
		if dep == pkg {
			continue // the package itself
		}
		// Stdlib import paths have no dot in their first element; everything
		// else is a module path (knomit/..., github.com/..., golang.org/x/...).
		first, _, _ := strings.Cut(dep, "/")
		if strings.Contains(first, ".") {
			t.Errorf("internal/embeddings/params imports %q — it must have ZERO non-stdlib "+
				"dependencies so that store, config and synthesize can read model thresholds "+
				"without linking cgo. In particular it must never import its own parent "+
				"internal/embeddings, which carries the ONNX runtime.", dep)
		}
	}
}
