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

// mergeCores are the exported methods of merge.Repo that assume the caller
// already holds the destination branch's lock (MarkExperimentsRewound,
// FactMergeResolutions and MergeTreesWithStrategy additionally run before or
// inside the move of a ref, outside the NotifyCommit ordering). They stay
// exported only because the remotesync child calls them, and Service.Merge
// hands the *merge.Repo out. MergeBranch (which takes the lock), the
// experiment lifecycle and PushedBranches are the handle's real surface.
var mergeCores = map[string]bool{
	"MergeIntoBranchLocked": true, "MergeIntoBranchLockedOpts": true,
	"MergeTreesWithStrategy": true, "MarkExperimentsRewound": true,
	"FactMergeResolutions": true,
}

// TestStoreMergeCores_OnlyInsideStore (guard C proper, PR E4): handing out
// *merge.Repo makes the five lock-assuming merge cores callable from every
// package that holds a Service. This parses every .go file outside
// internal/store/** (tests included) and fails on any selector of one of them;
// the host's merges from outside the store go through Service.MergePushed and
// Service.MergeConsensus, which take the lock.
//
// Non-vacuity: at least 500 files outside the store are walked; every one of
// the five names is still declared on a method in internal/store/merge and
// still selected by a non-test file elsewhere inside internal/store (the
// parent's service_merge.go and the remotesync child), so a rename or a move
// cannot leave the scan matching nothing.
func TestStoreMergeCores_OnlyInsideStore(t *testing.T) {
	const minFiles = 500
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	var offenders []string
	scanned := 0
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
		for _, u := range selectorUses(t, fset, path, nil, mergeCores) {
			offenders = append(offenders, rel+" "+u)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < minFiles {
		t.Fatalf("scanned only %d .go files outside internal/store, want at least %d: the walk no longer reaches the module, so this guard checks nothing", scanned, minFiles)
	}

	// The expected uses INSIDE the store: every core is still declared in the
	// merge child and still called from a non-test file elsewhere in the store.
	declared := map[string]bool{}
	inside := map[string]bool{}
	storeRoot := filepath.Join(root, "internal", "store")
	err = filepath.WalkDir(storeRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		inMerge := filepath.ToSlash(filepath.Dir(path)) == filepath.ToSlash(filepath.Join(storeRoot, "merge"))
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.FuncDecl:
				if inMerge && x.Recv != nil && mergeCores[x.Name.Name] {
					declared[x.Name.Name] = true
				}
			case *ast.SelectorExpr:
				if !inMerge && mergeCores[x.Sel.Name] {
					inside[x.Sel.Name] = true
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for name := range mergeCores {
		if !declared[name] {
			t.Errorf("internal/store/merge no longer declares a method %s: it was renamed or moved, so this guard checks nothing", name)
		}
		if !inside[name] {
			t.Errorf("no non-test file of internal/store outside the merge package selects %s: the caller moved or was renamed, so this guard's expectations are stale", name)
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("the merge cores assume the caller holds the destination branch's lock (kb/architecture/store/merge-child/4102d898.md); "+
			"only internal/store/** may name them, and a merge from outside goes through Service.MergePushed, Service.MergeConsensus or merge.Repo.MergeBranch:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// TestStoreMergeCores_GuardDiscriminates runs the detector on inline source:
// calls and method values of each core are flagged, whatever the receiver
// expression; MergeBranch (the locking merge), a declaration, a comment and a
// string are not.
func TestStoreMergeCores_GuardDiscriminates(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want int
	}{
		{"call through the handle", "package p\nfunc f() { svc.Merge().MergeIntoBranchLocked() }", 1},
		{"opts call", "package p\nfunc f() { m.MergeIntoBranchLockedOpts() }", 1},
		{"trees", "package p\nfunc f() { m.MergeTreesWithStrategy() }", 1},
		{"rewound", "package p\nfunc f() { m.MarkExperimentsRewound() }", 1},
		{"resolutions", "package p\nfunc f() { m.FactMergeResolutions() }", 1},
		{"method value", "package p\nfunc f() { g := m.MergeIntoBranchLocked; _ = g }", 1},
		{"locking merge", "package p\nfunc f() { svc.Merge().MergeBranch() }", 0},
		{"lifecycle", "package p\nfunc f() { svc.Merge().CommitExperiment() }", 0},
		{"declaration", "package p\ntype R struct{}\nfunc (r *R) MergeIntoBranchLocked() {}", 0},
		{"comment", "package p\n// MarkExperimentsRewound runs before the rewind\nfunc f() {}", 0},
		{"string", "package p\nvar s = \"m.FactMergeResolutions()\"", 0},
	}
	for _, c := range cases {
		got := selectorUses(t, token.NewFileSet(), c.name+".go", c.src, mergeCores)
		if len(got) != c.want {
			t.Errorf("%s: %d uses, want %d (%v)", c.name, len(got), c.want, got)
		}
	}
}

var consensusRecordDoor = map[string]bool{"RecordConsensusBranch": true}

// TestStoreConsensusRecord_OnlyInsideStore (PR E5): Service.Remote() hands out
// *remotesync.Index, and RecordConsensusBranch is exported on it only because
// package store's InitRepoWithUpstream calls it. It writes the repo's recorded
// consensus branch, which UpstreamBranch answers (and with it the fleet
// admission trust root), so a call from any other package would redirect it.
// This parses every .go file outside internal/store/** (tests included) and
// fails on any selector of the name; the record is written by SetOrigin and the
// bootstraps inside the child.
//
// Non-vacuity: at least 500 files are walked outside the store; the method is
// still declared in internal/store/remotesync and still selected by
// internal/store/repo.go.
func TestStoreConsensusRecord_OnlyInsideStore(t *testing.T) {
	const minFiles = 500
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	var offenders []string
	scanned := 0
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
		for _, u := range selectorUses(t, fset, path, nil, consensusRecordDoor) {
			offenders = append(offenders, rel+" "+u)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < minFiles {
		t.Fatalf("scanned only %d .go files outside internal/store, want at least %d: the walk no longer reaches the module, so this guard checks nothing", scanned, minFiles)
	}

	storeRoot := filepath.Join(root, "internal", "store")
	declared, calledFromRepo := false, false
	err = filepath.WalkDir(storeRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		dir := filepath.ToSlash(filepath.Dir(path))
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.FuncDecl:
				if dir == filepath.ToSlash(filepath.Join(storeRoot, "remotesync")) && x.Recv != nil && x.Name.Name == "RecordConsensusBranch" {
					declared = true
				}
			case *ast.SelectorExpr:
				if filepath.Base(path) == "repo.go" && dir == filepath.ToSlash(storeRoot) && x.Sel.Name == "RecordConsensusBranch" {
					calledFromRepo = true
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !declared {
		t.Error("internal/store/remotesync no longer declares a method RecordConsensusBranch: it was renamed or moved, so this guard checks nothing")
	}
	if !calledFromRepo {
		t.Error("internal/store/repo.go no longer selects RecordConsensusBranch: the caller moved or was renamed, so this guard's expectations are stale")
	}
	if len(offenders) > 0 {
		t.Fatalf("RecordConsensusBranch rewrites the recorded consensus branch (kb/invariants/store/consensus-branch-recorded/bcde3d66.md), "+
			"which UpstreamBranch and the fleet admission trust root follow; only internal/store/** may call it:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// TestStoreConsensusRecord_GuardDiscriminates: a call or method value is
// flagged whatever the receiver; the reader RecordedConsensusBranch, a
// declaration, a comment and a string are not.
func TestStoreConsensusRecord_GuardDiscriminates(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want int
	}{
		{"call through the handle", "package p\nfunc f() { svc.Remote().RecordConsensusBranch(\"x\") }", 1},
		{"bound handle", "package p\nfunc f() { r.RecordConsensusBranch(\"x\") }", 1},
		{"method value", "package p\nfunc f() { g := r.RecordConsensusBranch; _ = g }", 1},
		{"reader", "package p\nfunc f() { r.RecordedConsensusBranch() }", 0},
		{"declaration", "package p\ntype R struct{}\nfunc (r *R) RecordConsensusBranch(string) {}", 0},
		{"comment", "package p\n// RecordConsensusBranch persists\nfunc f() {}", 0},
		{"string", "package p\nvar s = \"r.RecordConsensusBranch()\"", 0},
	}
	for _, c := range cases {
		got := selectorUses(t, token.NewFileSet(), c.name+".go", c.src, consensusRecordDoor)
		if len(got) != c.want {
			t.Errorf("%s: %d uses, want %d (%v)", c.name, len(got), c.want, got)
		}
	}
}

// sessionsPlumbing is the part of internal/store/sessions that takes or opens
// raw database handles. The package is importable (PipelineIndex and ToolIndex
// are handed out by Service.Pipeline and Service.ToolSession), but a caller
// that could open a session database or build an index over a *sql.DB of its
// own would run session SQL outside the Service's lifetime (the Service closes
// and deletes the session files on Close, and reaps idle rows on its own
// schedule). Only package store wires these.
var sessionsPlumbing = map[string]bool{
	"Open": true, "NewPipelineIndex": true, "NewToolIndex": true, "ReapIdle": true,
}

const sessionsImport = "knomit/internal/store/sessions"

// sessionsPlumbingUses returns one "line N: sessions.<name>" per selector
// expression `<local>.<name>` where <local> is the local name this file
// imports internal/store/sessions under and <name> is plumbing. A file that
// does not import the package yields nothing, so store.Open, os.Open and a
// method called Open are never flagged. A dot-import of the package is itself
// reported, because it hides the selector.
func sessionsPlumbingUses(t *testing.T, fset *token.FileSet, name string, src any) []string {
	t.Helper()
	f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	local := ""
	var uses []string
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		if p != sessionsImport {
			continue
		}
		local = "sessions"
		if imp.Name != nil {
			local = imp.Name.Name
		}
		// A dot-import would let a bare NewToolIndex(...) or Open(...) slip
		// past the selector match, so the import itself is the offence.
		if local == "." {
			uses = append(uses, "line "+strconv.Itoa(fset.Position(imp.Pos()).Line)+": sessions dot-import")
		}
	}
	if local == "" {
		return nil
	}
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || !sessionsPlumbing[sel.Sel.Name] {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == local {
			uses = append(uses, "line "+strconv.Itoa(fset.Position(sel.Sel.Pos()).Line)+": sessions."+sel.Sel.Name)
		}
		return true
	})
	return uses
}

// TestStoreSessionsPlumbing_OnlyInsideStore: sessions.Open, NewPipelineIndex,
// NewToolIndex and ReapIdle are exported only because package store (the
// parent of the sessions child) must call them. This parses every .go file in
// the module, tests included, and fails on a use from anywhere but the
// top-level files of internal/store and the sessions package itself.
//
// Non-vacuity: the walk must visit at least minFiles files, each of the four
// must still be declared in internal/store/sessions, and internal/store/
// service.go must still use each, so a rename cannot leave the scan matching
// nothing.
func TestStoreSessionsPlumbing_OnlyInsideStore(t *testing.T) {
	const minFiles = 500
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	var offenders []string
	scanned := 0
	declared := map[string]bool{}
	usedByService := map[string]bool{}
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
		if !strings.HasSuffix(rel, ".go") {
			return nil
		}
		scanned++
		dir := filepath.ToSlash(filepath.Dir(rel))
		inSessions := dir == "internal/store/sessions"
		if inSessions && !strings.HasSuffix(rel, "_test.go") {
			f, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
			if perr != nil {
				return perr
			}
			for _, decl := range f.Decls {
				if fd, ok := decl.(*ast.FuncDecl); ok && fd.Recv == nil && sessionsPlumbing[fd.Name.Name] {
					declared[fd.Name.Name] = true
				}
			}
		}
		for _, u := range sessionsPlumbingUses(t, fset, path, nil) {
			if rel == "internal/store/service.go" {
				usedByService[u[strings.Index(u, "sessions.")+len("sessions."):]] = true
			}
			if dir != "internal/store" && !inSessions {
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
	for name := range sessionsPlumbing {
		if !declared[name] {
			t.Errorf("internal/store/sessions no longer declares a function %s: it was renamed or moved, so this guard checks nothing", name)
		}
		if !usedByService[name] {
			t.Errorf("internal/store/service.go no longer uses sessions.%s: the caller moved or was renamed, so this guard's expectations are stale", name)
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("sessions.Open, NewPipelineIndex, NewToolIndex and ReapIdle take raw database handles and run on the Service's schedule; only package store may call them:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// TestStoreSessionsPlumbing_GuardDiscriminates: a use through the import's
// local name is flagged; the same names on another package, a method, a
// declaration, a comment and a string are not, and a file that does not import
// the package yields nothing even when it says sessions.Open.
func TestStoreSessionsPlumbing_GuardDiscriminates(t *testing.T) {
	imp := "package p\nimport \"" + sessionsImport + "\"\n"
	cases := []struct {
		name string
		src  string
		want int
	}{
		{"open", imp + "func f() { sessions.Open(\"x\") }", 1},
		{"constructor", imp + "func f() { _ = sessions.NewPipelineIndex(a, b) }", 1},
		{"method value", imp + "func f() { g := sessions.ReapIdle; _ = g }", 1},
		{"aliased import", "package p\nimport ss \"" + sessionsImport + "\"\nfunc f() { ss.NewToolIndex(db) }", 1},
		{"dot import", "package p\nimport . \"" + sessionsImport + "\"\nfunc f() { _ = NewToolIndex(db) }", 1},
		{"handle method", imp + "func f(pi *sessions.PipelineIndex) { pi.Open() }", 0},
		{"other package", imp + "func f() { os.Open(\"x\") }", 0},
		{"not imported", "package p\nfunc f() { sessions.Open(\"x\") }", 0},
		{"comment", imp + "// sessions.Open opens\nfunc f() {}", 0},
		{"string", imp + "var s = \"sessions.Open\"", 0},
	}
	for _, c := range cases {
		got := sessionsPlumbingUses(t, token.NewFileSet(), c.name+".go", c.src)
		if len(got) != c.want {
			t.Errorf("%s: %d uses, want %d (%v)", c.name, len(got), c.want, got)
		}
	}
}

// indexInternals are the *index.Index methods that Service.IndexManager()
// exposes beyond its lifecycle surface (Sync, SyncLocked, Rebuild,
// SyncWatermark, NeedsRebuild). Each is exported only because package store or
// its fsck child calls it: SetSyncSuspended turns every Sync into a silent
// no-op (Replay does it around a bulk replay into a transient store), GC
// deletes index rows for facts the index believes are orphaned,
// EnsureFactsVecDefault creates the vector table before an embedder exists
// (store.Open calls it once), and the Persisted* readers feed the integrity
// report.
var indexInternals = map[string]bool{
	"SetSyncSuspended": true, "GC": true, "EnsureFactsVecDefault": true,
	"PersistedEmbedModelID": true, "PersistedEmbedDim": true,
}

const indexImport = "knomit/internal/store/index"

// indexScope is what the lock-free Sync half knows about ONE Go package (all
// of its files, test files included) plus what is exported module-wide: the
// names that hold or return an *index.Index. It is name-based, not
// type-checked (go/types would need every dependency of the module resolved,
// and x/tools is not a dependency), and deliberately over-approximate within a
// package: a field named im that is an *index.Index in one file makes every
// `.im.Sync` of the package a hit, which is the safe direction for a guard.
type indexScope struct {
	// bound: identifier or field names that hold an *index.Index in this
	// package (parameters, fields, vars, results of assignments).
	bound map[string]bool
	// producers: function and method names of this package whose result list
	// holds an *index.Index, with the result positions.
	producers map[string][]int
}

// indexFileLocal is the name file f gives the index package ("" if not imported).
func indexFileLocal(f *ast.File) string {
	for _, imp := range f.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p == indexImport {
			if imp.Name != nil {
				return imp.Name.Name
			}
			return "index"
		}
	}
	return ""
}

func isIndexPtrIn(local string, e ast.Expr) bool {
	st, ok := e.(*ast.StarExpr)
	if !ok || local == "" {
		return false
	}
	sel, ok := st.X.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Index" {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == local
}

// indexProducerPositions reports, for a call expression, the result positions
// that hold an *index.Index: Service.IndexManager() (position 0), index.New,
// and any function or method that scope or global (module-wide exported
// producers) records as returning one.
func indexProducerPositions(call ast.Expr, local string, sc *indexScope, global map[string][]int) []int {
	for {
		pe, ok := call.(*ast.ParenExpr)
		if !ok {
			break
		}
		call = pe.X
	}
	c, ok := call.(*ast.CallExpr)
	if !ok {
		return nil
	}
	switch fn := c.Fun.(type) {
	case *ast.Ident:
		if ps, ok := sc.producers[fn.Name]; ok {
			return ps
		}
	case *ast.SelectorExpr:
		if fn.Sel.Name == "IndexManager" {
			return []int{0}
		}
		if id, ok := fn.X.(*ast.Ident); ok && local != "" && id.Name == local && fn.Sel.Name == "New" {
			return []int{0}
		}
		if ps, ok := sc.producers[fn.Sel.Name]; ok {
			return ps
		}
		if ast.IsExported(fn.Sel.Name) {
			if ps, ok := global[fn.Sel.Name]; ok {
				return ps
			}
		}
	}
	return nil
}

// indexProducers collects, from one parsed file, the functions and methods
// whose results include an *index.Index (name -> result positions) and the
// exported field names of type *index.Index.
func indexProducers(f *ast.File, into map[string][]int, exportedFields map[string]bool) {
	local := indexFileLocal(f)
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncDecl:
			if x.Type.Results == nil {
				return true
			}
			pos := 0
			for _, r := range x.Type.Results.List {
				w := len(r.Names)
				if w == 0 {
					w = 1
				}
				for i := 0; i < w; i++ {
					if isIndexPtrIn(local, r.Type) {
						into[x.Name.Name] = append(into[x.Name.Name], pos)
					}
					pos++
				}
			}
		case *ast.StructType:
			for _, fl := range x.Fields.List {
				if isIndexPtrIn(local, fl.Type) {
					for _, nm := range fl.Names {
						if nm.IsExported() {
							exportedFields[nm.Name] = true
						}
					}
				}
			}
		}
		return true
	})
}

// indexBindings adds to sc.bound every name one file binds to an *index.Index:
// a parameter, field or var declared with the type; a var, := or = from a
// producer call (including the multi-value form `im, err := f()`); a struct
// literal key set from one; and a copy of an already bound name. It reports
// whether it added anything, so the caller can run it to a fixed point.
func indexBindings(f *ast.File, sc *indexScope, global map[string][]int, globalFields map[string]bool) bool {
	local := indexFileLocal(f)
	grew := false
	bind := func(e ast.Expr) {
		name := ""
		switch l := e.(type) {
		case *ast.Ident:
			name = l.Name
		case *ast.SelectorExpr:
			name = l.Sel.Name
		}
		if name != "" && name != "_" && !sc.bound[name] {
			sc.bound[name] = true
			grew = true
		}
	}
	isBound := func(e ast.Expr) bool {
		switch r := e.(type) {
		case *ast.Ident:
			return sc.bound[r.Name]
		case *ast.SelectorExpr:
			return sc.bound[r.Sel.Name] || (ast.IsExported(r.Sel.Name) && globalFields[r.Sel.Name])
		}
		return false
	}
	pair := func(lhs []ast.Expr, rhs []ast.Expr) {
		if len(rhs) == 1 && len(lhs) > 1 {
			for _, p := range indexProducerPositions(rhs[0], local, sc, global) {
				if p < len(lhs) {
					bind(lhs[p])
				}
			}
			return
		}
		for i, r := range rhs {
			if i >= len(lhs) {
				break
			}
			if len(indexProducerPositions(r, local, sc, global)) > 0 || isBound(r) {
				bind(lhs[i])
			}
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.Field:
			if isIndexPtrIn(local, x.Type) {
				for _, nm := range x.Names {
					bind(nm)
				}
			}
		case *ast.ValueSpec:
			if x.Type != nil && isIndexPtrIn(local, x.Type) {
				for _, nm := range x.Names {
					bind(nm)
				}
			}
			lhs := make([]ast.Expr, len(x.Names))
			for i, nm := range x.Names {
				lhs[i] = nm
			}
			pair(lhs, x.Values)
		case *ast.AssignStmt:
			pair(x.Lhs, x.Rhs)
		case *ast.KeyValueExpr:
			if len(indexProducerPositions(x.Value, local, sc, global)) > 0 || isBound(x.Value) {
				bind(x.Key)
			}
		}
		return true
	})
	return grew
}

// indexInternalsUsesPkg scans the files of ONE package (name -> source, or nil
// source to read the file) and returns "<file> line N: <name>" strings: one per
// selector `x.<name>` for a name in indexInternals (runtime.GC, the standard
// library's, is not one), and, in the files lockFree reports true for, per
// `x.Sync` whose receiver is an *index.Index: a call that produces one
// (IndexManager(), index.New, a function or method of the package or an
// exported one of the module that returns one), or a name the package binds to
// one in ANY of its files (a parameter, field or var of that type, a variable
// assigned from a producer, a copy of a bound name). Sync is the lock-FREE
// in-lock primitive (kb/invariants/store/index/branch-locking/f4911495.md);
// out-of-band callers use SyncLocked. Other types' Sync (os.File, remotesync's
// Sync) do not match.
func indexInternalsUsesPkg(t *testing.T, fset *token.FileSet, files map[string]any, lockFree func(file string) bool, global map[string][]int, globalFields map[string]bool) []string {
	t.Helper()
	parsed := map[string]*ast.File{}
	names := make([]string, 0, len(files))
	for name, src := range files {
		f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		parsed[name] = f
		names = append(names, name)
	}
	sort.Strings(names)
	sc := &indexScope{bound: map[string]bool{}, producers: map[string][]int{}}
	for _, name := range names {
		indexProducers(parsed[name], sc.producers, map[string]bool{})
	}
	for pass := 0; pass < 8; pass++ {
		grew := false
		for _, name := range names {
			if indexBindings(parsed[name], sc, global, globalFields) {
				grew = true
			}
		}
		if !grew {
			break
		}
	}
	var uses []string
	for _, name := range names {
		f := parsed[name]
		local := indexFileLocal(f)
		lf := lockFree(name)
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			line := name + " line " + strconv.Itoa(fset.Position(sel.Sel.Pos()).Line) + ": " + sel.Sel.Name
			if indexInternals[sel.Sel.Name] {
				if id, ok := sel.X.(*ast.Ident); !(ok && id.Name == "runtime") {
					uses = append(uses, line)
				}
				return true
			}
			if lf && sel.Sel.Name == "Sync" {
				recv := false
				x := sel.X
				for {
					pe, ok := x.(*ast.ParenExpr)
					if !ok {
						break
					}
					x = pe.X
				}
				switch r := x.(type) {
				case *ast.CallExpr:
					recv = len(indexProducerPositions(r, local, sc, global)) > 0
				case *ast.Ident:
					recv = sc.bound[r.Name]
				case *ast.SelectorExpr:
					recv = sc.bound[r.Sel.Name] || (ast.IsExported(r.Sel.Name) && globalFields[r.Sel.Name])
				}
				if recv {
					uses = append(uses, line)
				}
			}
			return true
		})
	}
	return uses
}

// indexInternalsUses is indexInternalsUsesPkg for a package of one file and no
// module-wide producers, the shape the discrimination table uses.
func indexInternalsUses(t *testing.T, fset *token.FileSet, name string, src any, lockFree bool) []string {
	t.Helper()
	return indexInternalsUsesPkg(t, fset, map[string]any{name: src}, func(string) bool { return lockFree }, nil, nil)
}

// TestStoreIndexInternals_OnlyInsideStore (PR E7a): Service.IndexManager()
// returns the concrete *index.Index, so every exported method of it is
// reachable from every package that holds a Service. This parses every .go
// file outside internal/store/** and fails on (a) any selector of
// SetSyncSuspended, GC, EnsureFactsVecDefault, PersistedEmbedModelID or
// PersistedEmbedDim, test files included (none outside the store needs them
// today), and (b) in non-test files, any lock-free Sync on an *index.Index.
// Tests outside the store may call the lock-free Sync to build fixtures; today
// none does, and no production file outside the store does.
//
// Non-vacuity: at least 500 files are walked; each name is still declared in
// internal/store/index and still selected by a non-test file elsewhere in the
// store (replay.go, service.go, fsck), and Sync is still declared.
func TestStoreIndexInternals_OnlyInsideStore(t *testing.T) {
	const minFiles = 500
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	var offenders []string
	scanned := 0
	// The lock-free Sync half is per PACKAGE: a field declared as *index.Index
	// in one file is used in another, so the files of a directory are
	// analysed together (indexInternalsUsesPkg). The module-wide pass first
	// collects the exported functions that return an *index.Index and the
	// exported fields of that type, which another package can reach.
	byDir := map[string][]string{}
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
		byDir[filepath.Dir(path)] = append(byDir[filepath.Dir(path)], path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	globalProducers := map[string][]int{}
	globalFields := map[string]bool{}
	for _, paths := range byDir {
		for _, path := range paths {
			f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if perr != nil {
				t.Fatalf("parse %s: %v", path, perr)
			}
			local := map[string][]int{}
			indexProducers(f, local, globalFields)
			for name, ps := range local {
				if ast.IsExported(name) {
					globalProducers[name] = ps
				}
			}
		}
	}
	dirs := make([]string, 0, len(byDir))
	for d := range byDir {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	for _, d := range dirs {
		files := map[string]any{}
		for _, path := range byDir[d] {
			files[path] = nil
		}
		for _, u := range indexInternalsUsesPkg(t, fset, files, func(file string) bool { return !strings.HasSuffix(file, "_test.go") }, globalProducers, globalFields) {
			offenders = append(offenders, filepath.ToSlash(strings.TrimPrefix(u, root+string(filepath.Separator))))
		}
	}
	if scanned < minFiles {
		t.Fatalf("scanned only %d .go files outside internal/store, want at least %d: the walk no longer reaches the module, so this guard checks nothing", scanned, minFiles)
	}

	declared := map[string]bool{}
	inside := map[string]bool{}
	storeRoot := filepath.Join(root, "internal", "store")
	err = filepath.WalkDir(storeRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		inIndex := filepath.ToSlash(filepath.Dir(path)) == filepath.ToSlash(filepath.Join(storeRoot, "index"))
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.FuncDecl:
				if inIndex && x.Recv != nil && (indexInternals[x.Name.Name] || x.Name.Name == "Sync") {
					declared[x.Name.Name] = true
				}
			case *ast.SelectorExpr:
				if !inIndex && indexInternals[x.Sel.Name] {
					inside[x.Sel.Name] = true
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for name := range indexInternals {
		if !declared[name] {
			t.Errorf("internal/store/index no longer declares a method %s: it was renamed or moved, so this guard checks nothing", name)
		}
		if !inside[name] {
			t.Errorf("no non-test file of internal/store outside the index package selects %s: the caller moved or was renamed, so this guard's expectations are stale", name)
		}
	}
	if !declared["Sync"] {
		t.Error("internal/store/index no longer declares a method Sync: it was renamed or moved, so the lock-free half of this guard checks nothing")
	}
	if len(offenders) > 0 {
		t.Fatalf("Service.IndexManager() hands out *index.Index, whose lifecycle surface is Sync(Locked)/Rebuild/SyncWatermark/NeedsRebuild; "+
			"SetSyncSuspended, GC, EnsureFactsVecDefault and the Persisted* readers are the store's own, and the lock-free Sync "+
			"assumes the branch lock (kb/invariants/store/index/branch-locking/f4911495.md):\n  %s", strings.Join(offenders, "\n  "))
	}
}

// TestStoreIndexInternals_GuardDiscriminates: each guarded name is flagged
// whatever the receiver; runtime.GC, a declaration, a comment and a string are
// not; the lock-free Sync is flagged only on an *index.Index (a call chain
// ending in IndexManager(), a parameter or field of that type, or a variable
// assigned from IndexManager()) and only when lockFree is set; SyncLocked and
// another type's Sync are not.
func TestStoreIndexInternals_GuardDiscriminates(t *testing.T) {
	imp := "package p\nimport \"" + indexImport + "\"\n"
	cases := []struct {
		name     string
		src      string
		lockFree bool
		want     int
	}{
		{"suspend", "package p\nfunc f() { svc.IndexManager().SetSyncSuspended(true) }", false, 1},
		{"gc", "package p\nfunc f() { svc.IndexManager().GC(nil) }", false, 1},
		{"ensure", "package p\nfunc f() { im.EnsureFactsVecDefault(ctx) }", false, 1},
		{"model id", "package p\nfunc f() { im.PersistedEmbedModelID(ctx) }", false, 1},
		{"dim", "package p\nfunc f() { im.PersistedEmbedDim(ctx) }", false, 1},
		{"method value", "package p\nfunc f() { g := im.GC; _ = g }", false, 1},
		{"runtime gc", "package p\nfunc f() { runtime.GC() }", false, 0},
		{"declaration", "package p\ntype I struct{}\nfunc (i *I) GC() {}", false, 0},
		{"comment", "package p\n// SetSyncSuspended is a flag\nfunc f() {}", false, 0},
		{"string", "package p\nvar s = \"im.GC()\"", false, 0},
		{"sync chain", "package p\nfunc f() { svc.IndexManager().Sync(ctx, b) }", true, 1},
		{"sync param", imp + "func f(im *index.Index) { im.Sync(ctx, b) }", true, 1},
		{"sync field", imp + "type h struct{ im *index.Index }\nfunc (x h) f() { x.im.Sync(ctx, b) }", true, 1},
		{"sync assigned", "package p\nfunc f() { im := svc.IndexManager(); im.Sync(ctx, b) }", true, 1},
		{"sync struct literal", "package p\nfunc f() { h := heal{im: svc.IndexManager()}; h.im.Sync(ctx, b) }", true, 1},
		{"sync in test", "package p\nfunc f() { svc.IndexManager().Sync(ctx, b) }", false, 0},
		{"sync locked", "package p\nfunc f() { svc.IndexManager().SyncLocked(ctx, b) }", true, 0},
		{"file sync", "package p\nfunc f() { tmp.Sync() }", true, 0},
		{"remote sync", "package p\nfunc f() { svc.Remote().Sync(ctx, b, nil) }", true, 0},
	}
	for _, c := range cases {
		got := indexInternalsUses(t, token.NewFileSet(), c.name+".go", c.src, c.lockFree)
		if len(got) != c.want {
			t.Errorf("%s: %d uses, want %d (%v)", c.name, len(got), c.want, got)
		}
	}

	// The lock-free Sync half works per package and follows producers, so the
	// shapes a one-file, assignment-only scan missed are flagged:
	//   V2 var x = svc.IndexManager(); x.Sync(...)
	//   V3 func f(svc) *index.Index, then f(svc).Sync(...)
	//   V4 a field declared *index.Index in one file, .Sync in another.
	pkgCases := []struct {
		name  string
		files map[string]any
		want  int
	}{
		{"V2 var spec", map[string]any{"a.go": "package p\nvar im = svc.IndexManager()\nfunc f() { im.Sync(ctx, b) }"}, 1},
		{"V2 local var spec", map[string]any{"a.go": "package p\nfunc f() { var im = svc.IndexManager(); im.Sync(ctx, b) }"}, 1},
		{"V2 typed var spec", map[string]any{"a.go": imp + "func f() { var im *index.Index; im.Sync(ctx, b) }"}, 1},
		{"V3 helper call", map[string]any{"a.go": imp + "func get(svc S) *index.Index { return svc.IndexManager() }\nfunc f() { get(svc).Sync(ctx, b) }"}, 1},
		{"V3 helper assigned", map[string]any{"a.go": imp + "func get(svc S) *index.Index { return svc.IndexManager() }\nfunc f() { im := get(svc); im.Sync(ctx, b) }"}, 1},
		{"V3 helper in another file", map[string]any{
			"a.go": imp + "func get(svc S) *index.Index { return svc.IndexManager() }",
			"b.go": "package p\nfunc f() { get(svc).Sync(ctx, b) }"}, 1},
		{"V3 method helper", map[string]any{"a.go": imp + "func (h H) idx() *index.Index { return h.svc.IndexManager() }\nfunc f(h H) { h.idx().Sync(ctx, b) }"}, 1},
		{"V3 multi-result helper", map[string]any{"a.go": imp + "func get() (*index.Index, error) { return nil, nil }\nfunc f() { im, err := get(); _ = err; im.Sync(ctx, b) }"}, 1},
		{"V4 field in another file", map[string]any{
			"a.go": imp + "type holder struct{ im *index.Index }",
			"b.go": "package p\nfunc (h holder) f() { h.im.Sync(ctx, b) }"}, 1},
		{"V4 field set from IndexManager in another file", map[string]any{
			"a.go": "package p\nfunc mk() holder { return holder{im: svc.IndexManager()} }",
			"b.go": "package p\nfunc (h holder) f() { h.im.Sync(ctx, b) }"}, 1},
		{"copy of a bound name", map[string]any{"a.go": "package p\nfunc f() { im := svc.IndexManager(); other := im; other.Sync(ctx, b) }"}, 1},
		{"SyncLocked through V2/V3/V4 shapes", map[string]any{
			"a.go": imp + "var im = svc.IndexManager()\nfunc get() *index.Index { return nil }\ntype holder struct{ x *index.Index }\nfunc f(h holder) { im.SyncLocked(ctx, b); get().SyncLocked(ctx, b); h.x.SyncLocked(ctx, b) }"}, 0},
		{"other type's Sync in a package with a bound name", map[string]any{
			"a.go": imp + "type holder struct{ im *index.Index }",
			"b.go": "package p\nfunc f(file F) { file.Sync(); tmp.Sync() }"}, 0},
		{"helper returning another type", map[string]any{"a.go": imp + "func get() *os.File { return nil }\nfunc f() { get().Sync() }"}, 0},
	}
	for _, c := range pkgCases {
		got := indexInternalsUsesPkg(t, token.NewFileSet(), c.files, func(string) bool { return true }, nil, nil)
		if len(got) != c.want {
			t.Errorf("%s: %d uses, want %d (%v)", c.name, len(got), c.want, got)
		}
	}
	// A producer or field EXPORTED by another package of the module is found
	// through the module-wide sets the walk passes in.
	global := map[string][]int{"Idx": {0}}
	gotX := indexInternalsUsesPkg(t, token.NewFileSet(), map[string]any{"a.go": "package q\nfunc f() { other.Idx(svc).Sync(ctx, b) }"}, func(string) bool { return true }, global, nil)
	if len(gotX) != 1 {
		t.Errorf("exported producer of another package: %d uses, want 1 (%v)", len(gotX), gotX)
	}
	gotF := indexInternalsUsesPkg(t, token.NewFileSet(), map[string]any{"a.go": "package q\nfunc f(h other.H) { h.Im.Sync(ctx, b) }"}, func(string) bool { return true }, nil, map[string]bool{"Im": true})
	if len(gotF) != 1 {
		t.Errorf("exported field of another package: %d uses, want 1 (%v)", len(gotF), gotF)
	}
}
