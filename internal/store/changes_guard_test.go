package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestChanges_NoWritePathNoClock is the structural half of F05's contract: the
// changes read — store, MCP tool and REST route — is a read of two trees. It
// must never reach a store WRITE (a new write path is exactly what F05 was
// approved without), and never read a CLOCK or the commit log (commit time is
// set by whoever makes the commit, and ordering by it is how a deletion gets
// lost). Behavioural tests prove the answers; this proves no future edit
// quietly adds either dependency to these three files.
//
// It inspects selector expressions in the AST — code, not comments — so the
// files' prose about timestamps does not trip it.
func TestChanges_NoWritePathNoClock(t *testing.T) {
	forbidden := map[string]string{
		// store writers and ref movers
		"WriteFact": "write", "WriteRootFile": "write", "BatchWriteFacts": "write", "DeleteFact": "write",
		"writeFile": "write", "writeFileExact": "write", "deleteFile": "write", "batchWrite": "write",
		"SetReference": "write", "SetEncodedObject": "write", "notifyCommit": "write",
		"CommitLogSync": "write", "SetPipelineWatermark": "write", "EnsureBranch": "write",
		// clocks and commit time
		"Now": "clock", "When": "clock",
		// the commit log (time-ordered, instance-local tiebreak)
		"CommitLogQuery": "commit log", "commitLogQuery": "commit log", "LogPaginated": "commit log",
	}
	files := []string{"changes.go", "../mcp/changes.go", "../web/handlers_changes.go"}
	fset := token.NewFileSet()
	for _, f := range files {
		file, err := parser.ParseFile(fset, f, nil, 0)
		require.NoError(t, err, f)
		checked := 0
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			checked++
			if why, bad := forbidden[sel.Sel.Name]; bad {
				t.Errorf("%s: %s reaches %s (%s) — the changes read must be two trees, no write, no clock",
					fset.Position(sel.Pos()), exprString(sel), sel.Sel.Name, why)
			}
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == "time" && sel.Sel.Name != "Duration" && sel.Sel.Name != "Second" {
				t.Errorf("%s: time.%s — no clock in the changes read (only the request timeout's time.Second)",
					fset.Position(sel.Pos()), sel.Sel.Name)
			}
			return true
		})
		require.Greater(t, checked, 20, "%s: the guard must actually have walked the file", f)
	}
}

func exprString(sel *ast.SelectorExpr) string {
	var b strings.Builder
	if x, ok := sel.X.(*ast.Ident); ok {
		b.WriteString(x.Name)
		b.WriteString(".")
	}
	b.WriteString(sel.Sel.Name)
	return b.String()
}
