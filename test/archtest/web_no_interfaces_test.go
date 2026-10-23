package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"testing"
)

const webPkg = "knomit/internal/edge/web"

// webInterfaceAllowlist is every interface type internal/edge/web may still
// declare, with where it goes. The web handlers take the concrete store
// handles (svc.Search(), svc.Facts(), ...) and tests use real stores, failing
// a read through the owner's fault hook; a provider interface is a seam that
// only a stub fills. The refactor deleted them in steps (W0b..W0i), and the
// last one (originProvider) went in W0i, so the list is EMPTY. It never grows:
// a new interface in web is not added here. The stale-entry check below stays
// for the same reason the list does: an entry whose name is no longer an
// interface fails, so nothing can be parked here.
var webInterfaceAllowlist = map[string]string{}

// declaredInterfaces parses files (name -> nil to read from disk, or the source
// text) and returns (named) every type declaration whose type is an interface,
// at any scope, keyed by type name with its file, and (inline) every other
// interface literal with a non-empty method list ("file:line"): a struct
// field, parameter, result, variable or alias target. An empty interface{} is
// not a seam and is never reported inline.
func declaredInterfaces(t *testing.T, fset *token.FileSet, files map[string]any) (named map[string]string, inline []string) {
	t.Helper()
	named = map[string]string{}
	for name, src := range files {
		f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		declared := map[*ast.InterfaceType]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.TypeSpec:
				if it, ok := x.Type.(*ast.InterfaceType); ok {
					named[x.Name.Name] = name
					declared[it] = true
				}
			case *ast.InterfaceType:
				if !declared[x] && x.Methods != nil && len(x.Methods.List) > 0 {
					inline = append(inline, name+":"+strconv.Itoa(fset.Position(x.Pos()).Line))
				}
			}
			return true
		})
	}
	sort.Strings(inline)
	return named, inline
}

// TestWeb_DeclaresNoInterfacesBeyondTheAllowlist (guard W): internal/edge/web
// declares no interface type except the names in webInterfaceAllowlist, which
// is empty since W0i. A stub-only interface in the handlers is
// what the refactor removes: a handler takes the concrete type and a test
// builds a real store. Build-constrained files are scanned too (go list
// IgnoredGoFiles); test files are not (a test helper interface is not a seam
// of the package).
//
// An inline interface literal with methods anywhere else (struct field,
// parameter, result, variable, alias target, type assertion) is flagged too;
// the empty interface{} / any is allowed.
//
// Known gap: a function-typed seam (the former branchesListerFunc shape) is
// not an interface and is not detected.
//
// Non-vacuity: the file list comes from go list, not from a count here; web
// must be listed with files, and every one of them must have been parsed (a
// parse error is fatal in declaredInterfaces). With the allowlist empty, an
// empty result is the expected one (a non-empty allowlist with nothing found
// fails as stale entries), so the detector's own discrimination is
// pinned by TestWeb_InterfaceDetector_Discriminates on inline source with a
// flagged and a clean case.
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
	found, inline := declaredInterfaces(t, fset, files)

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
	for _, at := range inline {
		bad = append(bad, at+": inline interface literal with methods (struct field, parameter, result, var or alias); take the concrete type")
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Error(b)
	}
}

func TestWeb_InterfaceDetector_Discriminates(t *testing.T) {
	src := map[string]any{
		"flagged.go": `package web
type Provider interface{ Get() string }
type alias = interface{ M() }
func f() { type local interface{ L() } }
type Constraint interface{ ~int | ~string }
type S struct{ I interface{ M() } }
func g(p interface{ P() }) {}
func h() (r interface{ R() }) { return }
var v interface{ V() }
`,
		"clean.go": `package web
type F func() error
type B = S
type E struct{ X interface{}; Y any }
func k(a interface{}) {}
`,
	}
	named, inline := declaredInterfaces(t, token.NewFileSet(), src)
	for _, want := range []string{"Provider", "alias", "local", "Constraint"} {
		if named[want] != "flagged.go" {
			t.Errorf("%s not flagged as named (got %v)", want, named)
		}
	}
	if len(named) != 4 {
		t.Errorf("named %v, want exactly the four interface declarations", named)
	}
	// struct field, parameter, result, var: the four inline literals.
	want := []string{"flagged.go:6", "flagged.go:7", "flagged.go:8", "flagged.go:9"}
	if !slices.Equal(inline, want) {
		t.Errorf("inline %v, want %v", inline, want)
	}
}
