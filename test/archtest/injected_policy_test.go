package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const (
	webDir        = "internal/edge/web"
	seamFile      = webDir + "/testpolicy.go"
	authzImport   = "knomit/internal/foundation/authz"
	seamName      = "injectedPolicy"
	policyTypeIdt = "Policy"
)

// checkInjectedPolicy applies the seam guard to non-_test.go sources, keyed by
// module-root-relative slash path, and returns one message per violation.
//
// The seam (unified authn/z plan, P2) is
// `var injectedPolicy func(*http.Request) (authz.Policy, bool)` in
// internal/edge/web/testpolicy.go: nil in production, assigned only by _test.go
// files. If production code could set it, a hook that replaces authentication
// would ship. Rules, over non-test files only:
//
//  1. injectedPolicy may be mentioned exactly once: its declaration in
//     testpolicy.go, which must name a func type and carry NO initializer.
//     ANY other mention is a violation: an assignment, `p := &injectedPolicy`,
//     a read, a declaration elsewhere.
//  2. No other package-level var in internal/edge/web may hold a func that
//     returns authz.Policy, however it is spelled: a func type literal, a
//     named func type (declared in the package), an untyped initializer that is
//     a func literal, or an initializer naming a package func that returns one.
//     (Types from other packages are not resolved: aliasing authz.Policy
//     through another package would slip past, and review should catch that.)
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

	parsed := map[string]*ast.File{}
	for _, n := range names {
		f, err := parser.ParseFile(fset, n, files[n], parser.SkipObjectResolution)
		if err != nil {
			problems = append(problems, n+": does not parse: "+err.Error())
			continue
		}
		parsed[n] = f
	}

	// Rule 1.
	for _, n := range names {
		f := parsed[n]
		if f == nil {
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

	// Rule 2.
	var webFiles []string
	for _, n := range names {
		if parsed[n] != nil && strings.HasPrefix(n, webDir+"/") && !strings.Contains(strings.TrimPrefix(n, webDir+"/"), "/") {
			webFiles = append(webFiles, n)
		}
	}
	funcTypes := map[string]bool{}   // package type names whose underlying type is a func
	policyFuncs := map[string]bool{} // package funcs returning authz.Policy
	type tspec struct {
		file string
		expr ast.Expr
	}
	typeSpecs := map[string]tspec{}
	for _, n := range webFiles {
		for _, d := range parsed[n].Decls {
			switch x := d.(type) {
			case *ast.GenDecl:
				if x.Tok != token.TYPE {
					continue
				}
				for _, s := range x.Specs {
					ts := s.(*ast.TypeSpec)
					typeSpecs[ts.Name.Name] = tspec{n, ts.Type}
				}
			}
		}
	}
	// returnsPolicy reports whether ft has a result of type authz.Policy (as
	// spelled via the file's authz import name).
	returnsPolicy := func(file string, ft *ast.FuncType) bool {
		if ft == nil || ft.Results == nil {
			return false
		}
		local := authzLocalName(parsed[file])
		for _, r := range ft.Results.List {
			if sel, ok := r.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == policyTypeIdt {
				if x, ok := sel.X.(*ast.Ident); ok && local != "" && x.Name == local {
					return true
				}
			}
		}
		return false
	}
	var resolveFunc func(file string, e ast.Expr, depth int) *ast.FuncType
	resolveFunc = func(file string, e ast.Expr, depth int) *ast.FuncType {
		if depth > 8 {
			return nil
		}
		switch x := e.(type) {
		case *ast.FuncType:
			return x
		case *ast.ParenExpr:
			return resolveFunc(file, x.X, depth+1)
		case *ast.Ident:
			if ts, ok := typeSpecs[x.Name]; ok {
				return resolveFunc(ts.file, ts.expr, depth+1)
			}
		}
		return nil
	}
	for name, ts := range typeSpecs {
		if ft := resolveFunc(ts.file, ts.expr, 0); ft != nil && returnsPolicy(ts.file, ft) {
			funcTypes[name] = true
		}
	}
	for _, n := range webFiles {
		for _, d := range parsed[n].Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && returnsPolicy(n, fd.Type) {
				policyFuncs[fd.Name.Name] = true
			}
		}
	}
	for _, n := range webFiles {
		for _, d := range parsed[n].Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, s := range gd.Specs {
				vs := s.(*ast.ValueSpec)
				holds := false
				if vs.Type != nil {
					if ft := resolveFunc(n, vs.Type, 0); ft != nil && returnsPolicy(n, ft) {
						holds = true
					}
				}
				for _, v := range vs.Values {
					switch x := v.(type) {
					case *ast.FuncLit:
						if returnsPolicy(n, x.Type) {
							holds = true
						}
					case *ast.Ident:
						if policyFuncs[x.Name] {
							holds = true
						}
					}
				}
				if !holds {
					continue
				}
				for _, id := range vs.Names {
					if id.Name == "_" || (n == seamFile && id.Name == seamName) {
						continue
					}
					add(id.Pos(), "package-level var "+id.Name+" holds a func returning authz.Policy: a second policy seam; the only one is "+seamName+" in testpolicy.go")
				}
			}
		}
	}
	return problems
}

// authzLocalName is the name f gives the authz package, or "".
func authzLocalName(f *ast.File) string {
	if f == nil {
		return ""
	}
	for _, imp := range f.Imports {
		if strings.Trim(imp.Path.Value, `"`) != authzImport {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return "authz"
	}
	return ""
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
			webDir + "/other.go": "package web\n\nimport (\n\t\"net/http\"\n\n\t\"knomit/internal/foundation/authz\"\n)\n\nvar _ = http.StatusOK\nvar _ authz.Policy\n\n" + extra,
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
		{"(c) second seam of a named func type", web("type policyHook func(*http.Request) (authz.Policy, bool)\n\nvar altPolicy policyHook\n"), "second policy seam"},
		{"(d) untyped initializer", web("var inferredPolicy = func(*http.Request) (authz.Policy, bool) { return authz.Policy{}, false }\n"), "second policy seam"},
		{"second seam by naming a package func", web("func hook(*http.Request) (authz.Policy, bool) { return authz.Policy{}, false }\n\nvar viaFunc = hook\n"), "second policy seam"},
		{"second seam under an import alias", map[string]string{
			seamFile:             goodSeamFile,
			webDir + "/alias.go": "package web\n\nimport az \"knomit/internal/foundation/authz\"\n\nvar aliased func() az.Policy\n",
		}, "second policy seam"},
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
