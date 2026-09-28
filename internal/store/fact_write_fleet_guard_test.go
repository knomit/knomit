package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestFactWrite_CallsNothingFleet (F10): the knowledge-base write path never
// reaches fleet code. The fleet record reconcile lives in internal/repos,
// which store cannot import (it would be a cycle); this pins the other half:
// nothing fact_write.go calls is fleet-anything.
func TestFactWrite_CallsNothingFleet(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "fact_write.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	ast.Inspect(f, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		calls++
		var name string
		switch fn := c.Fun.(type) {
		case *ast.Ident:
			name = fn.Name
		case *ast.SelectorExpr:
			name = fn.Sel.Name
		}
		if strings.Contains(strings.ToLower(name), "fleet") || strings.Contains(strings.ToLower(name), "member") {
			t.Errorf("fact_write.go calls %s: the KB write path must not reach fleet code", name)
		}
		return true
	})
	if calls < 20 {
		t.Fatalf("parsed only %d calls from fact_write.go: the guard is not looking at the write path", calls)
	}
}
