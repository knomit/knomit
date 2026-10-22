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

const storeImport = "knomit/internal/engine/store"

// storeChildPath returns the part of an import path below knomit/internal/engine/store/
// ("" when the path is not a store child).
func storeChildPath(p string) string {
	if !strings.HasPrefix(p, storePkgPrefix) {
		return ""
	}
	return strings.TrimPrefix(p, storePkgPrefix)
}

// storeChildImportable reports whether a package outside internal/engine/store may
// import the child: the Go internal rule bars anything below
// internal/engine/store/internal/.
func storeChildImportable(p string) bool {
	rest := storeChildPath(p)
	return rest != "" && !strings.HasPrefix(rest, "internal/") && !strings.Contains(rest, "/internal/")
}

// storeImports maps the local name of every import of a store child in f to
// its import path.
func storeImports(f *ast.File) map[string]string {
	m := map[string]string{}
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		if storeChildPath(p) == "" {
			continue
		}
		name := p[strings.LastIndex(p, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		m[name] = p
	}
	return m
}

// reexport is one name package store declares only to pass a child's name on.
type reexport struct {
	Name string // the name declared in package store
	Pkg  string // import path of the child it forwards to
	Kind string // "type alias", "const", "var" or "func"
	Line int
}

// storeReexports returns every re-export one parent file declares:
//
//	type X = p.Y            const/var X = p.Y
//	func X(a, b) … { [return] p.F(a, b) }   (one statement, args are the params in order)
//
// where p is an import of a store child. A function that does anything else on
// its way through (validation, ordering, a different argument list) is a
// function that does work, not a forward.
func storeReexports(t *testing.T, fset *token.FileSet, name string, src any) []reexport {
	t.Helper()
	f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	imps := storeImports(f)
	pkgOf := func(e ast.Expr) (string, bool) {
		sel, ok := e.(*ast.SelectorExpr)
		if !ok {
			return "", false
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok {
			return "", false
		}
		p, ok := imps[id.Name]
		return p, ok
	}
	line := func(p token.Pos) int { return fset.Position(p).Line }
	var out []reexport
	for _, d := range f.Decls {
		switch x := d.(type) {
		case *ast.GenDecl:
			for _, sp := range x.Specs {
				switch s := sp.(type) {
				case *ast.TypeSpec:
					if s.Assign.IsValid() {
						if p, ok := pkgOf(s.Type); ok {
							out = append(out, reexport{s.Name.Name, p, "type alias", line(s.Pos())})
						}
					}
				case *ast.ValueSpec:
					kind := "var"
					if x.Tok == token.CONST {
						kind = "const"
					}
					for i, v := range s.Values {
						if i >= len(s.Names) {
							break
						}
						if p, ok := pkgOf(v); ok {
							out = append(out, reexport{s.Names[i].Name, p, kind, line(s.Names[i].Pos())})
						}
					}
				}
			}
		case *ast.FuncDecl:
			if x.Recv != nil || x.Body == nil || len(x.Body.List) != 1 {
				continue
			}
			var call *ast.CallExpr
			switch st := x.Body.List[0].(type) {
			case *ast.ExprStmt:
				call, _ = st.X.(*ast.CallExpr)
			case *ast.ReturnStmt:
				if len(st.Results) == 1 {
					call, _ = st.Results[0].(*ast.CallExpr)
				}
			}
			if call == nil {
				continue
			}
			p, ok := pkgOf(call.Fun)
			if !ok {
				continue
			}
			var params []string
			for _, fl := range x.Type.Params.List {
				for _, nm := range fl.Names {
					params = append(params, nm.Name)
				}
			}
			if len(call.Args) != len(params) {
				continue
			}
			same := true
			for i, a := range call.Args {
				id, ok := a.(*ast.Ident)
				if !ok || id.Name != params[i] {
					same = false
				}
			}
			if same {
				out = append(out, reexport{x.Name.Name, p, "func", line(x.Pos())})
			}
		}
	}
	return out
}

// TestStoreParent_ReexportsOnlyNonImportableWithOutsideCaller (guard A1): the
// non-test files of package store (depth 1) declare no re-export of an
// IMPORTABLE child's name (callers import the owning package), and a
// re-export of a non-importable one (internal/engine/store/internal/...) exists only
// when some file outside internal/engine/store/** (tests included) selects store.X.
// An unexported re-export has no outside caller by definition, so it fails
// too: it is a private forward that exists so the parent's own code can avoid
// naming the child.
//
// Non-vacuity: the outside walk parses at least minOutside files and sees at
// least minDistinct distinct store.X selectors, including the live names
// Service, Open and SyncDSNParam (the design's floor of 100 distinct names is
// gone: after E3-E7b the parent's outside surface is a handful of names, the
// rest moved to model and the children), and the self-test runs the detector
// on inline source with one of each form.
func TestStoreParent_ReexportsOnlyNonImportableWithOutsideCaller(t *testing.T) {
	const (
		minOutside  = 300
		minDistinct = 5
		minParent   = 10
	)
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()

	var parent []string
	entries, err := filepath.Glob(filepath.Join(root, "internal", "engine", "store", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e, "_test.go") {
			parent = append(parent, e)
		}
	}
	if len(parent) < minParent {
		t.Fatalf("found only %d non-test files in internal/engine/store, want at least %d: the glob matches nothing", len(parent), minParent)
	}

	outsideSel := map[string]bool{}
	outside := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			switch rel {
			case ".git", "web", "dist", ".claude", "third_party", "internal/engine/store":
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
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		outside++
		local := ""
		for _, imp := range f.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == storeImport {
				local = "store"
				if imp.Name != nil {
					local = imp.Name.Name
				}
			}
		}
		if local == "" {
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == local {
					outsideSel[sel.Sel.Name] = true
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if outside < minOutside {
		t.Fatalf("parsed only %d files outside internal/engine/store, want at least %d: the walk no longer reaches the module", outside, minOutside)
	}
	if len(outsideSel) < minDistinct {
		t.Fatalf("saw only %d distinct store.X selectors outside internal/engine/store, want at least %d: the import-alias resolution matches nothing", len(outsideSel), minDistinct)
	}

	for _, want := range []string{"Service", "Open", "SyncDSNParam"} {
		if !outsideSel[want] {
			t.Fatalf("no file outside internal/engine/store selects store.%s: the import-alias resolution matches nothing, or the name moved", want)
		}
	}
	var offenders []string
	for _, path := range parent {
		for _, r := range storeReexports(t, fset, path, nil) {
			where := filepath.Base(path) + " line " + strconv.Itoa(r.Line) + ": " + r.Kind + " " + r.Name + " -> " + r.Pkg
			switch {
			case storeChildImportable(r.Pkg):
				offenders = append(offenders, where+" (the child is importable: callers import it)")
			case !outsideSel[r.Name]:
				offenders = append(offenders, where+" (non-importable child, but nothing outside internal/engine/store selects store."+r.Name+")")
			}
		}
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("package store re-exports a child name it need not: delete it and import the owner, or (non-importable child only) keep it because a caller outside the store needs it:\n  %s", strings.Join(offenders, "\n  "))
	}
}

// TestStoreParent_ReexportGuardDiscriminates runs the detector on inline
// source: one of each re-export form is found, and a function that does work on
// its way through, a method, a same-package alias and an alias of a stdlib
// type are not.
func TestStoreParent_ReexportGuardDiscriminates(t *testing.T) {
	const imp = "package store\nimport (\n\t\"knomit/internal/engine/store/merge\"\n\tm \"knomit/internal/engine/store/merge\"\n\t\"knomit/internal/engine/store/internal/vcs\"\n\t\"time\"\n)\n"
	flagged := map[string]struct {
		src  string
		name string
		pkg  string
	}{
		"type alias":         {imp + "type Mode = merge.Mode\n", "Mode", "knomit/internal/engine/store/merge"},
		"aliased import":     {imp + "type Mode = m.Mode\n", "Mode", "knomit/internal/engine/store/merge"},
		"var forward":        {imp + "var ErrZ = vcs.ErrBranchNotFound\n", "ErrZ", "knomit/internal/engine/store/internal/vcs"},
		"const forward":      {imp + "const K = merge.Kind\n", "K", "knomit/internal/engine/store/merge"},
		"var in group":       {imp + "var (\n\ta = 1\n\tB = vcs.Reg\n)\n", "B", "knomit/internal/engine/store/internal/vcs"},
		"func forward":       {imp + "func F(a int, b string) error { return vcs.F(a, b) }\n", "F", "knomit/internal/engine/store/internal/vcs"},
		"func forward, stmt": {imp + "func G(a int) { vcs.G(a) }\n", "G", "knomit/internal/engine/store/internal/vcs"},
		"func forward, none": {imp + "func H() string { return vcs.H() }\n", "H", "knomit/internal/engine/store/internal/vcs"},
	}
	for name, c := range flagged {
		got := storeReexports(t, token.NewFileSet(), name+".go", c.src)
		if len(got) != 1 || got[0].Name != c.name || got[0].Pkg != c.pkg {
			t.Errorf("%s: got %+v, want one %s -> %s", name, got, c.name, c.pkg)
		}
	}
	clean := map[string]string{
		"adds behaviour":       imp + "func F(a int) error { if a < 0 { return nil }; return vcs.F(a) }\n",
		"different arguments":  imp + "func F(a int) error { return vcs.F(a, 1) }\n",
		"reordered arguments":  imp + "func F(a, b int) error { return vcs.F(b, a) }\n",
		"two statements":       imp + "func F(a int) error { _ = a; return vcs.F(a) }\n",
		"method":               imp + "type T struct{}\nfunc (T) F(a int) error { return vcs.F(a) }\n",
		"stdlib alias":         imp + "type D = time.Duration\n",
		"local alias":          imp + "type A int\ntype B = A\n",
		"value from a call":    imp + "var X = vcs.New()\n",
		"non-store forward":    "package store\nimport \"strings\"\nfunc F(a string) string { return strings.ToUpper(a) }\n",
		"constant of a stdlib": imp + "const D = time.Second\n",
	}
	for name, s := range clean {
		if got := storeReexports(t, token.NewFileSet(), name+".go", s); len(got) != 0 {
			t.Errorf("%s: wrongly flagged: %+v", name, got)
		}
	}
	for p, want := range map[string]bool{
		"knomit/internal/engine/store/merge":             true,
		"knomit/internal/engine/store/model":             true,
		"knomit/internal/engine/store/remotesync":        true,
		"knomit/internal/engine/store/internal/vcs":      false,
		"knomit/internal/engine/store/internal/sqlite":   false,
		"knomit/internal/engine/store/internal/sessions": false,
		"knomit/internal/engine/embeddings":              false,
		"knomit/internal/engine/store":                   false,
	} {
		if storeChildImportable(p) != want {
			t.Errorf("storeChildImportable(%q) = %v, want %v", p, !want, want)
		}
	}
}

// serviceChildFields returns, for the Service struct declared in f, the
// field name -> import path of every field whose declared type is a (pointer
// to a) type of an IMPORTABLE store child.
func serviceChildFields(f *ast.File) map[string]string {
	imps := storeImports(f)
	out := map[string]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "Service" {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return true
		}
		for _, fl := range st.Fields.List {
			typ := fl.Type
			if s, ok := typ.(*ast.StarExpr); ok {
				typ = s.X
			}
			sel, ok := typ.(*ast.SelectorExpr)
			if !ok {
				continue
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok {
				continue
			}
			if p, ok := imps[id.Name]; ok && storeChildImportable(p) {
				for _, nm := range fl.Names {
					out[nm.Name] = p
				}
			}
		}
		return true
	})
	return out
}

// serviceChildHelpers returns, for the *Service methods f declares, the
// method name -> import path of every one whose first result is a (pointer
// to a) type of an IMPORTABLE store child (Facts() *facts.Index,
// Integrity() *fsck.Checker, ...): a call of one is a handle to that child
// exactly as a field of that type is.
func serviceChildHelpers(f *ast.File) map[string]string {
	imps := storeImports(f)
	out := map[string]string{}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Recv == nil || len(fd.Recv.List) != 1 || fd.Type.Results == nil || len(fd.Type.Results.List) == 0 {
			continue
		}
		rt := fd.Recv.List[0].Type
		if s, ok := rt.(*ast.StarExpr); ok {
			rt = s.X
		}
		if id, ok := rt.(*ast.Ident); !ok || id.Name != "Service" {
			continue
		}
		typ := fd.Type.Results.List[0].Type
		if s, ok := typ.(*ast.StarExpr); ok {
			typ = s.X
		}
		sel, ok := typ.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		if id, ok := sel.X.(*ast.Ident); ok {
			if p, ok := imps[id.Name]; ok && storeChildImportable(p) {
				out[fd.Name.Name] = p
			}
		}
	}
	return out
}

// serviceForwards returns the exported methods of *Service in f whose body
// only hands a call to an importable child: zero or more leading
// `x := <handle>` statements followed by ONE statement that is a call (as an
// expression statement or the sole result of a return) of <handle>.M(...) or
// <pkg>.F(...). A <handle> is s.<field> where <field> is in fields (a field
// whose declared type is an importable child's), a call s.<helper>() where
// <helper> is in helpers (a Service method returning an importable child's
// type), or a local bound by a leading statement to one of those. <pkg> is an
// import of an importable store child. It also returns how many exported
// *Service methods f declares, for the non-vacuity count.
func serviceForwards(t *testing.T, fset *token.FileSet, name string, src any, fields, helpers map[string]string) (hits []string, methods int) {
	t.Helper()
	f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	imps := storeImports(f)
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Recv == nil || len(fd.Recv.List) != 1 || !fd.Name.IsExported() || fd.Body == nil {
			continue
		}
		rt := fd.Recv.List[0].Type
		if s, ok := rt.(*ast.StarExpr); ok {
			rt = s.X
		}
		if id, ok := rt.(*ast.Ident); !ok || id.Name != "Service" {
			continue
		}
		methods++
		recv := ""
		if len(fd.Recv.List[0].Names) == 1 {
			recv = fd.Recv.List[0].Names[0].Name
		}
		// handle describes an expression that is a handle to an importable
		// child, or "" when it is not one.
		type target struct{ label, pkg string }
		locals := map[string]target{}
		var handle func(e ast.Expr) (target, bool)
		handle = func(e ast.Expr) (target, bool) {
			switch x := e.(type) {
			case *ast.ParenExpr:
				return handle(x.X)
			case *ast.Ident:
				tg, ok := locals[x.Name]
				return tg, ok
			case *ast.SelectorExpr: // s.<field>
				if id, ok := x.X.(*ast.Ident); ok && id.Name == recv {
					if p, ok := fields[x.Sel.Name]; ok {
						return target{"s." + x.Sel.Name, p}, true
					}
				}
			case *ast.CallExpr: // s.<helper>()
				if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == recv {
						if p, ok := helpers[sel.Sel.Name]; ok {
							return target{"s." + sel.Sel.Name + "()", p}, true
						}
					}
				}
			}
			return target{}, false
		}
		stmts := fd.Body.List
		if len(stmts) == 0 {
			continue
		}
		// `a, b := <call>; return a, b` returns exactly what the call
		// returned: it is the call as a return statement.
		if n := len(stmts); n >= 2 {
			if ret, ok := stmts[n-1].(*ast.ReturnStmt); ok {
				if as, ok := stmts[n-2].(*ast.AssignStmt); ok && as.Tok == token.DEFINE && len(as.Rhs) == 1 && len(as.Lhs) == len(ret.Results) {
					if c, ok := as.Rhs[0].(*ast.CallExpr); ok {
						same := true
						for i := range as.Lhs {
							l, lok := as.Lhs[i].(*ast.Ident)
							r, rok := ret.Results[i].(*ast.Ident)
							if !lok || !rok || l.Name != r.Name {
								same = false
							}
						}
						if same {
							stmts = append(append([]ast.Stmt{}, stmts[:n-2]...), &ast.ReturnStmt{Results: []ast.Expr{c}})
						}
					}
				}
			}
		}
		bound := true
		for _, st := range stmts[:len(stmts)-1] {
			var lhs, rhs ast.Expr
			switch d := st.(type) {
			case *ast.AssignStmt: // x := <handle>
				if d.Tok == token.DEFINE && len(d.Lhs) == 1 && len(d.Rhs) == 1 {
					lhs, rhs = d.Lhs[0], d.Rhs[0]
				}
			case *ast.DeclStmt: // var x = <handle>
				if gd, ok := d.Decl.(*ast.GenDecl); ok && gd.Tok == token.VAR && len(gd.Specs) == 1 {
					if vs, ok := gd.Specs[0].(*ast.ValueSpec); ok && len(vs.Names) == 1 && len(vs.Values) == 1 {
						lhs, rhs = vs.Names[0], vs.Values[0]
					}
				}
			}
			id, ok := lhs.(*ast.Ident)
			if !ok {
				bound = false
				break
			}
			h, ok := handle(rhs)
			if !ok {
				bound = false
				break
			}
			locals[id.Name] = h
		}
		if !bound {
			continue
		}
		var call *ast.CallExpr
		switch st := stmts[len(stmts)-1].(type) {
		case *ast.ExprStmt:
			call, _ = st.X.(*ast.CallExpr)
		case *ast.ReturnStmt:
			if len(st.Results) == 1 {
				call, _ = st.Results[0].(*ast.CallExpr)
			}
		}
		if call == nil {
			continue
		}
		fun, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		if h, ok := handle(fun.X); ok {
			hits = append(hits, fd.Name.Name+" -> "+h.label+"."+fun.Sel.Name+" ("+h.pkg+")")
			continue
		}
		if id, ok := fun.X.(*ast.Ident); ok {
			if _, local := locals[id.Name]; !local {
				if p, ok := imps[id.Name]; ok && storeChildImportable(p) {
					hits = append(hits, fd.Name.Name+" -> "+id.Name+"."+fun.Sel.Name+" ("+p+")")
				}
			}
		}
	}
	return hits, methods
}

// injectsStoreState is every exported Service method that is one call into an
// importable child and is nevertheless the store's own API, keyed by method
// name with the reason. A forward earns an entry only by passing the child
// something a caller outside the store cannot obtain (the storer, the fact
// root, the Service's own wiring). A stale entry fails.
var injectsStoreState = map[string]string{
	"Integrity":    "fsck.NewChecker is given the Service's repository handler (internal/vcs) and indices: callers outside the store cannot build the checker",
	"FleetAt":      "fleetrec.At is given s.rh.Storer() and the fact root: the storer cannot be obtained outside the store",
	"FileAt":       "fleetrec.FileAt is given s.rh.Storer(), which callers outside the store cannot obtain",
	"FleetKeysOf":  "fleetrec reader over s.rh.Storer()",
	"FleetMainLog": "fleetrec reader over s.rh.Storer()",
	"FleetSince":   "fleetrec reader over s.rh.Storer()",
}

// TestStoreService_NoForwardIntoImportableChild (guard A2): no exported
// *Service method only hands a call to an importable child. The handle may be
// a Service field whose type is the child's (s.fi.M, s.mr.M, s.rr.M, ...), a
// call of a Service method returning the child's type (s.helper().M), or a
// local bound to either (m := s.mr; return m.M()); the call may also be
// package-qualified (fleetrec.F). Callers get the concrete handle
// (Service.Facts, Merge, Remote, ...) and import the child. The vcs-backed
// operations (Service.OntologyAt, HasSharedHistory, DiffFacts, ...) call a
// non-importable child and are the ruled exception; they are not matched
// here. A match must be in injectsStoreState with a reason. A method that does
// work between binding the handle and calling it (Handler caches a gitserve
// handler behind a sync.Once) is not a forward and needs no entry. Also
// matched: `var m = s.mr`, a parenthesised handle `(s.mr).M()`, and
// `a, b := s.mr.M(); return a, b`.
//
// Known gaps, deliberately not closed: method values (f := s.mr.M; return
// f()), named results assigned from the call, helpers returning a struct that
// holds the child, package-level helper functions (func g(s *Service) ...) and
// closures. Each needs data-flow the shape check does not do.
//
// Non-vacuity: at least minMethods exported Service methods are inspected,
// every injectsStoreState name is still found as a flagged forward, and the
// self-test runs the detector on inline source.
func TestStoreService_NoForwardIntoImportableChild(t *testing.T) {
	const minMethods = 30
	dir := filepath.Join("..", "..", "internal", "engine", "store")
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	fields := map[string]string{}
	helpers := map[string]string{}
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("parse %s: %v", p, perr)
		}
		for k, v := range serviceChildFields(f) {
			fields[k] = v
		}
		for k, v := range serviceChildHelpers(f) {
			helpers[k] = v
		}
	}
	if len(fields) < 5 {
		t.Fatalf("found only %d Service fields typed by an importable child (%v): the struct moved or its types changed", len(fields), fields)
	}
	methods := 0
	seen := map[string]bool{}
	var offenders []string
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		hits, n := serviceForwards(t, fset, p, nil, fields, helpers)
		methods += n
		for _, h := range hits {
			name, _, _ := strings.Cut(h, " ")
			seen[name] = true
			if _, ok := injectsStoreState[name]; !ok {
				offenders = append(offenders, filepath.Base(p)+": "+h)
			}
		}
	}
	if methods < minMethods {
		t.Fatalf("inspected only %d exported Service methods, want at least %d: the scan matches nothing", methods, minMethods)
	}
	for name := range injectsStoreState {
		if !seen[name] {
			t.Errorf("injectsStoreState lists %s but it is no longer a one-call forward into an importable child: delete the entry", name)
		}
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("exported Service methods that only forward into an importable child: hand out the concrete handle and let the caller import the child, or (if the call injects store state a caller cannot obtain) add an injectsStoreState entry with the reason:\n  %s", strings.Join(offenders, "\n  "))
	}
}

// TestStoreService_ForwardGuardDiscriminates runs the detector on inline
// source.
func TestStoreService_ForwardGuardDiscriminates(t *testing.T) {
	const imp = "package store\nimport (\n\t\"knomit/internal/engine/store/facts\"\n\t\"knomit/internal/engine/store/fleetrec\"\n\t\"knomit/internal/engine/store/internal/vcs\"\n)\ntype Service struct{ fr *facts.Repo; rh *vcs.Repo; n int }\n"
	fields := map[string]string{"fr": "knomit/internal/engine/store/facts"}
	helpers := map[string]string{"checker": "knomit/internal/engine/store/fsck", "Facts": "knomit/internal/engine/store/facts"}
	flagged := map[string]string{
		"field forward":        imp + "func (s *Service) PushedBranches(a int) error { return s.fr.PushedBranches(a) }\n",
		"field forward, stmt":  imp + "func (s *Service) Poke() { s.fr.Poke() }\n",
		"field forward, value": imp + "func (s Service) Poke() int { return s.fr.Poke() }\n",
		"package-qualified":    imp + "func (s *Service) FleetThing() (any, error) { return fleetrec.At(s.rh, \"main\", \"\") }\n",
		"call on a call":       imp + "func (s *Service) Verify(a int) error { return s.checker().Verify(a) }\n",
		"call on a call, stmt": imp + "func (s *Service) Poke() { s.checker().Poke() }\n",
		"call on accessor":     imp + "func (s *Service) Ask() int { return s.Facts().Ask() }\n",
		"local copy":           imp + "func (s *Service) F() error { m := s.fr; return m.F() }\n",
		"local copy of call":   imp + "func (s *Service) F() error { c := s.checker(); return c.F() }\n",
		"local copy, chained":  imp + "func (s *Service) F() error { m := s.fr; n := m; return n.F() }\n",
		"var decl copy":        imp + "func (s *Service) F() error { var m = s.fr; return m.F() }\n",
		"parenthesised":        imp + "func (s *Service) F() error { return (s.fr).F() }\n",
		"bind, return same":    imp + "func (s *Service) F() (int, error) { n, err := s.fr.F(); return n, err }\n",
		"local copy, stmt":     imp + "func (s *Service) F() { m := s.fr; m.F() }\n",
	}
	for name, src := range flagged {
		hits, n := serviceForwards(t, token.NewFileSet(), name+".go", src, fields, helpers)
		if len(hits) != 1 || n != 1 {
			t.Errorf("%s: hits %v, methods %d, want 1 hit and 1 method", name, hits, n)
		}
	}
	clean := map[string]string{
		"vcs-backed forward":      imp + "func (s *Service) OntologyAt(a string) error { return s.rh.OntologyAt(a) }\n",
		"handle accessor":         imp + "func (s *Service) FactsRepo() *facts.Repo { return s.fr }\n",
		"two statements":          imp + "func (s *Service) F() error { _ = s.n; return s.fr.F() }\n",
		"bind, return other":      imp + "func (s *Service) F() (int, error) { n, err := s.fr.F(); return n + 1, err }\n",
		"bind, reorder":           imp + "func (s *Service) F() (int, error) { n, err := s.fr.F(); return err, n }\n",
		"unexported method":       imp + "func (s *Service) f() error { return s.fr.F() }\n",
		"non-Service method":      imp + "type Other struct{ fr *facts.Repo }\nfunc (o *Other) F() error { return o.fr.F() }\n",
		"non-child field":         imp + "func (s *Service) F() int { return s.n.Abs() }\n",
		"plain function":          imp + "func F(s *Service) error { return s.fr.F() }\n",
		"call on non-child call":  imp + "func (s *Service) F() int { return s.other().F() }\n",
		"local of non-child":      imp + "func (s *Service) F() int { m := s.n; return m.Abs() }\n",
		"local of non-child call": imp + "func (s *Service) F() int { m := s.other(); return m.Abs() }\n",
		"local, then work":        imp + "func (s *Service) F() error { m := s.fr; _ = s.n; return m.F() }\n",
	}
	for name, src := range clean {
		hits, _ := serviceForwards(t, token.NewFileSet(), name+".go", src, fields, helpers)
		if len(hits) != 0 {
			t.Errorf("%s: wrongly flagged: %v", name, hits)
		}
	}
	f, err := parser.ParseFile(token.NewFileSet(), "s.go", imp, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := serviceChildFields(f); len(got) != 1 || got["fr"] == "" {
		t.Errorf("serviceChildFields = %v, want only fr (rh is a non-importable child, n is a builtin)", got)
	}
}
