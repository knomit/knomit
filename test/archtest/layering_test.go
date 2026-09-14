package archtest

import (
	"os/exec"
	"slices"
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

// packages returns the package paths matching pattern via `go list`, which like
// deps resolves without compiling.
func packages(t *testing.T, pattern string) []string {
	t.Helper()
	out, err := exec.Command("go", "list", pattern).CombinedOutput()
	if err != nil {
		t.Fatalf("go list %s failed: %v\n%s", pattern, err, out)
	}
	var got []string
	for _, line := range strings.Split(string(out), "\n") {
		if pkg := strings.TrimSpace(line); pkg != "" {
			got = append(got, pkg)
		}
	}
	return got
}

// TestPlatformKnowsNothingAboutKnomit pins internal/platform as the process
// tier: it knows the OS and the binary, and nothing about this application.
//
// That sentence is the ONLY thing the directory name conveys, and it is the
// whole reason the tier exists — crashdump, diag, logging, metrics, reqinfo,
// memlimit and version were grouped because they share it, not because they
// are small. Without the property the grouping is just a drawer, and the next
// reader has no more idea where a new helper goes than they did when internal/
// had seventeen entries.
//
// The failure it prevents is the easy one, not the exotic one: reaching for
// knomit/internal/config (or fact, or store) from inside platform compiles and
// works. logging did exactly that until this tier was created — Build took a
// config.LogConfig — and the fix was three lines at two call sites. Once ONE
// such edge is tolerated the test needs an exception list, and an exception
// list is how a layering rule stops meaning anything.
//
// The check is the strictest one available: a platform package may import the
// stdlib and external libraries (zerolog, lumberjack and prometheus are the
// point of several of them), but not one knomit package outside the tier.
func TestPlatformKnowsNothingAboutKnomit(t *testing.T) {
	const tier = "knomit/internal/platform"

	pkgs := packages(t, tier+"/...")
	if len(pkgs) == 0 {
		t.Fatalf("go list %s/... matched nothing — the guard would pass vacuously", tier)
	}
	// The package whose config edge this test was written to foreclose. If it
	// is not in the list, the pattern is wrong and the loop below proves
	// nothing.
	if !slices.Contains(pkgs, tier+"/logging") {
		t.Fatalf("%s/logging is not among %v — the guard is not watching the package it was written for", tier, pkgs)
	}
	t.Logf("checking %d packages under %s: %v", len(pkgs), tier, pkgs)

	for _, pkg := range pkgs {
		for _, dep := range deps(t, pkg) {
			if !strings.HasPrefix(dep, "knomit/") {
				continue // stdlib and external libraries are fine
			}
			if dep == tier || strings.HasPrefix(dep, tier+"/") {
				continue // within the tier
			}
			t.Errorf("%s imports %q — nothing under internal/platform may know about knomit. "+
				"The tier is defined by that property, not by the size of its members: a package "+
				"that needs a knomit type either takes it as a parameter (see logging.Options) or "+
				"does not belong here.", pkg, dep)
		}
	}
}

// TestTextnormImportsNoInternal pins internal/fact/textnorm as a leaf that
// depends on nothing else in the tree.
//
// textnorm moved UNDER internal/fact because its subject is a fact concept —
// it decides when two tags, domains or motifs are the same token — and the
// nesting only reads honestly while the child stays lighter than the parent.
//
// The obvious mistake is already foreclosed by the compiler rather than by
// this test: fact imports textnorm, so an edge back to fact is an import cycle
// and will not build. What is NOT foreclosed is the sideways edge. Reaching
// for knomit/internal/store or knomit/internal/config from a normaliser
// compiles fine and costs every consumer the whole store closure — and the
// consumers are the ones that most want it cheap: fact itself, plus store and
// synthesize, which call it per candidate pair while matching.
//
// Unlike TestParamsHasNoDependencies this is not a zero-dependency check:
// textnorm legitimately uses go-pluralize and golang.org/x/text, which is what
// makes it a normaliser rather than a string switch. The rule is only that no
// knomit package is among its dependencies.
func TestTextnormImportsNoInternal(t *testing.T) {
	const pkg = "knomit/internal/fact/textnorm"

	for _, dep := range deps(t, pkg) {
		if dep == pkg {
			continue // the package itself
		}
		if strings.HasPrefix(dep, "knomit/") {
			t.Errorf("internal/fact/textnorm imports %q — it must depend on no knomit package at all. "+
				"It sits under internal/fact only because it is LIGHTER than its parent; an edge to "+
				"the store or the config inverts that and charges fact, store and synthesize for it "+
				"on every call. External text libraries are allowed and expected.", dep)
		}
	}
}
