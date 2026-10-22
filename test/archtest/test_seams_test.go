package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
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
// (test/, internal/testsupport/) and the testseams.go file of any store
// package, which is where a child defines its seams and its registration hook.
func seamAllowed(rel string) bool {
	switch {
	case strings.HasPrefix(rel, "internal/engine/store/storetest/"),
		strings.HasPrefix(rel, "test/"),
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
			// ("web") also exempts internal/web.
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
			"(allowed: internal/engine/store/storetest, test/, internal/testsupport/, internal/engine/store/**/testseams.go):\n  %s",
			strings.Join(offenders, "\n  "))
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
	for rel, want := range map[string]bool{
		"internal/engine/store/storetest/storetest.go":    true,
		"internal/engine/store/internal/vcs/testseams.go": true,
		"internal/engine/store/merge/testseams.go":        true,
		"test/testenv/corrupt.go":                         true,
		"internal/testsupport/testsigner/signer.go":       true,
		"internal/repos/experiment_sweep.go":              false,
		"internal/engine/store/service.go":                false,
		"internal/testseams.go":                           false,
		"internal/engine/store/vcs_seam.go":               false,
	} {
		if seamAllowed(rel) != want {
			t.Errorf("seamAllowed(%q) = %v, want %v", rel, !want, want)
		}
	}
}
