package archtest

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	gopackages "golang.org/x/tools/go/packages"
)

const (
	webDir      = "internal/edge/web"
	seamPkg     = "knomit/internal/edge/web"
	seamFile    = webDir + "/testpolicy.go"
	authzImport = "knomit/internal/foundation/authz"
	seamName    = "injectedPolicy"
)

// The in-process auth test seam (unified authn/z plan, P2) is
// `var injectedPolicy func(*http.Request) (authz.Policy, bool)` in
// internal/edge/web/testpolicy.go: nil in production, assigned only by _test.go
// files. If production code could set it, a hook that replaces authentication
// would ship. Three checks pin that.
//
// Rule 1 (checkInjectedPolicy, syntactic, non-_test.go files under internal/):
// injectedPolicy may be mentioned exactly once, in its declaration in
// testpolicy.go, which must name a func type and carry NO initializer. Any
// other mention (assignment, `p := &injectedPolicy`, a read, a declaration
// elsewhere) fails.
//
// Rule 2 (policyVarViolations, type-aware): in the non-test code of
// internal/edge/web and every package below it, no package-level var other than
// the seam itself may have a type that REACHES authz.Policy. Reaching walks
// aliases, named types (their underlying type and type arguments), pointers,
// slices, arrays, maps, channels, signatures (parameters and results), struct
// fields and interface methods, and treats a local named type whose underlying
// type is Policy's (`type P authz.Policy`) as Policy. The type is the checker's,
// so spelling does not matter: a local alias, a dot import, an inferred type
// from a call, a method value and a generic func type all resolve.
//
// What rule 2 does NOT catch, and review must: a Policy held through
// atomic.Value or any other any-typed cell (the type says "any"); a field added
// to an existing struct type such as Server (rule 2 looks at package-level
// vars, not at the fields of types declared with them, unless a var has such a
// type); a package outside internal/edge/web; and a func var that returns
// something convertible to a policy without naming authz.Policy.
//
// Rule 3 (linknameViolations): a //go:linkname naming injectedPolicy anywhere in
// the module, test files included.

// checkInjectedPolicy is rule 1 over non-_test.go sources keyed by
// module-root-relative slash path.
func checkInjectedPolicy(files map[string]string) []string {
	fset := token.NewFileSet()
	var problems []string
	add := func(p token.Pos, msg string) {
		pos := fset.Position(p)
		problems = append(problems, pos.Filename+":"+strconv.Itoa(pos.Line)+": "+msg)
	}

	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, n := range names {
		f, err := parser.ParseFile(fset, n, files[n], parser.SkipObjectResolution)
		if err != nil {
			problems = append(problems, n+": does not parse: "+err.Error())
			continue
		}
		var declIdent *ast.Ident
		if n == seamFile {
			for _, d := range f.Decls {
				gd, ok := d.(*ast.GenDecl)
				if !ok || gd.Tok != token.VAR {
					continue
				}
				for _, s := range gd.Specs {
					vs := s.(*ast.ValueSpec)
					for _, id := range vs.Names {
						if id.Name != seamName {
							continue
						}
						if declIdent != nil {
							add(id.Pos(), "second declaration of "+seamName)
							continue
						}
						declIdent = id
						if _, isFunc := vs.Type.(*ast.FuncType); !isFunc {
							add(id.Pos(), seamName+" must be declared with a func type")
						}
						if len(vs.Values) != 0 || len(vs.Names) != 1 {
							add(id.Pos(), seamName+" must be declared without an initializer: it is nil in production")
						}
					}
				}
			}
		}
		ast.Inspect(f, func(node ast.Node) bool {
			id, ok := node.(*ast.Ident)
			if ok && id.Name == seamName && id != declIdent {
				add(id.Pos(), "mentions "+seamName+" in a non-test file; only its declaration in "+seamFile+" may")
			}
			return true
		})
	}
	return problems
}

// reachesPolicy reports whether t mentions authz.Policy anywhere (see rule 2).
// under is Policy's underlying type as the package under inspection sees it, or
// nil when that package does not import authz.
func reachesPolicy(t types.Type, under types.Type, seen map[types.Type]bool) bool {
	if t == nil {
		return false
	}
	t = types.Unalias(t)
	if seen[t] {
		return false
	}
	seen[t] = true
	walk := func(ts ...types.Type) bool {
		for _, x := range ts {
			if reachesPolicy(x, under, seen) {
				return true
			}
		}
		return false
	}
	switch x := t.(type) {
	case *types.Named:
		if o := x.Obj(); o != nil && o.Pkg() != nil && o.Pkg().Path() == authzImport && o.Name() == "Policy" {
			return true
		}
		if under != nil && types.Identical(x.Underlying(), under) {
			return true // type P authz.Policy
		}
		targs := x.TypeArgs()
		for i := 0; i < targs.Len(); i++ {
			if walk(targs.At(i)) {
				return true
			}
		}
		return walk(x.Underlying())
	case *types.Pointer:
		return walk(x.Elem())
	case *types.Slice:
		return walk(x.Elem())
	case *types.Array:
		return walk(x.Elem())
	case *types.Chan:
		return walk(x.Elem())
	case *types.Map:
		return walk(x.Key(), x.Elem())
	case *types.Signature:
		for i := 0; i < x.Params().Len(); i++ {
			if walk(x.Params().At(i).Type()) {
				return true
			}
		}
		for i := 0; i < x.Results().Len(); i++ {
			if walk(x.Results().At(i).Type()) {
				return true
			}
		}
	case *types.Struct:
		for i := 0; i < x.NumFields(); i++ {
			if walk(x.Field(i).Type()) {
				return true
			}
		}
	case *types.Interface:
		for i := 0; i < x.NumMethods(); i++ {
			if walk(x.Method(i).Type()) {
				return true
			}
		}
	case *types.Tuple:
		for i := 0; i < x.Len(); i++ {
			if walk(x.At(i).Type()) {
				return true
			}
		}
	}
	return false
}

// policyUnder is authz.Policy's underlying type as seen from pkg's imports, or
// nil if pkg does not import authz.
func policyUnder(pkg *types.Package) types.Type {
	for _, imp := range pkg.Imports() {
		if imp.Path() != authzImport {
			continue
		}
		if o := imp.Scope().Lookup("Policy"); o != nil {
			return o.Type().Underlying()
		}
	}
	return nil
}

// policyVarViolations is rule 2 for one type-checked package: every
// package-level var whose type reaches authz.Policy, except the seam itself
// when pkg is internal/edge/web.
func policyVarViolations(pkg *types.Package) []string {
	under := policyUnder(pkg)
	var out []string
	scope := pkg.Scope()
	for _, name := range scope.Names() {
		v, ok := scope.Lookup(name).(*types.Var)
		if !ok || name == "_" {
			continue
		}
		if pkg.Path() == seamPkg && name == seamName {
			continue
		}
		if reachesPolicy(v.Type(), under, map[types.Type]bool{}) {
			out = append(out, pkg.Path()+"."+name+" has type "+v.Type().String()+", which reaches authz.Policy: a second policy seam; the only one is "+seamName+" in testpolicy.go")
		}
	}
	return out
}

// linknameViolations scans raw file text for //go:linkname directives that name
// the seam from anywhere, since linkname can reach an unexported var of another
// package without mentioning it by selector.
func linknameViolations(name, src string) []string {
	var out []string
	for i, line := range strings.Split(src, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "//go:linkname") && strings.Contains(t, seamName) {
			out = append(out, name+":"+strconv.Itoa(i+1)+": //go:linkname names "+seamName)
		}
	}
	return out
}

const goodSeamFile = `package web

import (
	"net/http"

	"knomit/internal/foundation/authz"
)

var injectedPolicy func(*http.Request) (authz.Policy, bool)
`

func TestInjectedPolicy_GuardShapes(t *testing.T) {
	web := func(extra string) map[string]string {
		return map[string]string{
			seamFile:             goodSeamFile,
			webDir + "/other.go": "package web\n\n" + extra,
		}
	}
	if got := checkInjectedPolicy(web("")); len(got) != 0 {
		t.Fatalf("clean tree flagged: %v", got)
	}

	for _, tc := range []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"direct assignment", web("func init() { injectedPolicy = nil }\n"), "mentions injectedPolicy"},
		{"(a) assignment through a pointer", web("func init() {\n\tp := &injectedPolicy\n\t*p = nil\n}\n"), "mentions injectedPolicy"},
		{"(b) initializer on the declaration", map[string]string{seamFile: "package web\n\nimport (\n\t\"net/http\"\n\n\t\"knomit/internal/foundation/authz\"\n)\n\nvar injectedPolicy func(*http.Request) (authz.Policy, bool) = func(*http.Request) (authz.Policy, bool) {\n\treturn authz.Policy{Owner: true}, true\n}\n"}, "without an initializer"},
		{"declared in another file", map[string]string{
			seamFile:            goodSeamFile,
			webDir + "/more.go": "package web\n\nvar injectedPolicy func()\n",
		}, "mentions injectedPolicy"},
		{"read from another package", map[string]string{
			seamFile:                    goodSeamFile,
			"internal/edge/mcp/peek.go": "package mcp\n\nfunc f() bool { return web.injectedPolicy != nil }\n",
		}, "mentions injectedPolicy"},
	} {
		got := checkInjectedPolicy(tc.files)
		if len(got) == 0 {
			t.Errorf("%s: guard stayed green", tc.name)
			continue
		}
		if !strings.Contains(strings.Join(got, "\n"), tc.want) {
			t.Errorf("%s: flagged, but not for %q: %v", tc.name, tc.want, got)
		}
	}

	// (e) linkname.
	src := "package evil\n\nimport _ \"unsafe\"\n\n//go:linkname hook knomit/internal/edge/web.injectedPolicy\nvar hook func()\n"
	if got := linknameViolations("internal/evil/x.go", src); len(got) != 1 {
		t.Errorf("(e) go:linkname naming injectedPolicy: %v", got)
	}
	if got := linknameViolations("x.go", "//go:linkname a b.other\n// injectedPolicy in prose\n"); len(got) != 0 {
		t.Errorf("unrelated linkname flagged: %v", got)
	}
}

// fakeAuthz stands in for knomit/internal/foundation/authz so the self-test
// type-checks small snippets without loading the module.
const fakeAuthz = `package authz

type Policy struct{ Owner bool }

type Principal struct{ ID string }
`

type fixtureImporter struct {
	authz *types.Package
	std   types.Importer
}

func (f fixtureImporter) Import(path string) (*types.Package, error) {
	if path == authzImport {
		return f.authz, nil
	}
	return f.std.Import(path)
}

func typeCheckFixture(t *testing.T, src string) *types.Package {
	t.Helper()
	fset := token.NewFileSet()
	std := importer.ForCompiler(fset, "source", nil)

	af, err := parser.ParseFile(fset, "authz.go", fakeAuthz, 0)
	if err != nil {
		t.Fatal(err)
	}
	az, err := (&types.Config{}).Check(authzImport, fset, []*ast.File{af}, nil)
	if err != nil {
		t.Fatal(err)
	}

	f, err := parser.ParseFile(fset, "fixture.go", src, 0)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	pkg, err := (&types.Config{Importer: fixtureImporter{az, std}}).Check(seamPkg, fset, []*ast.File{f}, nil)
	if err != nil {
		t.Fatalf("type-check fixture: %v\n%s", err, src)
	}
	return pkg
}

// TestInjectedPolicy_TypeWalkerShapes checks rule 2 on constructed types: each
// shape must be flagged, each innocent var must not be.
func TestInjectedPolicy_TypeWalkerShapes(t *testing.T) {
	const header = `package web

import (
	"net/http"
	"sync/atomic"

	"knomit/internal/foundation/authz"
)

var _ = http.StatusOK
var _ atomic.Int32

func mk() func() authz.Policy { return nil }

type holder struct{ f func(*http.Request) authz.Policy }
type reader interface{ Policy() authz.Policy }
type hook[T any] func(T) authz.Policy
type localAlias = authz.Policy
type localNamed authz.Policy
type srv struct{}

func (srv) get() authz.Policy { return authz.Policy{} }

var theServer srv
`
	flagged := map[string]string{
		"plainFunc":     "var plainFunc func(*http.Request) (authz.Policy, bool)",
		"ptrResult":     "var ptrResult func() *authz.Policy",
		"callInit":      "var callInit = mk()",
		"ptrToFunc":     "var ptrToFunc *func() authz.Policy",
		"sliceOfFunc":   "var sliceOfFunc []func() authz.Policy",
		"mapOfFunc":     "var mapOfFunc map[string]func() authz.Policy",
		"atomicPointer": "var atomicPointer atomic.Pointer[func() authz.Policy]",
		"ifaceVar":      "var ifaceVar reader",
		"structVar":     "var structVar holder",
		"aliasResult":   "var aliasResult func() localAlias",
		"namedResult":   "var namedResult func() localNamed",
		"genericHook":   "var genericHook hook[int]",
		"methodValue":   "var methodValue = theServer.get",
		"policyItself":  "var policyItself authz.Policy",
		"paramOnly":     "var paramOnly func(authz.Policy)",
	}
	clean := map[string]string{
		"anInt":      "var anInt int",
		"plainFn":    "var plainFn func(*http.Request) bool",
		"principal":  "var principal authz.Principal",
		"atomicInt":  "var atomicInt atomic.Int32",
		"mapOfStr":   "var mapOfStr map[string][]string",
		"emptyIface": "var emptyIface any",
	}
	var body strings.Builder
	body.WriteString(header)
	for _, d := range flagged {
		body.WriteString(d + "\n")
	}
	for _, d := range clean {
		body.WriteString(d + "\n")
	}
	pkg := typeCheckFixture(t, body.String())
	got := strings.Join(policyVarViolations(pkg), "\n")
	for name := range flagged {
		if !strings.Contains(got, "web."+name+" ") {
			t.Errorf("%s: type walker stayed green", name)
		}
	}
	for name := range clean {
		if strings.Contains(got, "web."+name+" ") {
			t.Errorf("%s: innocent var flagged", name)
		}
	}

	// The seam itself is exempt only as injectedPolicy in the web package.
	pkg = typeCheckFixture(t, header+"var injectedPolicy func() authz.Policy\n")
	if got := policyVarViolations(pkg); len(got) != 0 {
		t.Errorf("the seam was flagged: %v", got)
	}
}

// TestInjectedPolicy_AssignedOnlyFromTestFiles runs the guard over the real
// tree.
func TestInjectedPolicy_AssignedOnlyFromTestFiles(t *testing.T) {
	root := moduleRoot(t)
	prod := map[string]string{}
	var problems []string

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "third_party", "dist", ".claude":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		// go:linkname is checked everywhere, test files included.
		problems = append(problems, linknameViolations(rel, string(b))...)
		if strings.HasPrefix(rel, "internal/") && !strings.HasSuffix(path, "_test.go") {
			prod[rel] = string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	problems = append(problems, checkInjectedPolicy(prod)...)
	if _, ok := prod[seamFile]; !ok {
		problems = append(problems, seamFile+": missing; it declares the seam")
	}
	for _, p := range problems {
		t.Error(p)
	}
}

// TestInjectedPolicy_NoSecondSeamInTheWebTree is rule 2 over the real
// internal/edge/web tree, type-checked.
func TestInjectedPolicy_NoSecondSeamInTheWebTree(t *testing.T) {
	cfg := &gopackages.Config{
		Dir: moduleRoot(t),
		Mode: gopackages.NeedName | gopackages.NeedFiles | gopackages.NeedCompiledGoFiles | gopackages.NeedImports |
			gopackages.NeedTypes | gopackages.NeedSyntax | gopackages.NeedTypesInfo,
	}
	pkgs, err := gopackages.Load(cfg, seamPkg+"/...")
	if err != nil {
		t.Fatalf("gopackages.Load: %v", err)
	}
	sawSeam := false
	for _, p := range pkgs {
		if len(p.Errors) > 0 {
			t.Fatalf("type-checking %s: %v", p.PkgPath, p.Errors)
		}
		for _, v := range policyVarViolations(p.Types) {
			t.Error(v)
		}
		if p.PkgPath == seamPkg {
			if o := p.Types.Scope().Lookup(seamName); o != nil {
				// The walker must see the seam as a policy holder, or a
				// green run here proves nothing.
				sawSeam = reachesPolicy(o.Type(), policyUnder(p.Types), map[types.Type]bool{})
			}
		}
	}
	if !sawSeam {
		t.Errorf("%s.%s not found, or the type walker does not see authz.Policy in it", seamPkg, seamName)
	}
}
