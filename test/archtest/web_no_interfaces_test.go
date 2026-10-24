package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const webPkg = "knomit/internal/edge/web"

// webInterfaceAllowlist is every interface type a package of the web family
// (internal/edge/web and every package under it) may still declare, keyed by
// "<import path>.<Name>", with where it goes. The web handlers take the
// concrete store handles (svc.Search(), svc.Facts(), ...) and tests use real
// stores, failing a read through the owner's fault hook; a provider interface
// is a seam that only a stub fills. The refactor deleted them in steps
// (W0b..W0i), and the last one (originProvider) went in W0i, so the list is
// EMPTY. It never grows: a new interface in web or in a child of it (hal, and
// the httpx and api/* children of the web split) is not added here. The
// stale-entry check below stays for the same reason the list does: an entry
// whose name is no longer an interface fails, so nothing can be parked here.
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

// webKnownTypes are, per package of the web family (keyed by import path),
// concrete (non-interface) types it declares, each in a different file, with
// that file. The guard requires the scan of each package to have seen every
// one where it lives: a scan of the wrong package, of one file only, or of
// files read as empty fails here instead of passing with nothing found.
//
// Every package `go list knomit/internal/edge/web/...` returns must have an
// entry (a new child of web, such as httpx or api/kb in the web split, adds
// its own in the PR that creates it), and an entry for a package go list does
// not return fails as stale. Moving one of these types to another package
// (repoSummary and originView are planned to move to api/manage) means moving
// its entry, NOT deleting the check.
var webKnownTypes = map[string]map[string]string{
	webPkg: {
		"Server":      "server.go",
		"originView":  "handlers_origin_hal.go",
		"repoSummary": "handlers_repos_hal.go",
	},
	webPkg + "/hal": {
		"Anchor":         "anchor.go",
		"CollectionView": "collection.go",
		"Link":           "link.go",
		"Problem":        "problem.go",
	},
}

// isWebFamily reports whether an import path is internal/edge/web or a
// package under it.
func isWebFamily(p string) bool {
	return p == webPkg || strings.HasPrefix(p, webPkg+"/")
}

// webFamily returns the go list rows of internal/edge/web and every package
// under it (non-test files), sorted by import path, web first. It fails when
// web itself is missing or has no file: every web-family guard would examine
// nothing.
func webFamily(t *testing.T) []goListPackage {
	t.Helper()
	var out []goListPackage
	for _, p := range listModulePackages(t) {
		if isWebFamily(p.Path) {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	if len(out) == 0 || out[0].Path != webPkg || len(out[0].Files) == 0 {
		t.Fatalf("go list did not return %s with Go files: the guard would examine nothing", webPkg)
	}
	return out
}

// TestWeb_DeclaresNoInterfacesBeyondTheAllowlist (guard W): no package of the
// web family (internal/edge/web and every package under it, as go list
// returns them: hal today, httpx and api/* after the web split) declares an
// interface type except the names in webInterfaceAllowlist, which is empty
// since W0i. A stub-only interface in the handlers is what the refactor
// removes: a handler takes the concrete type and a test builds a real store.
// Build-constrained files are scanned too (go list IgnoredGoFiles); test files
// are not (a test helper interface is not a seam of the package).
//
// An inline interface literal with methods anywhere else (struct field,
// parameter, result, variable, alias target, type assertion) is flagged too;
// the empty interface{} / any is allowed.
//
// Known gap: a function-typed seam (the former branchesListerFunc shape) is
// not an interface and is not detected.
//
// Non-vacuity: with the allowlist empty, finding no interface is the expected
// answer, so the guard proves it read each real package instead: the package
// list and each file list come from go list (web must be listed with files,
// and every listed package must have files), the scanned file count of each
// package must equal go list's, every file must parse (fatal in
// declaredInterfaces) with the package clause that is the last element of its
// import path, and the same pass must have seen each of the package's
// webKnownTypes in its own file. A listed package with no webKnownTypes entry
// fails, and so does an entry for a package go list does not return. The
// detector's own discrimination is pinned by
// TestWeb_InterfaceDetector_Discriminates on inline source with a flagged and
// a clean case.
func TestWeb_DeclaresNoInterfacesBeyondTheAllowlist(t *testing.T) {
	family := webFamily(t)
	found := map[string]string{} // "<import path>.<Name>" -> file
	listed := map[string]bool{}
	var bad []string
	for _, pkg := range family {
		listed[pkg.Path] = true
		known := webKnownTypes[pkg.Path]
		if len(known) == 0 {
			t.Errorf("%s: no webKnownTypes entry: add concrete types it declares (each in its own file) so its scan is provably not vacuous", pkg.Path)
		}
		if len(pkg.Files) == 0 {
			t.Errorf("go list returned %s with no Go files: the guard would examine nothing there", pkg.Path)
			continue
		}
		files := map[string]any{}
		for _, f := range pkg.Files {
			files[filepath.Join(pkg.Dir, f)] = nil
		}
		named, inline, scan := declaredInterfaces(t, token.NewFileSet(), files)
		if len(scan.pkgs) != len(pkg.Files) {
			t.Errorf("%s: scanned %d files, go list returned %d", pkg.Path, len(scan.pkgs), len(pkg.Files))
		}
		clause := pkg.Path[strings.LastIndex(pkg.Path, "/")+1:]
		for file, got := range scan.pkgs {
			if got != clause {
				t.Errorf("%s: package clause %q, want %s: the guard is scanning the wrong package", file, got, clause)
			}
		}
		for typ, want := range known {
			if got, ok := scan.types[typ]; !ok || filepath.Base(got) != want {
				t.Errorf("non-vacuity: %s: type %s not seen in %s (got %q): the scan did not read the package's files", pkg.Path, typ, want, got)
			}
		}
		for name, file := range named {
			found[pkg.Path+"."+name] = file
		}
		for _, at := range inline {
			bad = append(bad, at+": inline interface literal with methods (struct field, parameter, result, var or alias); take the concrete type")
		}
	}
	for p := range webKnownTypes {
		if !listed[p] {
			t.Errorf("webKnownTypes has an entry for %s, which go list does not return: move the entry with its types or delete it", p)
		}
	}

	for key, file := range found {
		if _, ok := webInterfaceAllowlist[key]; !ok {
			bad = append(bad, key+" ("+filepath.Base(file)+"): the web family declares an interface; take the concrete type and test against a real store (see the owner's SetReadFaultForTest for failures)")
		}
	}
	for key := range webInterfaceAllowlist {
		if _, ok := found[key]; !ok {
			bad = append(bad, key+": stale webInterfaceAllowlist entry; the interface is gone, delete the entry")
		}
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
	// isWebFamily: web and packages under it, not a path that merely shares
	// the prefix.
	for p, want := range map[string]bool{webPkg: true, webPkg + "/hal": true, webPkg + "/api/kb": true, webPkg + "x": false, "knomit/internal/edge/mcp": false} {
		if isWebFamily(p) != want {
			t.Errorf("isWebFamily(%s) = %v, want %v", p, !want, want)
		}
	}
}
