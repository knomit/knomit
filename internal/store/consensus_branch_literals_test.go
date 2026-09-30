package store

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

// TestNoHardcodedBranchLiterals holds the user's ruling, "Hardcoded values are
// never good", for consensus-branch names in shipped Go code: across cmd/,
// internal/ and the dev tools that read a knowledge base (tools/okf,
// tools/calibrate), no non-test file may carry a string literal naming a
// branch by convention ("main", "master", or a ref/remote path ending in one).
// The single exception is the definition of DefaultConsensusBranch, the one
// named default a brand-new repo gets.
//
// It parses the files (go/ast), so comments and prose do not count, only
// values the program can act on. Restoring any of the removed defaults
// (`upstream := "main"`, `org.Branch = "main"`, `--upstream origin/main`, a
// "prefer main" loop, ...) turns this red at the line that did it.
func TestNoHardcodedBranchLiterals(t *testing.T) {
	root := filepath.Join("..", "..")
	dirs := []string{"cmd", "internal", filepath.Join("tools", "okf"), filepath.Join("tools", "calibrate")}
	named := func(v string) bool {
		for _, n := range []string{"main", "master"} {
			if v == n || strings.HasSuffix(v, "/"+n) {
				return true
			}
		}
		return false
	}
	var defs, hits []string
	for _, d := range dirs {
		err := filepath.WalkDir(filepath.Join(root, d), func(path string, e fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if e.IsDir() {
				// pkitest is a test-helper package (its "master" is the fleet's
				// master-key directory, not a branch).
				if n := e.Name(); n == "testdata" || n == "node_modules" || n == "static" || n == "pkitest" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if perr != nil {
				t.Fatalf("parse %s: %v", path, perr)
			}
			allowed := map[*ast.BasicLit]bool{}
			ast.Inspect(f, func(n ast.Node) bool {
				vs, ok := n.(*ast.ValueSpec)
				if !ok {
					return true
				}
				for i, id := range vs.Names {
					if id.Name == "DefaultConsensusBranch" && i < len(vs.Values) {
						if bl, ok := vs.Values[i].(*ast.BasicLit); ok {
							allowed[bl] = true
							defs = append(defs, fset.Position(bl.Pos()).String())
						}
					}
				}
				return true
			})
			ast.Inspect(f, func(n ast.Node) bool {
				bl, ok := n.(*ast.BasicLit)
				if !ok || bl.Kind != token.STRING || allowed[bl] {
					return true
				}
				v, uerr := strconv.Unquote(bl.Value)
				if uerr == nil && named(v) {
					hits = append(hits, fset.Position(bl.Pos()).String()+": "+bl.Value)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(defs) != 1 {
		t.Errorf("DefaultConsensusBranch must be defined exactly once, found %d: %v", len(defs), defs)
	}
	for _, h := range hits {
		t.Errorf("hardcoded branch name (use the recorded/configured branch, or DefaultConsensusBranch at birth): %s", h)
	}
}
