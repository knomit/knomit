package archtest

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The old name, built so this file does not contain it.
var retiredServerVar = "KNOMIT_" + "BASE_URL"

// KNOMIT_SERVER replaced the old variable everywhere, with no alias (user
// decision 2026-10-02: "No compatibility, nothing shipped yet. Rip and
// replace."). A leftover read would be a second, silent way to name the
// server — exactly the ambiguity the rename removes — and a leftover comment
// or doc would tell the next reader to set a variable nothing reads.
//
// Walks the module's tracked-content directories; .claude/ (plans and
// history), .git, node_modules and build output are skipped.
func TestNoRetiredServerVariable(t *testing.T) {
	root := moduleRoot(t)
	skip := map[string]bool{".git": true, ".claude": true, "node_modules": true, "dist": true, "build": true}
	var hits []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != root && skip[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > 4<<20 {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		if strings.Contains(string(b), retiredServerVar) {
			rel, _ := filepath.Rel(root, p)
			hits = append(hits, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) > 0 {
		t.Fatalf("%s is gone (use KNOMIT_SERVER), yet these files still name it:\n  %s",
			retiredServerVar, strings.Join(hits, "\n  "))
	}
}

// The ONE server entry point, `knomit serve`, hands the repos Manager its own
// address once its listeners are bound, or a recipe's `exec` child gets no
// KNOMIT_SERVER and its `kb` falls back to the lockfile — the bug
// KNOMIT_SERVER exists to remove. The desktop runs `knomit serve --supervised`
// as a child instead of booting a server of its own, so cmd/serve.go is the
// only production file that may reference Manager.SetServerAddress: a second
// one would be a second server construction path coming back. Which address
// is chosen is pinned by the tests beside internal/app/listeners.go.
//
// References are found with go/ast (a selector named SetServerAddress, called
// or taken as a method value), so a comment naming the method does not count,
// and neither does the method's own declaration in internal/corpus/repos (a
// declaration is not a selector). No directory is excluded: a call from inside
// repos would be a second path too.
func TestOnlyServeRecordsItsAddress(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()
	var got []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".claude", "node_modules", "dist", "build", "third_party":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("parse %s: %w", rel, err)
		}
		found := false
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "SetServerAddress" {
				found = true
			}
			return !found
		})
		if found {
			got = append(got, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Positive control: the one expected reference is found by this walk, so
	// an empty result cannot pass by the walker seeing nothing.
	if len(got) != 1 || got[0] != "cmd/serve.go" {
		t.Fatalf("Manager.SetServerAddress is referenced from %v; want exactly [cmd/serve.go], the one server entry point", got)
	}
}
