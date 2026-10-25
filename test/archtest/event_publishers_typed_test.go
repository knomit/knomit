package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	gopackages "golang.org/x/tools/go/packages"
)

// The type-aware half of the publisher guard (the name-based half is in
// event_publishers_test.go). A publisher can leave the repos owner without ever
// being NAMED in a signature:
//
//	func (ri *RepoInstance) Publishing() any { return ri.pub }
//	func (ri *RepoInstance) Announcer() func(events.IndexEvent) { return ri.pub.BroadcastIndex }
//
// so, with go/types, in every non-test file of a package that imports events
// (other than events itself) the guard flags
//
//   - (a) a return statement of an EXPORTED func or method (closures inside it
//     included) whose result expression has a publisher type, is a method value
//     bound to a publisher, is a func literal that uses a publisher-typed
//     expression, or is a composite literal with a publisher-typed element,
//     whatever the declared result type is; and
//   - (b) an assignment, keyed composite-literal field or var initializer that
//     puts a publisher-typed expression or a bound method value into an EXPORTED
//     struct field or package-level var (so `Pub any` filled with ri.pub is
//     caught).
//
// Not followed (each needs data flow the check does not do): a publisher passed
// through a local variable or a helper's result before it is returned, named
// results assigned and bare-returned, publishers sent on channels or stored in
// maps/slices and returned, and reflection. Files behind build tags other than
// the default are not type-checked here (the name-based half still scans them).
func isPublisherType(t types.Type) bool {
	for {
		p, ok := t.(*types.Pointer)
		if !ok {
			break
		}
		t = p.Elem()
	}
	n, ok := t.(*types.Named)
	if !ok || n.Obj().Pkg() == nil {
		return false
	}
	return n.Obj().Pkg().Path() == eventsPkgPath && publisherTypes[n.Obj().Name()]
}

// publisherValue reports whether e evaluates to a publisher or to a method
// value bound to one.
func publisherValue(info *types.Info, e ast.Expr) bool {
	e = unparen(e)
	if tv, ok := info.Types[e]; ok && tv.Type != nil && isPublisherType(tv.Type) {
		return true
	}
	if sel, ok := e.(*ast.SelectorExpr); ok {
		if s := info.Selections[sel]; s != nil && s.Kind() == types.MethodVal && isPublisherType(s.Recv()) {
			return true
		}
	}
	return false
}

func unparen(e ast.Expr) ast.Expr {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			return e
		}
		e = p.X
	}
}

// leaksPublisher reports whether a returned/assigned expression carries a
// publisher out: itself, a method value, a closure over one, or a composite
// literal holding one.
func leaksPublisher(info *types.Info, e ast.Expr) bool {
	e = unparen(e)
	if publisherValue(info, e) {
		return true
	}
	switch e := e.(type) {
	case *ast.FuncLit:
		hit := false
		ast.Inspect(e.Body, func(n ast.Node) bool {
			if x, ok := n.(ast.Expr); ok && publisherValue(info, x) {
				hit = true
			}
			return !hit
		})
		return hit
	case *ast.CompositeLit:
		for _, el := range e.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				// A struct's unexported field is the owner's own state, not a way out.
				if k, ok := kv.Key.(*ast.Ident); ok {
					if v, _ := info.ObjectOf(k).(*types.Var); v != nil && v.IsField() && !v.Exported() {
						continue
					}
				}
				el = kv.Value
			}
			if leaksPublisher(info, el) {
				return true
			}
		}
	case *ast.UnaryExpr: // &T{...}
		return leaksPublisher(info, e.X)
	}
	return false
}

// exportedTarget reports whether lhs names an exported struct field or an
// exported package-level var.
func exportedTarget(info *types.Info, lhs ast.Expr) bool {
	var id *ast.Ident
	switch l := unparen(lhs).(type) {
	case *ast.SelectorExpr:
		id = l.Sel
	case *ast.Ident:
		id = l
	default:
		return false
	}
	v, _ := info.ObjectOf(id).(*types.Var)
	if v == nil || !v.Exported() {
		return false
	}
	return v.IsField() || (v.Pkg() != nil && v.Parent() == v.Pkg().Scope())
}

// typedPublisherLeaks runs rules (a) and (b) over one type-checked file.
func typedPublisherLeaks(fset *token.FileSet, f *ast.File, info *types.Info) []string {
	var out []string
	at := func(p token.Pos, msg string) { out = append(out, fset.Position(p).String()+": "+msg) }
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Body != nil && d.Name.IsExported() {
				ast.Inspect(d.Body, func(n ast.Node) bool {
					if r, ok := n.(*ast.ReturnStmt); ok {
						for _, e := range r.Results {
							if leaksPublisher(info, e) {
								at(e.Pos(), "exported "+d.Name.Name+" returns a publisher (or a method value or closure over one)")
							}
						}
					}
					return true
				})
			}
		case *ast.GenDecl:
			for _, sp := range d.Specs {
				if vs, ok := sp.(*ast.ValueSpec); ok {
					for i, v := range vs.Values {
						if i < len(vs.Names) && vs.Names[i].IsExported() && leaksPublisher(info, v) {
							at(v.Pos(), "exported var "+vs.Names[i].Name+" holds a publisher")
						}
					}
				}
			}
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			if len(n.Lhs) == len(n.Rhs) {
				for i := range n.Lhs {
					if exportedTarget(info, n.Lhs[i]) && leaksPublisher(info, n.Rhs[i]) {
						at(n.Rhs[i].Pos(), "a publisher is assigned to an exported field or var")
					}
				}
			}
		case *ast.CompositeLit:
			for _, el := range n.Elts {
				if kv, ok := el.(*ast.KeyValueExpr); ok {
					if k, ok := kv.Key.(*ast.Ident); ok && exportedTarget(info, k) && leaksPublisher(info, kv.Value) {
						at(kv.Value.Pos(), "a publisher is stored in an exported field of a composite literal")
					}
				}
			}
		}
		return true
	})
	return out
}

// typedLeaksInPackages type-checks the packages that import events and returns
// every leak. Imports are read from export data (no NeedDeps), so only the
// roots are checked from source.
func typedLeaksInPackages(t *testing.T, paths []string) (leaks []string, files int) {
	t.Helper()
	cfg := &gopackages.Config{
		Mode: gopackages.NeedName | gopackages.NeedFiles | gopackages.NeedCompiledGoFiles | gopackages.NeedImports |
			gopackages.NeedTypes | gopackages.NeedSyntax | gopackages.NeedTypesInfo,
	}
	pkgs, err := gopackages.Load(cfg, paths...)
	if err != nil {
		t.Fatalf("gopackages.Load: %v", err)
	}
	for _, p := range pkgs {
		if len(p.Errors) > 0 {
			t.Fatalf("type-checking %s: %v", p.PkgPath, p.Errors)
		}
		for i, f := range p.Syntax {
			name := p.CompiledGoFiles[i]
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			files++
			leaks = append(leaks, typedPublisherLeaks(p.Fset, f, p.TypesInfo)...)
		}
	}
	return leaks, files
}

func TestEventPublishers_NotLeakedTyped(t *testing.T) {
	var paths []string
	fset := token.NewFileSet()
	for _, p := range listModulePackages(t) {
		if p.Path == eventsPkgPath {
			continue
		}
		for _, file := range p.Files {
			f, err := parser.ParseFile(fset, filepath.Join(p.Dir, file), nil, parser.ImportsOnly)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			hit := false
			for _, im := range f.Imports {
				hit = hit || im.Path.Value == strconv.Quote(eventsPkgPath)
			}
			if hit {
				paths = append(paths, p.Path)
				break
			}
		}
	}
	if len(paths) == 0 {
		t.Fatal("no package imports events: the typed scan is vacuous")
	}
	leaks, files := typedLeaksInPackages(t, paths)
	if files == 0 {
		t.Fatal("the typed scan type-checked no file")
	}
	if len(leaks) > 0 {
		sort.Strings(leaks)
		t.Fatalf("a publisher of the repo event hubs leaves its owner without being named in a signature; only the repos owner may hold it, in unexported fields:\n  %s", strings.Join(leaks, "\n  "))
	}
}

// checkInline type-checks src (importing only the stub events package) and
// returns the typed leaks.
func checkInline(t *testing.T, src string) []string {
	t.Helper()
	const stub = `package events
type IndexEvent struct{}
type TaskHub struct{}
type TaskPublisher struct{}
func (*TaskPublisher) BroadcastIndex(IndexEvent) {}
type RepoEventPublisher struct{}
func (*RepoEventPublisher) Publish(any) {}
`
	fset := token.NewFileSet()
	check := func(path, s string, imp types.Importer) (*types.Package, *ast.File, *types.Info) {
		f, err := parser.ParseFile(fset, path+".go", s, 0)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Selections: map[*ast.SelectorExpr]*types.Selection{},
			Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{}}
		pkg, err := (&types.Config{Importer: imp}).Check(path, fset, []*ast.File{f}, info)
		if err != nil {
			t.Fatalf("type-check %s: %v\n%s", path, err, s)
		}
		return pkg, f, info
	}
	ev, _, _ := check(eventsPkgPath, stub, nil)
	imp := importerFunc(func(path string) (*types.Package, error) { return ev, nil })
	_, f, info := check("knomit/internal/corpus/repos", src, imp)
	return typedPublisherLeaks(fset, f, info)
}

type importerFunc func(string) (*types.Package, error)

func (f importerFunc) Import(path string) (*types.Package, error) { return f(path) }

func TestEventPublishers_TypedGuardDiscriminates(t *testing.T) {
	const imp = "package repos\nimport \"knomit/internal/corpus/repos/events\"\ntype RepoInstance struct{ pub *events.TaskPublisher; rp *events.RepoEventPublisher; Any any; Fn func(events.IndexEvent) }\nvar ri = &RepoInstance{}\n"
	flagged := map[string]string{
		"reviewer shape 1: returns the publisher as any":  "func (r *RepoInstance) Publishing() any { return r.pub }\n",
		"reviewer shape 2: returns a bound method value":  "func (r *RepoInstance) Announcer() func(events.IndexEvent) { return r.pub.BroadcastIndex }\n",
		"returns a closure over the publisher":            "func (r *RepoInstance) Announce() func() { return func() { r.pub.BroadcastIndex(events.IndexEvent{}) } }\n",
		"returns a slice holding the publisher":           "func (r *RepoInstance) Both() []any { return []any{r.pub, r.rp} }\n",
		"returns the repo publisher as any, parenthesed":  "func (r *RepoInstance) P() any { return (r.rp) }\n",
		"returns through a nested closure of an exported": "func F(r *RepoInstance) func() any { return func() any { return r.pub } }\n",
		"assigns to an exported any field":                "func (r *RepoInstance) Set() { r.Any = r.pub }\n",
		"assigns a method value to an exported field":     "func (r *RepoInstance) Set() { r.Fn = r.pub.BroadcastIndex }\n",
		"keyed literal into an exported field":            "func New(r *RepoInstance) *RepoInstance { return &RepoInstance{Any: r.pub} }\n",
		"exported package var initialised with it":        "var Leak any = ri.pub\n",
	}
	for name, src := range flagged {
		if got := checkInline(t, imp+src); len(got) == 0 {
			t.Errorf("%s: not flagged", name)
		}
	}
	clean := map[string]string{
		"calls the publisher and returns nothing of it": "func (r *RepoInstance) Do() { r.pub.BroadcastIndex(events.IndexEvent{}) }\n",
		"returns a call's result":                       "func (r *RepoInstance) N() int { r.pub.BroadcastIndex(events.IndexEvent{}); return 1 }\n",
		"unexported method returns it":                  "func (r *RepoInstance) publisher() any { return r.pub }\n",
		"stores it in an unexported field":              "func (r *RepoInstance) Set() { r.pub = nil; r.rp = nil }\n",
		"keyed literal into unexported fields":          "func New(p *events.TaskPublisher) *RepoInstance { return &RepoInstance{pub: p} }\n",
		"exported var of something else":                "var Fine any = 1\n",
	}
	for name, src := range clean {
		if got := checkInline(t, imp+src); len(got) != 0 {
			t.Errorf("%s: flagged %v", name, got)
		}
	}
}
