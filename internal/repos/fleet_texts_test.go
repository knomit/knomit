package repos

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"knomit/internal/testsupport/modroot"
)

// restRoute is a REST route as a refusal text might name one.
var restRoute = regexp.MustCompile(`\b(GET|POST|PUT|PATCH|DELETE) /|/api/v1/`)

// logMethods are zerolog's event methods: a literal passed to one is a log
// line or a log field, read by an operator in the log, never shown on the
// Fleet page.
var logMethods = map[string]bool{"Msg": true, "Msgf": true, "Str": true, "Strs": true, "Err": true, "Int": true, "Bool": true, "Dur": true}

// goFiles parses the non-test Go files matching pattern.
func goFiles(t *testing.T, fset *token.FileSet, pattern string) []*ast.File {
	t.Helper()
	names, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatal(err)
	}
	var out []*ast.File
	for _, n := range names {
		if strings.HasSuffix(n, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, n, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, af)
	}
	if len(out) == 0 {
		t.Fatalf("no Go files match %s", pattern)
	}
	return out
}

// literalText is a string literal's value; ok is false for any other node.
func literalText(t *testing.T, fset *token.FileSet, n ast.Node) (string, bool) {
	lit, ok := n.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		t.Fatalf("%s: %v", fset.Position(lit.Pos()), err)
	}
	return s, true
}

// The fleet's refusal and error texts are shown verbatim on the Fleet page,
// so they name the page's action (e.g. "Fleet → Settings → Leave"), never a
// REST route.
//
// In the fleet files of this package EVERY string literal is checked, a
// const's or a variable's as much as one written inside the call, except an
// import path, a struct tag and a log line (logMethods). The counts are
// asserted so a scan that reads nothing cannot pass.
//
// SABOTAGE: put "(POST /api/v1/fleet/leave)" back into refuseFleetPresent ->
// red; the same text held in a `const msg` and passed as fleetErr(..., msg)
// (PR #527 N2) -> red.
func TestFleetTexts_NameNoRESTRoutes(t *testing.T) {
	fset := token.NewFileSet()
	files := goFiles(t, fset, "fleet*.go")
	checked := 0
	for _, af := range files {
		skip := map[ast.Node]bool{}
		ast.Inspect(af, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.ImportSpec:
				skip[x.Path] = true
			case *ast.Field:
				if x.Tag != nil {
					skip[x.Tag] = true
				}
			case *ast.CallExpr:
				if sel, ok := x.Fun.(*ast.SelectorExpr); ok && logMethods[sel.Sel.Name] {
					for _, a := range x.Args {
						ast.Inspect(a, func(m ast.Node) bool { skip[m] = true; return true })
					}
				}
			}
			if skip[n] {
				return true
			}
			s, ok := literalText(t, fset, n)
			if !ok {
				return true
			}
			checked++
			if restRoute.MatchString(s) {
				t.Errorf("%s: the text %q names a REST route; name the Fleet page's action instead", fset.Position(n.Pos()), s)
			}
			return true
		})
	}
	if checked < 200 {
		t.Fatalf("checked %d texts in %d files, want every literal of the fleet files (at least 200)", checked, len(files))
	}
}

// The pki and oauth texts that reach the Fleet page and the Authorizations
// page verbatim (an origin this instance cannot fetch, pki.ErrNotEnrolled; a
// signed approval with no fleet root, oauth.ErrNoFleetRoot; every other
// error those packages make) name no REST route either (PR #527). Every
// literal passed to errors.New or fmt.Errorf is checked, and so is an
// identifier passed there that names a const or var of the package holding
// a literal. Those packages also hold real route patterns (oauth's mux) and
// log lines, which are not texts a page shows, so only error texts are
// scanned. The two named texts must have been checked.
//
// SABOTAGE: put "(POST /api/v1/fleet/join)" into pki.ErrNotEnrolled or
// oauth.ErrNoFleetRoot -> red.
func TestFleetTexts_PKIAndOAuthErrorsNameNoRESTRoutes(t *testing.T) {
	for _, pkg := range []struct {
		dir  string
		must string
	}{
		// From the module root: a layer move changes the depth and the sibling
		// names, so "../pki" would break with it. goFiles fails on no match.
		{modroot.Path(t, "internal", "pki"), "ErrNotEnrolled"},
		{modroot.Path(t, "internal", "oauth"), "ErrNoFleetRoot"},
	} {
		fset := token.NewFileSet()
		files := goFiles(t, fset, filepath.Join(pkg.dir, "*.go"))
		// consts and vars whose value holds literals, by name
		values := map[string][]ast.Expr{}
		for _, af := range files {
			ast.Inspect(af, func(n ast.Node) bool {
				if vs, ok := n.(*ast.ValueSpec); ok {
					for i, name := range vs.Names {
						if i < len(vs.Values) {
							values[name.Name] = append(values[name.Name], vs.Values[i])
						}
					}
				}
				return true
			})
		}
		checked := 0
		reached := map[string]bool{}
		var check func(e ast.Expr, depth int)
		check = func(e ast.Expr, depth int) {
			ast.Inspect(e, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && depth < 3 {
					for _, v := range values[id.Name] {
						check(v, depth+1)
					}
				}
				s, ok := literalText(t, fset, n)
				if !ok {
					return true
				}
				checked++
				if restRoute.MatchString(s) {
					t.Errorf("%s: the error text %q names a REST route; name the page's action instead", fset.Position(n.Pos()), s)
				}
				return true
			})
		}
		for _, af := range files {
			ast.Inspect(af, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.ValueSpec:
					for i, name := range x.Names {
						if i < len(x.Values) && isErrorCall(x.Values[i]) {
							reached[name.Name] = true
						}
					}
				case *ast.CallExpr:
					if isErrorCall(x) {
						for _, a := range x.Args {
							check(a, 0)
						}
					}
				}
				return true
			})
		}
		if !reached[pkg.must] {
			t.Errorf("%s: %s is not an errors.New the scan reads", pkg.dir, pkg.must)
		}
		if checked < 20 {
			t.Errorf("%s: checked %d error texts, want the package's (at least 20)", pkg.dir, checked)
		}
	}
}

// isErrorCall reports whether e is a call of errors.New or fmt.Errorf.
func isErrorCall(e ast.Expr) bool {
	c, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := c.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	p, ok := sel.X.(*ast.Ident)
	return ok && (p.Name == "errors" && sel.Sel.Name == "New" || p.Name == "fmt" && sel.Sel.Name == "Errorf")
}
