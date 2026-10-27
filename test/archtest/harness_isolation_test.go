package archtest

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// forbidden lists the test-only support packages that must NEVER be linked into
// a shipped binary. They import "testing" and stand up fault servers, bare git
// repos, etc.; they exist only for _test.go files to import. If a production
// package ever imports one (directly or transitively), it would bloat the real
// binary with test scaffolding — this guard fails first.
var forbidden = []string{
	"knomit/test/testenv", // + /gitserver and any subpackage (prefix match)
	"knomit/test/storytests",
	// Installs a fallback commit signer for test binaries (F09 PR 2). The hook
	// it calls refuses to work outside a test binary anyway; this keeps the
	// package itself out of every shipped binary too.
	"knomit/internal/testsupport/testsigner",
	// Module-root lookup for tests that scan source across packages; imports
	// "testing", so it stays out of every shipped binary.
	"knomit/internal/testsupport/modroot",
	// Bare-repository fixtures for tests (git's auto-maintenance off); imports
	// "testing", so it stays out of every shipped binary.
	"knomit/internal/testsupport/gitfixture",
	// The test doors into a *store.Service (corrupt an object, plant a ref,
	// commit without the index, backdate an experiment). Its init panics outside
	// a test binary; this keeps the package itself out of every shipped binary
	// too, and TestTestSeamsNotReachedFromProduction keeps production source
	// from importing it.
	"knomit/internal/engine/store/storetest",
	// The synthesize tree's shared test fixtures (store repos, embedder
	// stand-ins, environment builders). It imports "testing" and is meant for
	// _test.go files only; living under synthesize/internal/ keeps every package
	// outside the tree from importing it, and this keeps the package out of every
	// shipped binary even if a production file inside the tree ever did.
	"knomit/internal/corpus/synthesize/internal/synthtest",
}

// shippedGroups are the module's real binaries (main packages), grouped by the
// build tags needed to resolve them. The desktop app lives behind //go:build
// desktop, so it needs the tag to appear at all.
var shippedGroups = []struct {
	name string
	tags string
	pkgs []string
}{
	{
		name: "default binaries",
		pkgs: []string{
			"knomit",
			"knomit/tools/appcast",
			"knomit/tools/bridge",
			"knomit/tools/calibrate",
			"knomit/tools/fetchlibs",
			"knomit/tools/okf",
			"knomit/tools/relnotes",
		},
	},
	{
		name: "desktop app",
		tags: "desktop",
		pkgs: []string{"knomit/tools/desktop"},
	},
}

// TestHarnessNotLinkedIntoShippedBinaries walks the transitive import graph of
// every shipped binary (via `go list -deps`, which resolves imports without
// compiling, so it needs no native libs) and fails if any forbidden test-only
// package appears. This is the enforcement behind "the test harness never ships
// in the real binary": Go already excludes packages nothing imports, and this
// keeps it that way as the code evolves.
func TestHarnessNotLinkedIntoShippedBinaries(t *testing.T) {
	for _, grp := range shippedGroups {
		args := []string{"list", "-deps"}
		if grp.tags != "" {
			args = append(args, "-tags", grp.tags)
		}
		args = append(args, grp.pkgs...)

		out, err := exec.Command("go", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: go list -deps failed: %v\n%s", grp.name, err, out)
		}

		for _, line := range strings.Split(string(out), "\n") {
			dep := strings.TrimSpace(line)
			for _, f := range forbidden {
				if dep == f || strings.HasPrefix(dep, f+"/") {
					t.Errorf("%s: a shipped binary transitively imports test-only package %q — "+
						"the test harness must never ship in the real binary. Break the import "+
						"(move the helper into a _test.go file or a test-only package).", grp.name, dep)
				}
			}
		}
	}
}

// TestShippedGroupsListEveryMainPackage keeps shippedGroups complete: B1 only
// checks the binaries it lists, so a new `package main` (tools/appcast,
// tools/okf and tools/relnotes were missing for a while) would otherwise ship
// without the storetest and harness checks. It asks `go list` for every main
// package in the module under the default tags and the desktop tag and fails
// on one that no group names.
func TestShippedGroupsListEveryMainPackage(t *testing.T) {
	listed := map[string]bool{}
	for _, grp := range shippedGroups {
		for _, p := range grp.pkgs {
			listed[p] = true
		}
	}
	seen := 0
	for _, tags := range []string{"", "desktop"} {
		args := []string{"list", "-f", "{{if eq .Name \"main\"}}{{.ImportPath}}{{end}}"}
		if tags != "" {
			args = append(args, "-tags", tags)
		}
		args = append(args, "./...")
		cmd := exec.Command("go", args...)
		cmd.Dir = moduleRoot(t)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("go list (tags %q): %v", tags, err)
		}
		for _, p := range strings.Fields(string(out)) {
			seen++
			if !listed[p] {
				t.Errorf("main package %s is not in shippedGroups: add it so TestHarnessNotLinkedIntoShippedBinaries checks it", p)
			}
		}
	}
	if seen < len(listed) {
		t.Errorf("go list found %d main packages, shippedGroups names %d: the listing matches nothing", seen, len(listed))
	}
}

// bareFixturePkg is the one place test code may create a bare git
// repository: it turns git's auto-maintenance off in every one (#651, #654).
const bareFixturePkg = "internal/testsupport/gitfixture"

// bareRepoAllowlist is every file (module-relative) outside bareFixturePkg
// that may create a bare repository directly, with the reason. Empty: no
// exception has been needed. A stale entry fails.
var bareRepoAllowlist = map[string]string{}

// TestBareReposComeFromGitfixture: test code creates a bare git repository
// only through internal/testsupport/gitfixture (InitBare, CloneBare, or
// NoAutoMaintenance right after go-git made one). A bare repository made any
// other way keeps git's auto-maintenance on, and every push into it leaves a
// detached `git maintenance run` holding objects/maintenance.lock past the
// push: the cause of #651's byte-snapshot and t.TempDir-cleanup flakes.
//
// It scans every .go file of the module (third_party excluded) that is test
// code: a _test.go file, or any file under test/ or internal/testsupport/.
// Flagged, as syntax (comments are not):
//   - any string literal, interpreted or raw, that holds --bare or --mirror
//     as a whitespace-separated word: a git argument, or a whole command line
//     handed to `sh -c`;
//   - go-git PlainInit, PlainClone or PlainCloneContext whose isBare argument
//     is not the literal false, unless the same function calls
//     gitfixture.NoAutoMaintenance on the same directory expression;
//   - a Bare field set to anything but the literal false in a go-git
//     *InitOptions or *CloneOptions literal, unless the function calls
//     gitfixture.NoAutoMaintenance at all (see bareRepoCreations for why).
//
// SABOTAGE (each red, naming the file and line): a stray
// exec.Command("git", "init", "--bare", dir); the flag as a raw string; a
// `sh -c "git init --bare ..."`; PlainCloneContext(ctx, d, true, ...); a
// PlainInit(d, true) whose NoAutoMaintenance names another directory.
func TestBareReposComeFromGitfixture(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()
	var bad []string
	allowed := map[string]bool{}
	scanned := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == "third_party" || rel == "web" || rel == "dist" || strings.HasPrefix(d.Name(), ".") && rel != "." {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasPrefix(rel, bareFixturePkg+"/") {
			return nil
		}
		if !strings.HasSuffix(rel, "_test.go") && !strings.HasPrefix(rel, "test/") && !strings.HasPrefix(rel, "internal/testsupport/") {
			return nil
		}
		f, perr := parser.ParseFile(fset, p, nil, 0)
		if perr != nil {
			return perr
		}
		scanned++
		hits := bareRepoCreations(fset, f)
		if len(hits) == 0 {
			return nil
		}
		if _, ok := bareRepoAllowlist[rel]; ok {
			allowed[rel] = true
			return nil
		}
		for _, h := range hits {
			bad = append(bad, rel+":"+h)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 100 {
		t.Fatalf("reached: only %d test files scanned under %s; the walk is broken", scanned, root)
	}
	for _, b := range bad {
		t.Errorf("creates a bare git repository outside %s (use gitfixture.InitBare/CloneBare, or call gitfixture.NoAutoMaintenance on it): %s", bareFixturePkg, b)
	}
	for rel := range bareRepoAllowlist {
		if !allowed[rel] {
			t.Errorf("bareRepoAllowlist entry %s is stale: the file creates no bare repository directly", rel)
		}
	}
}

// bareFlags are the git arguments that make a bare repository, built so this
// file holds no such literal itself (it is test code the guard scans).
var bareFlags = map[string]bool{"--" + "bare": true, "--" + "mirror": true}

// bareRepoCreations returns "<line>: <what>" for every direct bare-repository
// creation in f (see TestBareReposComeFromGitfixture).
//
// The exemption for go-git calls is per DIRECTORY where the call names one:
// PlainInit(dir, true) / PlainClone(dir, true, ...) / PlainCloneContext(ctx,
// dir, true, ...) is fixed only by gitfixture.NoAutoMaintenance(t, dir) in
// the same function with the same dir expression (compared as source text).
// A *InitOptions{Bare: true} / *CloneOptions{Bare: true} literal carries no
// directory (the call it is passed to does, possibly through a variable), so
// it keeps the coarser per-function exemption: any NoAutoMaintenance call in
// the enclosing function. go-git's Init/Clone with a nil worktree also make a
// bare repository, but on a storer, and the tree uses them only on in-memory
// storage, where no git-receive-pack ever runs; they are not flagged.
func bareRepoCreations(fset *token.FileSet, f *ast.File) []string {
	var out []string
	notFalse := func(e ast.Expr) bool {
		id, ok := e.(*ast.Ident)
		return !ok || id.Name != "false"
	}
	// fixedDirs returns the source text of every directory argument the
	// function enclosing pos passes to gitfixture.NoAutoMaintenance, and
	// whether it calls it at all.
	fixedDirs := func(pos token.Pos) (map[string]bool, bool) {
		dirs := map[string]bool{}
		called := false
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil || pos < fd.Pos() || pos > fd.End() {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				c, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				s, ok := c.Fun.(*ast.SelectorExpr)
				if !ok || s.Sel.Name != "NoAutoMaintenance" {
					return true
				}
				if x, ok := s.X.(*ast.Ident); ok && x.Name == "gitfixture" {
					called = true
					if len(c.Args) == 2 {
						dirs[types.ExprString(c.Args[1])] = true
					}
				}
				return true
			})
		}
		return dirs, called
	}
	// isBareArg is the position of the isBare argument and of the directory
	// argument of each go-git creator.
	isBareArg := map[string][2]int{
		"PlainInit":         {1, 0},
		"PlainClone":        {1, 0},
		"PlainCloneContext": {2, 1},
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.BasicLit:
			if n.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(n.Value)
			if err != nil {
				return true
			}
			for _, field := range strings.Fields(s) {
				if bareFlags[field] {
					out = append(out, fmt.Sprintf("%d: git argument %s in %s", fset.Position(n.Pos()).Line, field, n.Value))
					break
				}
			}
		case *ast.CallExpr:
			s, ok := n.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			idx, ok := isBareArg[s.Sel.Name]
			if !ok || len(n.Args) <= idx[0] || !notFalse(n.Args[idx[0]]) {
				return true
			}
			dirs, _ := fixedDirs(n.Pos())
			if dir := types.ExprString(n.Args[idx[1]]); !dirs[dir] {
				out = append(out, fmt.Sprintf("%d: %s(%s) with isBare not false and no gitfixture.NoAutoMaintenance(t, %s) in the function",
					fset.Position(n.Pos()).Line, s.Sel.Name, dir, dir))
			}
		case *ast.CompositeLit:
			name := ""
			switch ty := n.Type.(type) {
			case *ast.SelectorExpr:
				name = ty.Sel.Name
			case *ast.Ident:
				name = ty.Name
			}
			if !strings.HasSuffix(name, "InitOptions") && !strings.HasSuffix(name, "CloneOptions") {
				return true
			}
			for _, el := range n.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Bare" && notFalse(kv.Value) {
					if _, called := fixedDirs(n.Pos()); !called {
						out = append(out, fmt.Sprintf("%d: %s{Bare: ...}", fset.Position(n.Pos()).Line, name))
					}
				}
			}
		}
		return true
	})
	return out
}
