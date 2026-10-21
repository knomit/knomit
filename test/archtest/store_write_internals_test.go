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

// systemTreeDoor is the one fact write door that keeps the case of a nested
// path (facts.Index.WriteSystemTree, reached as svc.Facts().WriteSystemTree).
// kb/invariants/store/paths/system-tree-door/ec0eb08f.md: it exists for
// knomit's own act of creating a repo from a template, accepts only README.md
// and paths under .knomit/, and is never on the MCP or REST surface (.knomit/
// stays closed to the fact tools).
const systemTreeDoor = "WriteSystemTree"

// systemTreeDoorAllowed reports whether a NON-test file, given as a
// module-root-relative slash path, may name the door: internal/repos, which
// creates a repo from a template. Tests may name it anywhere (they set up
// fixtures), and everything under internal/store is the store's own code.
func systemTreeDoorAllowed(rel string) bool {
	return strings.HasPrefix(rel, "internal/repos/") && !strings.Contains(strings.TrimPrefix(rel, "internal/repos/"), "/")
}

// systemTreeDoorUses returns one "line N" string per selector expression
// `x.WriteSystemTree` (a call or a method value) in the source. A declaration
// (`func (fi *Index) WriteSystemTree`) and a comment are not selectors.
func systemTreeDoorUses(t *testing.T, fset *token.FileSet, name string, src any) []string {
	return selectorUses(t, fset, name, src, map[string]bool{systemTreeDoor: true})
}

// selectorUses returns one "line N: <name>" string per selector expression
// `x.<name>` for a name in names.
func selectorUses(t *testing.T, fset *token.FileSet, name string, src any, names map[string]bool) []string {
	t.Helper()
	f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	var uses []string
	ast.Inspect(f, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && names[sel.Sel.Name] {
			uses = append(uses, "line "+strconv.Itoa(fset.Position(sel.Sel.Pos()).Line)+": "+sel.Sel.Name)
		}
		return true
	})
	return uses
}

// TestStoreSystemTreeDoor_OnlyFromRepos: handing out *facts.Index (PR E3)
// makes WriteSystemTree reachable from every package that holds a Service, so
// the rule that used to be "callers of Service.WriteSystemTree" is now a rule
// about who may name the method. This parses every .go file outside
// internal/store/** and fails on a non-test file other than the top-level
// files of internal/repos that selects WriteSystemTree.
//
// Non-vacuity: the walk must visit at least minFiles files, find at least one
// allowed use (internal/repos/lifecycle.go, today), and see the door still
// declared in internal/store/facts/system_files.go, so a rename cannot leave
// the scan matching nothing.
func TestStoreSystemTreeDoor_OnlyFromRepos(t *testing.T) {
	const minFiles = 500
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	var offenders []string
	scanned, allowedUses := 0, 0
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
			if d.Name() == "node_modules" || rel == "internal/store" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") {
			return nil
		}
		scanned++
		if strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		uses := systemTreeDoorUses(t, fset, path, nil)
		for _, u := range uses {
			if systemTreeDoorAllowed(rel) {
				allowedUses++
			} else {
				offenders = append(offenders, rel+" "+u)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < minFiles {
		t.Fatalf("scanned only %d .go files, want at least %d: the walk no longer reaches the module, so this guard checks nothing", scanned, minFiles)
	}
	if allowedUses < 1 {
		t.Fatal("found no allowed use of WriteSystemTree in internal/repos: the caller moved or was renamed, so this guard's allowlist is stale")
	}
	decl := filepath.Join(root, "internal", "store", "facts", "system_files.go")
	f, err := parser.ParseFile(token.NewFileSet(), decl, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	declared := false
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv != nil && fn.Name.Name == systemTreeDoor {
			declared = true
		}
	}
	if !declared {
		t.Fatalf("%s no longer declares a %s method: the door moved, so this guard checks nothing", decl, systemTreeDoor)
	}
	if len(offenders) > 0 {
		t.Fatalf("%s keeps the case of a nested path and is never on the MCP or REST surface "+
			"(kb/invariants/store/paths/system-tree-door/ec0eb08f.md); only internal/repos may name it outside the store:\n  %s",
			systemTreeDoor, strings.Join(offenders, "\n  "))
	}
}

// TestStoreSystemTreeDoor_GuardDiscriminates runs the detector on inline
// source: a call and a method value are flagged; a declaration, a comment and
// a string are not. It also checks the allowed-path predicate.
func TestStoreSystemTreeDoor_GuardDiscriminates(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want int
	}{
		{"call", "package p\nfunc f() { svc.Facts().WriteSystemTree() }", 1},
		{"method value", "package p\nfunc f() { g := svc.Facts().WriteSystemTree; _ = g }", 1},
		{"declaration", "package p\ntype I struct{}\nfunc (fi *I) WriteSystemTree() {}", 0},
		{"comment", "package p\n// WriteSystemTree is a door\nfunc f() {}", 0},
		{"string", "package p\nvar s = \"svc.Facts().WriteSystemTree()\"", 0},
	}
	for _, c := range cases {
		got := systemTreeDoorUses(t, token.NewFileSet(), c.name+".go", c.src)
		if len(got) != c.want {
			t.Errorf("%s: %d uses, want %d (%v)", c.name, len(got), c.want, got)
		}
	}
	for rel, want := range map[string]bool{
		"internal/repos/lifecycle.go":   true,
		"internal/repos/sub/x.go":       false,
		"internal/mcp/learn.go":         false,
		"internal/web/handlers_fact.go": false,
	} {
		if systemTreeDoorAllowed(rel) != want {
			t.Errorf("systemTreeDoorAllowed(%q) = %v, want %v", rel, !want, want)
		}
	}
}

// triggerTableWrites are the facts.Repo methods that write the trigger
// dispatcher's SQLite tables. They take no branch lock and touch no git
// object, but nothing except the dispatcher has a reason to call them: a
// caller elsewhere could forge or erase fire history and move watermarks.
var triggerTableWrites = map[string]bool{
	"RecordTriggerRun": true, "RecordTriggerRuns": true,
	"RecordTriggerResults": true, "AdvanceTriggerWatermarks": true,
}

// TestFactsRepoTriggerWrites_OnlyFromDispatcher: handing out *facts.Repo (PR E3)
// makes the four trigger-table writes callable from every package that holds a
// Service. They were already reachable through Service.Triggers(), so E3 did
// not widen the method set; this guard pins the reach. It parses every .go
// file outside internal/store/** and fails on a non-test file other than
// internal/repos/triggers.go that selects one of them.
//
// Non-vacuity: at least 500 files walked; triggers.go must use
// RecordTriggerRuns, RecordTriggerResults and AdvanceTriggerWatermarks; and
// facts/triggers.go must still declare all four.
func TestFactsRepoTriggerWrites_OnlyFromDispatcher(t *testing.T) {
	const minFiles = 500
	const dispatcher = "internal/repos/triggers.go"
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	var offenders []string
	scanned := 0
	allowed := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			switch rel {
			case ".git", "web", "dist", ".claude", "third_party", "internal/store":
				return filepath.SkipDir
			}
			if d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") {
			return nil
		}
		scanned++
		if strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		for _, u := range selectorUses(t, fset, path, nil, triggerTableWrites) {
			if rel == dispatcher {
				allowed[u[strings.Index(u, ": ")+2:]] = true
			} else {
				offenders = append(offenders, rel+" "+u)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < minFiles {
		t.Fatalf("scanned only %d .go files, want at least %d: the walk no longer reaches the module", scanned, minFiles)
	}
	for _, want := range []string{"RecordTriggerRuns", "RecordTriggerResults", "AdvanceTriggerWatermarks"} {
		if !allowed[want] {
			t.Errorf("%s no longer calls %s: the dispatcher moved or was renamed, so this guard's allowlist is stale", dispatcher, want)
		}
	}
	decl := filepath.Join(root, "internal", "store", "facts", "triggers.go")
	f, err := parser.ParseFile(token.NewFileSet(), decl, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]bool{}
	for _, dd := range f.Decls {
		if fn, ok := dd.(*ast.FuncDecl); ok && fn.Recv != nil && triggerTableWrites[fn.Name.Name] {
			declared[fn.Name.Name] = true
		}
	}
	for name := range triggerTableWrites {
		if !declared[name] {
			t.Errorf("%s no longer declares %s: it moved, so this guard checks nothing", decl, name)
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("the trigger-table writes belong to the dispatcher (%s) alone:\n  %s", dispatcher, strings.Join(offenders, "\n  "))
	}
}

// TestFactsRepoTriggerWrites_GuardDiscriminates: calls and method values of any
// of the four names are flagged; a declaration, a comment and a read such as
// RecentTriggerFires are not.
func TestFactsRepoTriggerWrites_GuardDiscriminates(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want int
	}{
		{"call", "package p\nfunc f() { svc.FactsRepo().AdvanceTriggerWatermarks() }", 1},
		{"method value", "package p\nfunc f() { g := tr.RecordTriggerRun; _ = g }", 1},
		{"two", "package p\nfunc f() { tr.RecordTriggerRuns(); tr.RecordTriggerResults() }", 2},
		{"read", "package p\nfunc f() { tr.RecentTriggerFires() }", 0},
		{"declaration", "package p\ntype R struct{}\nfunc (r *R) RecordTriggerRun() {}", 0},
		{"comment", "package p\n// AdvanceTriggerWatermarks is tx2\nfunc f() {}", 0},
	}
	for _, c := range cases {
		got := selectorUses(t, token.NewFileSet(), c.name+".go", c.src, triggerTableWrites)
		if len(got) != c.want {
			t.Errorf("%s: %d uses, want %d (%v)", c.name, len(got), c.want, got)
		}
	}
}
