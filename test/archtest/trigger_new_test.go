package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// trigger.New builds a repo's trigger dispatcher and returns its run loop as
// a capability (14c review B1). Whoever calls New holds a dispatcher bound to
// a store lease, and its run loop writes the trigger tables and fires
// emits, scripts and recipes; a second dispatcher (or a second loop) on one
// repo would fire the same watermark→head range twice. The repos parent is
// the one owner: Manager.newInstance builds the dispatcher and the Serve
// stage runs it, so only a top-level non-test file of internal/corpus/repos
// may select trigger.New. Everyone else gets RepoInstance.Triggers(), which
// can kick the dispatcher and read its report and cannot run it.
//
// Test files are not scanned: the parent's tests build a second dispatcher
// on purpose (TestDispatch_PhaseBHoldsNoLease counts its leases).
//
// Non-vacuity: at least 500 files walked, and internal/corpus/repos/manager.go
// still selects trigger.New (a rename or move of the constructor fails here
// instead of emptying the scan).
const triggerPkgPath = "knomit/internal/corpus/repos/trigger"

// triggerNewUses returns "line N" for every selection of New on an import of
// the trigger package (any local name), and a dot import of it.
func triggerNewUses(t *testing.T, fset *token.FileSet, name string, src any) []string {
	t.Helper()
	f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	locals := map[string]bool{}
	var out []string
	for _, im := range f.Imports {
		p, _ := strconv.Unquote(im.Path.Value)
		if p != triggerPkgPath {
			continue
		}
		local := "trigger"
		if im.Name != nil {
			local = im.Name.Name
		}
		if local == "." {
			out = append(out, "line "+strconv.Itoa(fset.Position(im.Pos()).Line)+": dot import of trigger")
			continue
		}
		locals[local] = true
	}
	if len(locals) == 0 {
		return out
	}
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "New" {
			return true
		}
		if x, ok := sel.X.(*ast.Ident); ok && locals[x.Name] {
			out = append(out, "line "+strconv.Itoa(fset.Position(sel.Pos()).Line)+": trigger.New")
		}
		return true
	})
	return out
}

func TestTriggerNew_OnlyFromRepos(t *testing.T) {
	const minFiles = 500
	const owner = "internal/corpus/repos/manager.go"
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	var offenders []string
	scanned, ownerUses := 0, 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			switch rel {
			case ".git", "web", "dist", ".claude", "third_party":
				return filepath.SkipDir
			}
			if d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		scanned++
		uses := triggerNewUses(t, fset, path, nil)
		if rel == owner {
			ownerUses += len(uses)
		}
		if filepath.ToSlash(filepath.Dir(rel)) == "internal/corpus/repos" {
			return nil
		}
		for _, u := range uses {
			offenders = append(offenders, rel+" "+u)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < minFiles {
		t.Fatalf("scanned only %d non-test .go files, want at least %d: the walk no longer reaches the module", scanned, minFiles)
	}
	if ownerUses == 0 {
		t.Fatalf("%s no longer selects trigger.New: the constructor moved or was renamed, so this guard checks nothing", owner)
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("only the repos parent (a top-level file of internal/corpus/repos) may build a trigger dispatcher; use RepoInstance.Triggers():\n  %s", strings.Join(offenders, "\n  "))
	}
}

func TestTriggerNew_GuardDiscriminates(t *testing.T) {
	const imp = "package p\nimport \"knomit/internal/corpus/repos/trigger\"\n"
	flagged := map[string]string{
		"call":           imp + "func f() { _, _ = trigger.New(\"\", \"\", \"\", x, 0, 0, nil, \"\", nil, nil, nil, nil, nil, nil) }\n",
		"function value": imp + "var g = trigger.New\n",
		"aliased import": "package p\nimport tr \"knomit/internal/corpus/repos/trigger\"\nvar g = tr.New\n",
		"dot import":     "package p\nimport . \"knomit/internal/corpus/repos/trigger\"\n",
	}
	for name, src := range flagged {
		if len(triggerNewUses(t, token.NewFileSet(), name+".go", src)) == 0 {
			t.Errorf("%s: not flagged", name)
		}
	}
	clean := map[string]string{
		"other package's New": "package p\nimport \"knomit/internal/corpus/repos\"\nvar g = repos.New\n",
		"handle use":          imp + "func f(d *trigger.Dispatcher) { d.Kick() }\n",
		"comment and string":  imp + "// trigger.New\nvar s = \"trigger.New\"\nvar _ trigger.Report\n",
		"local named trigger": "package p\nfunc f(trigger T) { trigger.New() }\n",
	}
	for name, src := range clean {
		if got := triggerNewUses(t, token.NewFileSet(), name+".go", src); len(got) != 0 {
			t.Errorf("%s: wrongly flagged: %v", name, got)
		}
	}
}
