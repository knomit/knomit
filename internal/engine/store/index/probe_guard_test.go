package index

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestEveryQueryServiceReadProbes pins the contract of Overrides.Reads: every
// exported method of FactQuery, GraphStore, HistoryQuery and
// MethodologyMatcher begins with `<recv>.probe("<its own name>")`, either as a
// statement or as the init of an `if err := ...; err != nil` that returns the
// injected error. A read that forgot its probe would be invisible to the strict
// read policy the synthesize tests rely on (a code path could read it freely),
// and a misspelt name would be allowed or rejected under the wrong name.
func TestEveryQueryServiceReadProbes(t *testing.T) {
	services := map[string]bool{"FactQuery": true, "GraphStore": true, "HistoryQuery": true, "MethodologyMatcher": true}
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	require.NoError(t, err)

	seen := 0
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 || !fn.Name.IsExported() ||
					strings.HasSuffix(fn.Name.Name, "ForTest") {
					continue
				}
				star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
				if !ok {
					continue
				}
				typ, ok := star.X.(*ast.Ident)
				if !ok || !services[typ.Name] {
					continue
				}
				seen++
				recv := fn.Recv.List[0].Names[0].Name
				where := fset.Position(fn.Pos()).String()
				require.NotEmpty(t, fn.Body.List, "%s: %s.%s has an empty body", where, typ.Name, fn.Name.Name)
				name, got := probeName(fn.Body.List[0], recv)
				require.True(t, got, "%s: %s.%s must begin with %s.probe(%q)", where, typ.Name, fn.Name.Name, recv, fn.Name.Name)
				require.Equal(t, fn.Name.Name, name, "%s: %s.%s probes under another name", where, typ.Name, fn.Name.Name)
			}
		}
	}
	// Not vacuous: 16 FactQuery (VersionMetaAt and VersionMetasAt, the
	// factset walk; VersionsByBlob, BlobsByHash, LiveVersions: the streaming prototype) + 12 GraphStore (OutgoingAtCommits; NodeIDsByBlob, DerivedFromOfNodes) + 7 HistoryQuery + 1 MethodologyMatcher
	// public reads. Adding a read means adding its probe and raising this
	// number on purpose.
	require.Equal(t, 36, seen, "the number of public reads on the four query services")
}

// probeName returns the string literal passed to <recv>.probe in the statement,
// whether it is a bare call or the init of an if.
func probeName(stmt ast.Stmt, recv string) (string, bool) {
	var call *ast.CallExpr
	switch s := stmt.(type) {
	case *ast.ExprStmt:
		call, _ = s.X.(*ast.CallExpr)
	case *ast.IfStmt:
		if as, ok := s.Init.(*ast.AssignStmt); ok && len(as.Rhs) == 1 {
			call, _ = as.Rhs[0].(*ast.CallExpr)
		}
	}
	if call == nil || len(call.Args) != 1 {
		return "", false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "probe" {
		return "", false
	}
	if id, ok := sel.X.(*ast.Ident); !ok || id.Name != recv {
		return "", false
	}
	lit, ok := call.Args[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	name, err := strconv.Unquote(lit.Value)
	return name, err == nil
}
