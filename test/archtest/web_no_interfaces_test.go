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

// typeScan is what one pass over a package's files saw: every declared type
// name with its file (types, interfaces included), and each file's package
// clause (pkgs). The guard uses both for non-vacuity: with the allowlist empty,
// "no interface found" is the expected answer, so the pass must prove it read
// the real package.
type typeScan struct {
	types map[string]string
	pkgs  map[string]string
}

// declaredInterfaces parses files (name -> nil to read from disk, or the source
// text) and returns (named) every type declaration whose type is an interface,
// at any scope, keyed by type name with its file, (inline) every other
// interface literal with a non-empty method list ("file:line"): a struct
// field, parameter, result, variable or alias target, and (scan) every type
// name and package clause the same pass saw. An empty interface{} is not a
// seam and is never reported inline.
func declaredInterfaces(t *testing.T, fset *token.FileSet, files map[string]any) (named map[string]string, inline []string, scan typeScan) {
	t.Helper()
	named = map[string]string{}
	scan = typeScan{types: map[string]string{}, pkgs: map[string]string{}}
	for name, src := range files {
		f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		scan.pkgs[name] = f.Name.Name
		declared := map[*ast.InterfaceType]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.TypeSpec:
				scan.types[x.Name.Name] = name
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
	return named, inline, scan
}

// webKnownTypes are concrete (non-interface) types web declares, each in a
// different file, with that file. The guard requires the scan to have seen
// every one where it lives: a scan of the wrong package, of one file only, or
// of files read as empty fails here instead of passing with nothing found.
var webKnownTypes = map[string]string{
	"Server":      "server.go",
	"originView":  "handlers_origin_hal.go",
	"repoSummary": "handlers_repos_hal.go",
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
// Non-vacuity: with the allowlist empty, finding no interface is the expected
// answer, so the guard proves it read the real package instead: the file list
// comes from go list (web must be listed with files), every file must parse
// (fatal in declaredInterfaces) with package clause `web`, and the same pass
// must have seen each of webKnownTypes in its own file. The detector's own
// discrimination is pinned by TestWeb_InterfaceDetector_Discriminates on
// inline source with a flagged and a clean case.
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
	found, inline, scan := declaredInterfaces(t, fset, files)
	if len(scan.pkgs) != len(web.Files) {
		t.Fatalf("scanned %d files, go list returned %d", len(scan.pkgs), len(web.Files))
	}
	for file, pkg := range scan.pkgs {
		if pkg != "web" {
			t.Errorf("%s: package clause %q, want web: the guard is scanning the wrong package", file, pkg)
		}
	}
	for typ, want := range webKnownTypes {
		if got, ok := scan.types[typ]; !ok || filepath.Base(got) != want {
			t.Errorf("non-vacuity: type %s not seen in %s (got %q): the scan did not read web's files", typ, want, got)
		}
	}

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
	named, inline, scan := declaredInterfaces(t, token.NewFileSet(), src)
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
	// The scan sees every type, interface or not, in its own file, and each
	// file's package clause.
	for typ, file := range map[string]string{"Provider": "flagged.go", "S": "flagged.go", "F": "clean.go", "B": "clean.go", "E": "clean.go"} {
		if scan.types[typ] != file {
			t.Errorf("scan.types[%s] = %q, want %q", typ, scan.types[typ], file)
		}
	}
	if scan.pkgs["flagged.go"] != "web" || scan.pkgs["clean.go"] != "web" || len(scan.pkgs) != 2 {
		t.Errorf("scan.pkgs %v, want both files in package web", scan.pkgs)
	}
}
