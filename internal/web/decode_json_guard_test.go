package web

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// bodyReadAllowed lists the only places under internal/web that may read a
// request body without decodeJSON, each with the reason. A new entry needs a
// reason as good as these; "it was quicker" is how the Content-Type rule
// quietly stops covering a handler.
var bodyReadAllowed = map[string]string{
	"handlers_ontologies.go": "ontologies:validate reads YAML, not JSON",
	"oauth_pending_api.go":   "OAuth approve: browserProof already requires application/json from a browser, and it shares isJSONMediaType",
}

// TestNoHandlerDecodesARequestBodyOutsideDecodeJSON keeps every JSON request
// body behind decodeJSON's Content-Type rule. It flags, in any non-test file
// under internal/web, a json.NewDecoder or io.ReadAll call whose arguments
// reach a `.Body` field (r.Body, req.Body, through http.MaxBytesReader or
// not). A body passed through a variable first is not seen; this is a guard
// against the ordinary shape, not a proof.
func TestNoHandlerDecodesARequestBodyOutsideDecodeJSON(t *testing.T) {
	found := map[string][]string{} // file -> offending calls
	fset := token.NewFileSet()
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			fn := selectorName(call.Fun)
			if fn != "json.NewDecoder" && fn != "io.ReadAll" {
				return true
			}
			for _, arg := range call.Args {
				if reachesBody(arg) {
					found[filepath.ToSlash(path)] = append(found[filepath.ToSlash(path)],
						fmt.Sprintf("%s at %s", fn, fset.Position(call.Pos())))
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	var bad []string
	for file, calls := range found {
		if _, ok := bodyReadAllowed[file]; !ok {
			bad = append(bad, calls...)
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Fatalf("a handler reads a request body directly:\n  %s\n"+
			"read a JSON body with decodeJSON (or decodeOptionalJSON when it may be absent), "+
			"which refuses a non-JSON Content-Type with 415; if this is not a JSON request body "+
			"(a response body, YAML), add the file to bodyReadAllowed with the reason",
			strings.Join(bad, "\n  "))
	}
	// An allowance whose read has gone is a hole waiting for the next one.
	for file, reason := range bodyReadAllowed {
		if len(found[file]) == 0 {
			t.Errorf("bodyReadAllowed[%q] (%s) no longer reads a body; remove the entry", file, reason)
		}
	}
}

func selectorName(e ast.Expr) string {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return ""
	}
	return pkg.Name + "." + sel.Sel.Name
}

// reachesBody reports whether e mentions a `.Body` selector anywhere in it.
func reachesBody(e ast.Expr) bool {
	hit := false
	ast.Inspect(e, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "Body" {
			hit = true
		}
		return !hit
	})
	return hit
}
