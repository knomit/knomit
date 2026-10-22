package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"testing"
)

const webPkg = "knomit/internal/edge/web"

// webInterfaceAllowlist is every interface type internal/edge/web may still
// declare, with where it goes. The web handlers take the concrete store
// handles (svc.Search(), svc.Facts(), ...) and tests use real stores, failing
// a read through the owner's fault hook; a provider interface is a seam that
// only a stub fills. The refactor deletes them in steps (W0b..W0i), so the
// list shrinks and never grows: an entry is removed in the PR that deletes the
// interface, a new interface in web is not added here. A stale entry (the name
// is no longer an interface) fails, so a deletion cannot leave its entry
// behind.
var webInterfaceAllowlist = map[string]string{
	"FactReader":              "provider seam, deleted with the fact-read handler's rewrite",
	"FactWriter":              "provider seam, deleted with the fact-write handler's rewrite",
	"TopicLister":             "provider seam, deleted with the topics handler's rewrite",
	"searchProvider":          "provider seam, deleted with the search handler's rewrite",
	"commitsProvider":         "provider seam, deleted with the commits handler's rewrite",
	"factSubProvider":         "provider seam, deleted with the fact sub-resource handler's rewrite",
	"statsProvider":           "provider seam, deleted with the stats handler's rewrite",
	"motifsProvider":          "provider seam, deleted with the motifs handler's rewrite",
	"factsCollectionProvider": "provider seam, deleted with the facts-collection handler's rewrite",
	"activityProvider":        "provider seam, deleted with the activity handler's rewrite",
	"originProvider":          "provider seam, deleted with the origin handler's rewrite",
	"RefResolver":             "ref_view.go's resolver seam, deleted with its rewrite",
}

// declaredInterfaces parses files (name -> nil to read from disk, or the source
// text) and returns the name of every interface type declared at any scope
// (package level or inside a function), with the file it is in. A type
// declaration whose type is an interface literal is what counts: a generic
// constraint declared as `type C interface{ ~int }` is one too.
func declaredInterfaces(t *testing.T, fset *token.FileSet, files map[string]any) map[string]string {
	t.Helper()
	out := map[string]string{}
	for name, src := range files {
		f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			if _, ok := ts.Type.(*ast.InterfaceType); ok {
				out[ts.Name.Name] = name
			}
			return true
		})
	}
	return out
}

// TestWeb_DeclaresNoInterfacesBeyondTheAllowlist (guard W): internal/edge/web
// declares no interface type except the provider seams still awaiting
// deletion in webInterfaceAllowlist. A stub-only interface in the handlers is
// what the refactor removes: a handler takes the concrete type and a test
// builds a real store. Build-constrained files are scanned too (go list
// IgnoredGoFiles); test files are not (a test helper interface is not a seam
// of the package).
//
// Known gap: an interface type that is not declared with its own type
// declaration (an inline `interface{ M() }` in a signature or a type
// assertion) is not matched.
//
// Non-vacuity: the file list comes from go list, not from a count here; web
// must be listed with files, and every one of them must have been parsed. The
// self-test runs the detector on inline source with a flagged and a clean case.
func TestWeb_DeclaresNoInterfacesBeyondTheAllowlist(t *testing.T) {
	var web *goListPackage
	for _, p := range listModulePackages(t) {
		if p.Path == webPkg {
			p := p
			web = &p
		}
	}
	if web == nil || len(web.Files) == 0 {
		t.Fatalf("go list did not return %s with Go files: the guard would examine nothing", webPkg)
	}
	files := map[string]any{}
	for _, f := range web.Files {
		files[filepath.Join(web.Dir, f)] = nil
	}
	fset := token.NewFileSet()
	found := declaredInterfaces(t, fset, files)

	var bad []string
	for name, file := range found {
		if _, ok := webInterfaceAllowlist[name]; !ok {
			bad = append(bad, name+" ("+filepath.Base(file)+"): web declares an interface; take the concrete type and test against a real store (see the owner's SetReadFaultForTest for failures)")
		}
	}
	for name := range webInterfaceAllowlist {
		if _, ok := found[name]; !ok {
			bad = append(bad, name+": stale webInterfaceAllowlist entry; the interface is gone, delete the entry")
		}
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Error(b)
	}
	if len(found) == 0 {
		t.Error("no interface found in web at all: with the allowlist non-empty the parse is broken")
	}
}

func TestWeb_InterfaceDetector_Discriminates(t *testing.T) {
	src := map[string]any{
		"flagged.go": `package web
type Provider interface{ Get() string }
type alias = interface{ M() }
func f() { type local interface{ L() } }
type Constraint interface{ ~int | ~string }
`,
		"clean.go": `package web
type S struct{ I interface{ M() } }
type F func() error
type A = S
var _ interface{ M() }
`,
	}
	got := declaredInterfaces(t, token.NewFileSet(), src)
	for _, want := range []string{"Provider", "alias", "local", "Constraint"} {
		if got[want] != "flagged.go" {
			t.Errorf("%s not flagged (got %v)", want, got)
		}
	}
	for _, name := range []string{"S", "F", "A"} {
		if _, bad := got[name]; bad {
			t.Errorf("%s flagged but is not an interface type declaration", name)
		}
	}
	if len(got) != 4 {
		t.Errorf("flagged %v, want exactly the four interface declarations", got)
	}
}
