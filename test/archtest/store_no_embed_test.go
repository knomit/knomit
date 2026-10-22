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

// storePkgPrefix is the import-path root of the store and every child under it.
const storePkgPrefix = "knomit/internal/engine/store/"

// embedAllowed is every struct under internal/engine/store/** that may embed a type
// from another store package, keyed "<root-relative file>|<struct>|<embedded
// type as written>". Each entry says why; a stale entry fails the test, so the
// list cannot outlive the embed it excuses.
//
// An embed promotes EVERY exported method of the embedded type onto the
// struct, so a struct that is handed out (or whose methods are called through
// a field a caller can reach) silently widens its API by whatever the child
// grows. The store's own handles (Service, repoHandler) held the facts, merge
// and remotesync children by embedding until E7c; they are named fields now.
var embedAllowed = map[string]string{
	"internal/engine/store/vcs_seam.go|repoHandler|vcs.Repo":                      "the one shared vcs handle; repoHandler is unexported, never handed out, and the parent's own code calls the vcs.Repo surface through it (the documented vcs exception)",
	"internal/engine/store/gitserve/host.go|host|vcs.Repo":                        "unexported per-handler view of the git layer, built inside NewHandler and never returned; every receive-pack and upload-pack path calls the vcs surface on it",
	"internal/engine/store/index/search_query.go|SearchResult|facts.FactWithBody": "a data struct that flattens FactWithBody's fields into the JSON search result; FactWithBody has no behaviour to leak",
}

// embedUses returns, for each struct type in one parsed file that embeds a
// type from a store package, "<Struct>|<embedded type as written>". aliases
// holds the same-package names declared as `type X = <store pkg>.Y`, so the
// old shape (`type factsRepo = facts.Repo` then `*factsRepo` in a struct) is
// caught as well as a direct `*facts.Repo`.
func embedUses(t *testing.T, fset *token.FileSet, name string, src any, aliases map[string]bool) []string {
	t.Helper()
	f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	pkgs := storeImportNames(f)
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return true
		}
		for _, fl := range st.Fields.List {
			if len(fl.Names) != 0 {
				continue
			}
			typ := fl.Type
			if s, ok := typ.(*ast.StarExpr); ok {
				typ = s.X
			}
			switch x := typ.(type) {
			case *ast.IndexExpr:
				typ = x.X
			case *ast.IndexListExpr:
				typ = x.X
			}
			switch x := typ.(type) {
			case *ast.SelectorExpr:
				if id, ok := x.X.(*ast.Ident); ok && pkgs[id.Name] {
					out = append(out, ts.Name.Name+"|"+id.Name+"."+x.Sel.Name)
				}
			case *ast.Ident:
				if aliases[x.Name] {
					out = append(out, ts.Name.Name+"|"+x.Name)
				}
			}
		}
		return true
	})
	return out
}

// storeImportNames maps the local names a file gives to imports under
// internal/engine/store/ (the alias if any, else the last path element).
func storeImportNames(f *ast.File) map[string]bool {
	m := map[string]bool{}
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		if !strings.HasPrefix(p, storePkgPrefix) {
			continue
		}
		name := p[strings.LastIndex(p, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		m[name] = true
	}
	return m
}

// storeAliasNames returns the type names a file declares as an alias of a
// store-package type.
func storeAliasNames(f *ast.File) []string {
	pkgs := storeImportNames(f)
	var out []string
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, sp := range gd.Specs {
			ts := sp.(*ast.TypeSpec)
			if !ts.Assign.IsValid() {
				continue
			}
			t := ts.Type
			if s, ok := t.(*ast.StarExpr); ok {
				t = s.X
			}
			if se, ok := t.(*ast.SelectorExpr); ok {
				if id, ok := se.X.(*ast.Ident); ok && pkgs[id.Name] {
					out = append(out, ts.Name.Name)
				}
			}
		}
	}
	return out
}

// TestStoreTypes_DoNotEmbedStorePackageTypes (guard A, the embed half): no
// struct in a non-test file under internal/engine/store/** embeds a type declared in
// another store package, directly or through a same-package alias, unless it
// is in embedAllowed with a reason. The facts, merge and remotesync handles
// are named fields (fr, mr, rr) on Service, and remotesync.Repo holds its
// merge handle as the named field mr, so which child a call reaches reads at
// the call site and a method added to a child does not appear on the parent.
//
// Non-vacuity: at least minFiles store files are parsed, at least one alias
// declaration is inspected by the self-test, and every embedAllowed entry must
// match a live embed.
func TestStoreTypes_DoNotEmbedStorePackageTypes(t *testing.T) {
	const minFiles = 150
	root := filepath.Join("..", "..", "internal", "engine", "store")
	fset := token.NewFileSet()
	type pkgFiles struct {
		files   []string
		aliases map[string]bool
	}
	pkgs := map[string]*pkgFiles{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		dir := filepath.Dir(path)
		pf := pkgs[dir]
		if pf == nil {
			pf = &pkgFiles{aliases: map[string]bool{}}
			pkgs[dir] = pf
		}
		pf.files = append(pf.files, path)
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution|parser.ParseComments)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		for _, a := range storeAliasNames(f) {
			pf.aliases[a] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var offenders []string
	used := map[string]bool{}
	parsed := 0
	for _, pf := range pkgs {
		sort.Strings(pf.files)
		for _, path := range pf.files {
			parsed++
			rel, _ := filepath.Rel(filepath.Join("..", ".."), path)
			rel = filepath.ToSlash(rel)
			for _, u := range embedUses(t, fset, path, nil, pf.aliases) {
				key := rel + "|" + u
				if _, ok := embedAllowed[key]; ok {
					used[key] = true
					continue
				}
				offenders = append(offenders, key)
			}
		}
	}
	if parsed < minFiles {
		t.Fatalf("parsed only %d non-test files under internal/engine/store, want at least %d: the walk no longer reaches the store, so this guard checks nothing", parsed, minFiles)
	}
	for k := range embedAllowed {
		if !used[k] {
			t.Errorf("embedAllowed entry %q matches no embed: it is stale, delete it", k)
		}
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("a struct under internal/engine/store embeds a type from another store package, which promotes every exported method of it. Use a named field, or add an embedAllowed entry with a reason:\n  %s", strings.Join(offenders, "\n  "))
	}
}

// TestStoreTypes_EmbedGuardDiscriminates runs the detector on inline source:
// the shapes it must flag and the shapes it must leave alone.
func TestStoreTypes_EmbedGuardDiscriminates(t *testing.T) {
	const imp = "package p\nimport (\n\t\"knomit/internal/engine/store/facts\"\n\tmg \"knomit/internal/engine/store/merge\"\n\t\"sync\"\n)\n"
	aliasSrc := imp + "type factsRepo = facts.Repo\n"
	af, err := parser.ParseFile(token.NewFileSet(), "a.go", aliasSrc, 0)
	if err != nil {
		t.Fatal(err)
	}
	aliases := map[string]bool{}
	for _, a := range storeAliasNames(af) {
		aliases[a] = true
	}
	if !aliases["factsRepo"] {
		t.Fatalf("alias detector did not see `type factsRepo = facts.Repo`: %v", aliases)
	}
	flagged := map[string]string{
		"pointer embed":         imp + "type S struct{ *facts.Repo }\n",
		"value embed":           imp + "type S struct{ facts.Repo }\n",
		"aliased import embed":  imp + "type S struct{ *mg.Repo }\n",
		"embed via local alias": imp + "type S struct{ *factsRepo }\n",
		"embed beside named":    imp + "type S struct{ x int; *facts.Index }\n",
	}
	for name, s := range flagged {
		if got := embedUses(t, token.NewFileSet(), name+".go", s, aliases); len(got) != 1 {
			t.Errorf("%s: want exactly 1 hit, got %v", name, got)
		}
	}
	clean := map[string]string{
		"named field":        imp + "type S struct{ fr *facts.Repo }\n",
		"stdlib embed":       imp + "type S struct{ sync.Mutex }\n",
		"own-package embed":  imp + "type S struct{ *Other }\n",
		"embed in func body": imp + "func f() { var _ struct{ fr *facts.Repo } }\n",
	}
	for name, s := range clean {
		if got := embedUses(t, token.NewFileSet(), name+".go", s, aliases); len(got) != 0 {
			t.Errorf("%s: wrongly flagged: %v", name, got)
		}
	}
}
