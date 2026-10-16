package repos

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// restRoute is a REST route as a refusal text might name one.
var restRoute = regexp.MustCompile(`\b(GET|POST|PUT|PATCH|DELETE) /|/api/v1/`)

// The fleet's refusal and error texts are shown verbatim on the Fleet page,
// so they name the page's action (e.g. "Fleet → Settings → Leave"), never a
// REST route. Every string literal passed to fleetErr, errors.New or
// fmt.Errorf, or set as a FleetError's Msg, in the fleet files of this
// package is checked; the test counts them so a scan that reads nothing
// cannot pass.
// SABOTAGE: put "(POST /api/v1/fleet/leave)" back into refuseFleetPresent ->
// red.
func TestFleetTexts_NameNoRESTRoutes(t *testing.T) {
	files, err := filepath.Glob("fleet*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	check := func(e ast.Expr) {
		ast.Inspect(e, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatalf("%s: %v", fset.Position(lit.Pos()), err)
			}
			checked++
			if restRoute.MatchString(s) {
				t.Errorf("%s: the text %q names a REST route; name the Fleet page's action instead", fset.Position(lit.Pos()), s)
			}
			return true
		})
	}
	for _, f := range files {
		if matched, _ := filepath.Match("*_test.go", f); matched {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(af, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				switch fn := x.Fun.(type) {
				case *ast.Ident:
					if fn.Name == "fleetErr" {
						for _, a := range x.Args {
							check(a)
						}
					}
				case *ast.SelectorExpr:
					if p, ok := fn.X.(*ast.Ident); ok && (p.Name == "errors" && fn.Sel.Name == "New" || p.Name == "fmt" && fn.Sel.Name == "Errorf") {
						for _, a := range x.Args {
							check(a)
						}
					}
				}
			case *ast.CompositeLit:
				if id, ok := x.Type.(*ast.Ident); ok && id.Name == "FleetError" {
					for _, el := range x.Elts {
						if kv, ok := el.(*ast.KeyValueExpr); ok {
							if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Msg" {
								check(kv.Value)
							}
						}
					}
				}
			}
			return true
		})
	}
	if checked < 50 {
		t.Fatalf("checked %d texts in %d files, want the fleet files' refusals (at least 50)", checked, len(files))
	}
}
