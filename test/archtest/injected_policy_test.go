package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestInjectedPolicy_AssignedOnlyFromTestFiles pins the in-process auth test
// seam (unified authn/z plan, P2): internal/edge/web declares
// `var injectedPolicy func(*http.Request) (authz.Policy, bool)`, nil in
// production, and only _test.go files may assign it. If production code could
// set it, a hook that replaces authentication would ship.
//
// Two things fail the test, in any non-_test.go file under internal/:
//   - an assignment to an identifier or selector named injectedPolicy;
//   - in internal/edge/web, a second package-level function-typed var whose
//     name contains "Policy" (a parallel seam under another name), or a
//     declaration of injectedPolicy outside testpolicy.go.
func TestInjectedPolicy_AssignedOnlyFromTestFiles(t *testing.T) {
	root := moduleRoot(t)
	internalDir := filepath.Join(root, "internal")
	fset := token.NewFileSet()

	isInjected := func(e ast.Expr) bool {
		switch x := e.(type) {
		case *ast.Ident:
			return x.Name == "injectedPolicy"
		case *ast.SelectorExpr:
			return x.Sel.Name == "injectedPolicy"
		}
		return false
	}

	var problems []string
	err := filepath.WalkDir(internalDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		at := func(p token.Pos) string { return rel + ":" + strconv.Itoa(fset.Position(p).Line) }

		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.AssignStmt:
				for _, lhs := range x.Lhs {
					if isInjected(lhs) {
						problems = append(problems, at(x.Pos())+": assigns injectedPolicy from a non-test file")
					}
				}
			case *ast.IncDecStmt:
				if isInjected(x.X) {
					problems = append(problems, at(x.Pos())+": modifies injectedPolicy from a non-test file")
				}
			}
			return true
		})

		inWeb := strings.HasPrefix(rel, "internal/edge/web/") &&
			!strings.Contains(strings.TrimPrefix(rel, "internal/edge/web/"), "/")
		if !inWeb {
			return nil
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for _, name := range vs.Names {
					if !strings.Contains(name.Name, "Policy") {
						continue
					}
					if _, isFunc := vs.Type.(*ast.FuncType); !isFunc {
						continue
					}
					if name.Name == "injectedPolicy" && rel == "internal/edge/web/testpolicy.go" {
						continue
					}
					problems = append(problems, at(name.Pos())+": package-level func var "+name.Name+" is a second policy seam; the only one is injectedPolicy in testpolicy.go")
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Error(p)
	}
}
