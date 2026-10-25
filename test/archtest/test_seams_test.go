package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// storetestImport is the package of test doors into a *store.Service.
const storetestImport = "knomit/internal/engine/store/storetest"

// testSeamName matches the two spellings a test-only door takes: a *ForTest
// method or function (vcs.Repo.SetRefForTest, facts.Index.WriteFileExactForTest)
// and a Testing* one (the old Service.TestingCommitFiles). `testing.Testing()`
// does not match: nothing follows "Testing".
var testSeamName = regexp.MustCompile(`^(?:[A-Z]\w*ForTest|Testing[A-Z]\w*)$`)

// seamAllowed reports whether the module-root-relative, slash-separated file
// may import storetest and name a test seam: storetest itself, the test trees
// (test/, internal/testsupport/), the synthesize tree's test-support package
// (internal/corpus/synthesize/internal/synthtest/, which TestHarnessNotLinkedIntoShippedBinaries
// keeps out of every shipped binary) and the testseams.go file of any store
// package, which is where a child defines its seams and its registration hook.
func seamAllowed(rel string) bool {
	switch {
	case strings.HasPrefix(rel, "internal/engine/store/storetest/"),
		strings.HasPrefix(rel, "test/"),
		strings.HasPrefix(rel, "internal/corpus/synthesize/internal/synthtest/"),
		strings.HasPrefix(rel, "internal/testsupport/"):
		return true
	case strings.HasPrefix(rel, "internal/engine/store/") && filepath.Base(rel) == "testseams.go":
		return true
	}
	return false
}

// seamUses parses one Go source and returns, for every use of a test seam, a
// "line N: <what>" string: an import of storetest, a selector `x.FooForTest` or
// `x.TestingFoo` (called, passed as a method value, or assigned), or a bare
// identifier `FooForTest` (called or taken as a value, from inside the
// declaring package or through a dot import). A declaration
// (`func (r *Repo) FooForTest()`) is not a use.
func seamUses(t *testing.T, fset *token.FileSet, name string, src any) []string {
	t.Helper()
	f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	var uses []string
	at := func(p token.Pos) int { return fset.Position(p).Line }
	for _, imp := range f.Imports {
		if strings.Trim(imp.Path.Value, `"`) == storetestImport {
			uses = append(uses, "line "+strconv.Itoa(at(imp.Pos()))+": imports "+storetestImport)
		}
	}
	// Identifiers that are DECLARATIONS or the selector half of `x.Name`, not
	// bare uses: the second kind is reported by the SelectorExpr case below.
	skip := map[*ast.Ident]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncDecl:
			skip[x.Name] = true
		case *ast.TypeSpec:
			skip[x.Name] = true
		case *ast.ValueSpec:
			for _, id := range x.Names {
				skip[id] = true
			}
		case *ast.Field:
			for _, id := range x.Names {
				skip[id] = true
			}
		case *ast.AssignStmt:
			if x.Tok == token.DEFINE {
				for _, l := range x.Lhs {
					if id, ok := l.(*ast.Ident); ok {
						skip[id] = true
					}
				}
			}
		case *ast.SelectorExpr:
			skip[x.Sel] = true
		}
		return true
	})
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if testSeamName.MatchString(x.Sel.Name) {
				uses = append(uses, "line "+strconv.Itoa(at(x.Sel.Pos()))+": uses "+x.Sel.Name)
			}
		case *ast.Ident:
			// A bare identifier: a call from inside the declaring package, a
			// function value (`f := SetRefForTest`), or a use through a dot
			// import (`import . "knomit/internal/engine/store/..."`). A bare CALL was
			// the only shape caught before.
			if !skip[x] && testSeamName.MatchString(x.Name) {
				uses = append(uses, "line "+strconv.Itoa(at(x.Pos()))+": uses bare "+x.Name)
			}
		}
		return true
	})
	return uses
}

// TestTestSeamsNotReachedFromProduction: storetest and the *ForTest / Testing*
// doors behind it exist so a TEST can corrupt an object, plant a ref, commit
// without the index or backdate an experiment. Each door refuses to run outside
// a test binary (mustBeTest), and storetest panics in init outside one, but a
// production file that names one would still be a production path to a
// store-corrupting door, and a build that links it dies at start-up instead of
// failing review. This test keeps production SOURCE from naming one.
//
// It parses every non-_test.go file in the module (AST, so a comment or string
// that mentions a seam does not count and an aliased import cannot hide one)
// and fails on a use outside the allowed files: storetest, test/,
// internal/testsupport/ and the testseams.go files under internal/engine/store/.
//
// Non-vacuity: the walk must visit at least minProductionFiles files, find at
// least minAllowedUses seam uses in the allowed files (storetest alone makes
// more than that), and see every door named below defined or used where it
// should be. A rename that left the scan matching nothing fails here.
func TestTestSeamsNotReachedFromProduction(t *testing.T) {
	const (
		minProductionFiles = 500
		minAllowedUses     = 12
	)
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	var offenders []string
	scanned, allowedUses := 0, 0
	storetestFiles := 0
	seenInStoretest := map[string]bool{}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			// Skipped by PATH at the module root, never by name: a name match
			// ("web") also exempts internal/edge/web.
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
		uses := seamUses(t, fset, path, nil)
		if seamAllowed(rel) {
			allowedUses += len(uses)
			if strings.HasPrefix(rel, "internal/engine/store/storetest/") {
				storetestFiles++
				for _, u := range uses {
					if _, name, ok := strings.Cut(u, "uses "); ok {
						seenInStoretest[name] = true
					}
				}
			}
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

	if scanned < minProductionFiles {
		t.Fatalf("scanned only %d non-test .go files, want at least %d: the walk no longer reaches the module, so this guard checks nothing", scanned, minProductionFiles)
	}
	if storetestFiles == 0 {
		t.Fatal("internal/engine/store/storetest has no non-test file: the package moved, so this guard's allowlist is stale")
	}
	if allowedUses < minAllowedUses {
		t.Fatalf("found %d test-seam uses in the allowed files, want at least %d: the seam names changed shape, so the pattern matches nothing", allowedUses, minAllowedUses)
	}
	for _, want := range []string{
		"ServiceRepoForTest", "SetRefForTest", "DeleteRefForTest",
		"CommitFilesForTest", "RawSymlinkForTest", "WriteFileExactForTest", "FetchRefspecsForTest",
		"RemoteURLsForTest", "DeleteObjectForTest", "SetExperimentActivityForTest",
	} {
		if !seenInStoretest[want] {
			t.Errorf("storetest no longer uses %s: a door was renamed or dropped, so this guard's expectations are stale", want)
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("production source must not import storetest or use a *ForTest / Testing* door "+
			"(allowed: internal/engine/store/storetest, test/, internal/testsupport/, internal/corpus/synthesize/internal/synthtest/, internal/engine/store/**/testseams.go):\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// faultHook identifies one per-owner fail-only read hook by the package that
// owns it, its receiver type and its name. The name alone is not enough:
// SetReadFaultForTest exists on both index.Facade and vocab.MotifIndex, and a
// check by name would stay green when one of the two left its testseams.go.
type faultHook struct{ pkg, recv, name string }

// faultHookSetters are the hooks the web tests use to make a read or write fail
// on one store.Service. Each is a *ForTest door that must be declared in its
// owner's internal/engine/store/<pkg>/testseams.go, so B2 (the walk above)
// keeps production source from naming it.
var faultHookSetters = []faultHook{
	{"index", "Facade", "SetReadFaultForTest"},
	{"vocab", "MotifIndex", "SetReadFaultForTest"},
	{"facts", "Index", "SetReadFactFaultForTest"},
	{"facts", "Index", "SetListDirFaultForTest"},
	{"facts", "Index", "SetWriteFaultForTest"},
	{"facts", "Index", "SetDeleteFaultForTest"},
}

// recvTypeName is the receiver's type name of a method, "" for a function.
func recvTypeName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) != 1 {
		return ""
	}
	t := fn.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// startsWithMustBeTest reports whether the first statement of body is the call
// mustBeTest("<name>").
func startsWithMustBeTest(body *ast.BlockStmt, name string) bool {
	if body == nil || len(body.List) == 0 {
		return false
	}
	es, ok := body.List[0].(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := es.X.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return false
	}
	fun, ok := call.Fun.(*ast.Ident)
	if !ok || fun.Name != "mustBeTest" {
		return false
	}
	lit, ok := call.Args[0].(*ast.BasicLit)
	return ok && lit.Value == strconv.Quote(name)
}

// TestFaultHookSettersDeclaredInTestseams: every hook in faultHookSetters is
// declared exactly once, as a method on its receiver, in
// internal/engine/store/<pkg>/testseams.go, and starts with mustBeTest(name).
// A declaration of any listed setter NAME in another non-test file under
// internal/engine/store/** fails: moving a setter out of testseams.go would take
// it out of the only files B2 lets name a seam, and its mustBeTest with it.
func TestFaultHookSettersDeclaredInTestseams(t *testing.T) {
	root := moduleRoot(t)
	storeDir := filepath.Join(root, "internal", "engine", "store")
	names := map[string]bool{}
	for _, h := range faultHookSetters {
		names[h.name] = true
		if !testSeamName.MatchString(h.name) {
			t.Errorf("%s does not match the seam pattern, so B2 would not flag a production use", h.name)
		}
	}
	found := map[faultHook]int{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(storeDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		rel, _ := filepath.Rel(storeDir, path)
		rel = filepath.ToSlash(rel)
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !names[fn.Name.Name] {
				continue
			}
			h := faultHook{pkg: filepath.ToSlash(filepath.Dir(rel)), recv: recvTypeName(fn), name: fn.Name.Name}
			listed := false
			for _, want := range faultHookSetters {
				if want == h {
					listed = true
				}
			}
			if !listed || filepath.Base(rel) != "testseams.go" {
				t.Errorf("%s declares %s.%s(%s): a fault-hook setter must be a listed (package, receiver, name) declared in its owner's testseams.go", rel, h.pkg, h.recv, h.name)
				continue
			}
			found[h]++
			if !startsWithMustBeTest(fn.Body, h.name) {
				t.Errorf("%s: %s.%s must start with mustBeTest(%q)", rel, h.recv, h.name, h.name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range faultHookSetters {
		if found[h] != 1 {
			t.Errorf("%s.%s.%s is declared %d times in internal/engine/store/%s/testseams.go, want exactly 1: it moved, was renamed, or its receiver changed", h.pkg, h.recv, h.name, found[h], h.pkg)
		}
	}
}

// triggerSeams are the doors the repos parent's tests use into the trigger
// child (internal/corpus/repos/trigger, step 14c): the dispatcher's test hooks,
// its run bookkeeping and its configuration. recv is "" for a function. Each
// must be declared in the child's testseams.go and start with mustBeTest, so a
// production caller panics and B2 (the walk above, which matches the *ForTest
// names) keeps production source from naming it.
var triggerSeams = []faultHook{
	{"trigger", "", "SetTestHooksForTest"},
	{"trigger", "", "TestHooksForTest"},
	{"trigger", "", "SetRecipeAfterStartForTest"},
	{"trigger", "", "SetRecipeServerAddrWaitForTest"},
	{"trigger", "", "RecipeMCPForTest"},
	{"trigger", "Dispatcher", "RunSequenceForTest"},
	{"trigger", "Dispatcher", "FlushedForTest"},
	{"trigger", "Dispatcher", "StatsViewForTest"},
	{"trigger", "Dispatcher", "RunStatsForTest"},
	{"trigger", "Dispatcher", "RunOnceForTest"},
	{"trigger", "Dispatcher", "FlushForTest"},
	{"trigger", "Dispatcher", "IndexIsReadyForTest"},
	{"trigger", "Dispatcher", "ScriptCompilesForTest"},
	{"trigger", "Dispatcher", "HomeForTest"},
	{"trigger", "Dispatcher", "SlowForTest"},
	{"trigger", "Dispatcher", "IdentityForTest"},
}

// TestTriggerSeamsDeclaredInTestseams: every door in triggerSeams is declared
// exactly once, on its receiver, in internal/corpus/repos/trigger/testseams.go,
// and starts with mustBeTest(name); a declaration of a listed name in any other
// non-test file under internal/corpus/repos/** fails (moving a door out of
// testseams.go would take its mustBeTest with it), and so does a *ForTest
// function or method in the child's testseams.go that is not listed (a new
// door must be listed here to be checked).
func TestTriggerSeamsDeclaredInTestseams(t *testing.T) {
	root := moduleRoot(t)
	reposDir := filepath.Join(root, "internal", "corpus", "repos")
	names := map[string]bool{}
	for _, h := range triggerSeams {
		names[h.name] = true
		if !testSeamName.MatchString(h.name) {
			t.Errorf("%s does not match the seam pattern, so B2 would not flag a production use", h.name)
		}
	}
	found := map[faultHook]int{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(reposDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		rel, _ := filepath.Rel(reposDir, path)
		rel = filepath.ToSlash(rel)
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			inSeams := rel == "trigger/testseams.go"
			if !names[fn.Name.Name] {
				if inSeams && testSeamName.MatchString(fn.Name.Name) {
					t.Errorf("%s declares %s, which triggerSeams does not list: list it so its mustBeTest is checked", rel, fn.Name.Name)
				}
				continue
			}
			h := faultHook{pkg: filepath.ToSlash(filepath.Dir(rel)), recv: recvTypeName(fn), name: fn.Name.Name}
			listed := false
			for _, want := range triggerSeams {
				if want == h {
					listed = true
				}
			}
			if !listed || !inSeams {
				t.Errorf("%s declares %s.%s(%s): a trigger seam must be a listed (package, receiver, name) declared in trigger/testseams.go", rel, h.pkg, h.recv, h.name)
				continue
			}
			found[h]++
			if !startsWithMustBeTest(fn.Body, h.name) {
				t.Errorf("%s: %s must start with mustBeTest(%q)", rel, h.name, h.name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range triggerSeams {
		if found[h] != 1 {
			t.Errorf("%s.%s.%s is declared %d times in internal/corpus/repos/trigger/testseams.go, want exactly 1: it moved, was renamed, or its receiver changed", h.pkg, h.recv, h.name, found[h])
		}
	}
}

// TestTestSeamsGuard_Discriminates runs the guard's detector on inline source:
// every shape it must flag is flagged, and every shape it must leave alone is
// left alone. Without it a pattern that matched nothing would pass the walk
// above whenever production happened to be clean.
func TestTestSeamsGuard_Discriminates(t *testing.T) {
	src := func(body string) string { return "package p\n" + body }
	flagged := map[string]string{
		"storetest import":          src(`import "knomit/internal/engine/store/storetest"` + "\n"),
		"aliased storetest import":  src(`import st "knomit/internal/engine/store/storetest"` + "\n"),
		"blank storetest import":    src(`import _ "knomit/internal/engine/store/storetest"` + "\n"),
		"ForTest call":              src("func f(r R) { r.SetRefForTest(\"a\", \"b\") }\n"),
		"ForTest method value":      src("func f(r R) { _ = r.SetExperimentActivityForTest }\n"),
		"package-qualified ForTest": src("func f() { vcs.SetBatchWriteUnlockedHookForTest(nil) }\n"),
		"Testing* call":             src("func f(s S) { s.TestingCommitFiles(\"a\", nil, \"m\") }\n"),
		"bare in-package call":      src("func f() { MetaGetForTest(nil, nil, \"k\") }\n"),
		"bare function value":       src("func f() { g := SetRefForTest; _ = g }\n"),
		"bare as argument":          src("func f() { run(SetRefForTest) }\n"),
		"bare dot-import use":       src("import . \"knomit/internal/engine/store/x\"\nvar h = TestingCommitFiles\n"),
	}
	// The per-owner fail-only read hooks: production naming one is a path to a
	// read that fails on demand.
	for _, h := range faultHookSetters {
		flagged["fault hook "+h.pkg+"."+h.recv+"."+h.name] = src("func f(s S) { _ = s.X()." + h.name + "(nil) }\n")
	}
	// The trigger child's doors: production naming one reaches the
	// dispatcher's test hooks or its internals.
	for _, h := range triggerSeams {
		if h.recv == "" {
			flagged["trigger seam "+h.name] = src("func f() { trigger." + h.name + "(nil) }\n")
		} else {
			flagged["trigger seam "+h.recv+"."+h.name] = src("func f(ri R) { _ = ri.Triggers()." + h.name + "() }\n")
		}
	}
	for name, s := range flagged {
		if got := seamUses(t, token.NewFileSet(), name+".go", s); len(got) == 0 {
			t.Errorf("%s: not flagged", name)
		}
	}
	clean := map[string]string{
		"declaration only":         src("func (r R) SetRefForTest(a, b string) error { return nil }\n"),
		"testing.Testing()":        src("import \"testing\"\nfunc f() bool { return testing.Testing() }\n"),
		"name in a comment":        src("// r.SetRefForTest and storetest.SetRef\nfunc f() {}\n"),
		"name in a string":         src("var s = \"r.SetRefForTest\"\n"),
		"lowercase suffix":         src("func f(r R) { r.SetRefforTest() }\n"),
		"struct field decl":        src("type T struct{ SetRefForTest int }\n"),
		"param named like a seam":  src("func f(SetRefForTest int) {}\n"),
		"Register hook":            src("func f(r R) { r.RegisterServiceRepo(nil) }\n"),
		"other storetest-ish path": src(`import "knomit/internal/engine/store/storetest2"` + "\n"),
	}
	for name, s := range clean {
		if got := seamUses(t, token.NewFileSet(), name+".go", s); len(got) != 0 {
			t.Errorf("%s: wrongly flagged: %v", name, got)
		}
	}
	// Every row names a file that exists, except those in notAFile: a
	// negative row is false at ANY path, so without this a moved or renamed
	// file would leave its row green and unchecked.
	notAFile := map[string]bool{
		"internal/testseams.go": true, // the prefix clause: a testseams.go outside internal/engine/store/ is not a door
	}
	root := moduleRoot(t)
	for rel, want := range map[string]bool{
		"internal/engine/store/storetest/storetest.go":    true,
		"internal/engine/store/internal/vcs/testseams.go": true,
		"internal/engine/store/merge/testseams.go":        true,
		"test/testenv/corrupt.go":                         true,
		"internal/testsupport/testsigner/testsigner.go":   true,
		"internal/corpus/repos/experiment_sweep.go":       false,
		"internal/engine/store/service.go":                false,
		"internal/testseams.go":                           false,
		"internal/engine/store/vcs_seam.go":               false,
	} {
		if seamAllowed(rel) != want {
			t.Errorf("seamAllowed(%q) = %v, want %v", rel, !want, want)
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil && !notAFile[rel] {
			t.Errorf("seamAllowed table row %q names no file (%v): the file moved or was renamed, so the row checks nothing; update the path", rel, err)
		}
	}
}
