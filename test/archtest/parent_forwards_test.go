package archtest

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// parentForwardAllowlist is every name a parent package under internal/ may
// declare although it only hands a child's name or call on, keyed by
// "<import path>.<Name>" (methods "<import path>.<Type>.<Method>"), with the
// reason. The convention (no re-export or forwarding layers: callers import the
// owning package) has no exception for convenience; an entry earns its place by
// a reason a caller outside the parent cannot be pointed at the child. A stale
// entry (the name is no longer a forward) fails.
var parentForwardAllowlist = map[string]string{
	"knomit/internal/engine/store.SyncDSNParam": "re-export of the non-importable internal/engine/store/internal/sqlite name: callers outside the store cannot import the owner (Go internal rule), and the store's guard A1 requires and finds an outside caller",
}

// parentFwd is one forward found in a parent package.
type parentFwd struct {
	Name   string // pkgpath-relative name: "X" or "Type.Method"
	Kind   string
	Child  string
	File   string
	Line   int
	Detail string
}

// childPackages returns, for the package parent, the predicate "is this import
// path a child": a path with parent as a strict prefix (children of any depth,
// internal/ ones included).
func childPackages(parent string) func(string) bool {
	return func(p string) bool { return strings.HasPrefix(p, parent+"/") }
}

// parentForwards parses the non-test files of one parent package (files: name
// -> nil to read from disk, or the source text) and returns every forward
// into a child package it declares:
//
//	type X = child.Y                          alias (also *child.Y, child.Y[T])
//	var X = child.Y / const X = child.Y       initializer is only a child name
//	func X(a, b) T { return child.F(a, b) }   exported func: one statement, a
//	                                          child call with the parameters
//	                                          passed through, in order
//	func (r *T) X(...) { return r.field.M() } exported method: one statement, a
//	                                          call on a field of r (or on a
//	                                          method result of r) whose type is
//	                                          a child's, or child.F(params)
//
// The statement may be preceded by `x := <handle>` / `var x = <handle>` locals
// and may be `a, b := call; return a, b`, parenthesised, or a call on a call
// (see methodForwards, the A2 detector this generalises).
//
// What is NOT a forward: a package-qualified call fed anything but the
// function's own parameters. `func (s *S) Integrity() *fsck.Checker { return
// fsck.NewChecker(s.repo) }` builds a child value from private state; it is a
// constructor, not a pass-through.
func parentForwards(t *testing.T, fset *token.FileSet, parent string, files map[string]any) []parentFwd {
	t.Helper()
	isChild := childPackages(parent)
	type parsed struct {
		name string
		f    *ast.File
		imps map[string]string
	}
	var fs []parsed
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		f, err := parser.ParseFile(fset, n, files[n], parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", n, err)
		}
		fs = append(fs, parsed{n, f, childImports(f, isChild)})
	}

	var out []parentFwd
	// (a) and (b): declarations.
	for _, p := range fs {
		if len(p.imps) == 0 {
			continue
		}
		pkgOf := func(e ast.Expr) (string, bool) {
			for {
				switch x := e.(type) {
				case *ast.ParenExpr:
					e = x.X
					continue
				case *ast.StarExpr:
					e = x.X
					continue
				case *ast.IndexExpr:
					e = x.X
					continue
				case *ast.IndexListExpr:
					e = x.X
					continue
				}
				break
			}
			sel, ok := e.(*ast.SelectorExpr)
			if !ok {
				return "", false
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok {
				return "", false
			}
			path, ok := p.imps[id.Name]
			return path, ok
		}
		for _, d := range p.f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, sp := range gd.Specs {
				switch s := sp.(type) {
				case *ast.TypeSpec:
					if s.Assign.IsValid() {
						if c, ok := pkgOf(s.Type); ok {
							out = append(out, parentFwd{s.Name.Name, "type alias", c, p.name, fset.Position(s.Pos()).Line, ""})
						}
					}
				case *ast.ValueSpec:
					kind := "var"
					if gd.Tok == token.CONST {
						kind = "const"
					}
					for i, v := range s.Values {
						if i >= len(s.Names) {
							break
						}
						for {
							pe, ok := v.(*ast.ParenExpr)
							if !ok {
								break
							}
							v = pe.X
						}
						if _, isSel := v.(*ast.SelectorExpr); !isSel {
							continue
						}
						if c, ok := pkgOf(v); ok {
							out = append(out, parentFwd{s.Names[i].Name, kind, c, p.name, fset.Position(s.Names[i].Pos()).Line, ""})
						}
					}
				}
			}
		}
	}

	// (c): functions and methods. Receiver types are every type some method of
	// the parent is declared on; "" is the package-level functions.
	recvTypes := map[string]bool{"": true}
	for _, p := range fs {
		for _, d := range p.f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv != nil && len(fd.Recv.List) == 1 {
				rt := fd.Recv.List[0].Type
				for {
					switch x := rt.(type) {
					case *ast.StarExpr:
						rt = x.X
						continue
					case *ast.IndexExpr:
						rt = x.X
						continue
					case *ast.IndexListExpr:
						rt = x.X
						continue
					}
					break
				}
				if id, ok := rt.(*ast.Ident); ok {
					recvTypes[id.Name] = true
				}
			}
		}
	}
	for rtype := range recvTypes {
		fields := map[string]string{}
		helpers := map[string]string{}
		if rtype != "" {
			for _, p := range fs {
				for k, v := range childFields(p.f, rtype, p.imps) {
					fields[k] = v
				}
				for k, v := range childHelpers(p.f, rtype, p.imps) {
					helpers[k] = v
				}
			}
		}
		for _, p := range fs {
			if len(p.imps) == 0 {
				continue
			}
			hits, _ := methodForwards(p.f, fwdCfg{recvType: rtype, imps: p.imps, fields: fields, helpers: helpers})
			for _, h := range hits {
				name, detail, _ := strings.Cut(h, " -> ")
				kind := "func"
				if rtype != "" {
					name = rtype + "." + name
					kind = "method"
				}
				out = append(out, parentFwd{name, kind, "", p.name, 0, detail})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].File < out[j].File
	})
	return out
}

// goListPackage is one `go list` row of the module.
type goListPackage struct {
	Path, Dir string
	Files     []string // non-test Go files (GoFiles + CgoFiles)
}

// listModulePackages returns every package under knomit/internal/ via go list.
func listModulePackages(t *testing.T) []goListPackage {
	t.Helper()
	cmd := exec.Command("go", "list", "-f", `{{.ImportPath}}|{{.Dir}}|{{join .GoFiles ","}}|{{join .CgoFiles ","}}`, "knomit/internal/...")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list knomit/internal/...: %v\n%s", err, out)
	}
	var pkgs []goListPackage
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.Split(line, "|")
		if len(parts) != 4 {
			t.Fatalf("unexpected go list row %q", line)
		}
		p := goListPackage{Path: parts[0], Dir: parts[1]}
		for _, grp := range parts[2:] {
			for _, f := range strings.Split(grp, ",") {
				if f != "" {
					p.Files = append(p.Files, f)
				}
			}
		}
		pkgs = append(pkgs, p)
	}
	return pkgs
}

// TestParents_NoForwardsIntoChildren (guard P): a package P under internal/
// that has child packages (an import path with P as a strict prefix, internal/
// children included) declares, in its non-test files, no
//
//   - type alias of a child's type,
//   - package-level var or const whose initializer is only a child's name,
//   - exported func or method whose body is one statement that calls a child's
//     function or method with the parameters passed through.
//
// Callers import the owning package (convention: no re-export or forwarding
// layers). This is the generic form of the store's A1 and A2, which stay: A2 is
// stricter inside package store (a package-qualified call counts whatever its
// arguments; the injectsStoreState entries are the ones that inject store
// state) and A1 carries the non-importable-with-outside-caller exemption that
// parentForwardAllowlist records for SyncDSNParam. The detectors share
// methodForwards, so the shapes A2 matches are matched here: field handles,
// helper-method handles, local copies, `var x = ...`, parentheses, call on a
// call, bind-then-return. Packages and constructors that BUILD a child value
// from the receiver's state (fsck.NewChecker(s.repo)) are not forwards.
//
// Known gaps, deliberately not closed (each needs data-flow the shape check
// does not do): method values (f := child.F; return f()), named results
// assigned from the call, helpers returning a struct that holds the child,
// closures, forwards through a package-level var holding a child value, calls
// whose arguments are wrapped (child.F(a, 1) is a wrapper that does something),
// unexported forwards (only declarations and exported functions are matched),
// files excluded by build constraints in the default configuration, and
// children outside internal/ (only the internal tree is scanned).
//
// Non-vacuity: the parents come from go list; every layer (platform, domain,
// ...) that has a package with a child must have at least one parent parsed
// (at least one file), and at least one layer must; the self-test runs the
// detector on inline source with a flagged and a clean case per shape.
func TestParents_NoForwardsIntoChildren(t *testing.T) {
	pkgs := listModulePackages(t)
	paths := make([]string, len(pkgs))
	for i, p := range pkgs {
		paths[i] = p.Path
	}
	hasChild := func(parent string) bool {
		return slices.ContainsFunc(paths, childPackages(parent))
	}
	fset := token.NewFileSet()
	found := map[string]bool{}
	var offenders []string
	examined := map[int]int{}   // layer rank -> parents parsed
	expecting := map[int]bool{} // layer rank -> has a package with a child
	for _, p := range pkgs {
		if !hasChild(p.Path) {
			continue
		}
		rank, exempt, ok := layerOf(p.Path)
		if !ok || exempt {
			continue
		}
		expecting[rank] = true
		files := map[string]any{}
		for _, f := range p.Files {
			files[filepath.Join(p.Dir, f)] = nil
		}
		if len(files) > 0 {
			examined[rank]++
		}
		for _, h := range parentForwards(t, fset, p.Path, files) {
			key := p.Path + "." + h.Name
			found[key] = true
			if _, ok := parentForwardAllowlist[key]; ok {
				continue
			}
			where := fmt.Sprintf("%s: %s %s", p.Path, h.Kind, h.Name)
			if h.Line > 0 {
				where += fmt.Sprintf(" (%s:%d) -> %s", filepath.Base(h.File), h.Line, strings.TrimPrefix(h.Child, "knomit/"))
			} else {
				where += fmt.Sprintf(" (%s) -> %s", filepath.Base(h.File), h.Detail)
			}
			offenders = append(offenders, where)
		}
	}
	if len(expecting) == 0 {
		t.Fatalf("go list found no package under internal/ with a child package: the scan matches nothing")
	}
	for rank := range expecting {
		if examined[rank] == 0 {
			t.Errorf("layer %s has packages with children but no parent was parsed: the scan is blind there", layerNames[rank])
		}
	}
	for key, reason := range parentForwardAllowlist {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("parentForwardAllowlist[%s] has no reason", key)
		}
		if !found[key] {
			t.Errorf("parentForwardAllowlist lists %s but it is no longer a forward into a child: delete the entry", key)
		}
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("parent packages forward a child's names: delete the forward and point callers at the child (or add a parentForwardAllowlist entry with a reason):\n  %s", strings.Join(offenders, "\n  "))
	}
}

// TestParents_ForwardGuardDiscriminates runs the detector on inline source: a
// flagged and a clean case per shape.
func TestParents_ForwardGuardDiscriminates(t *testing.T) {
	const parent = "knomit/internal/corpus/synthesize"
	const imp = "package synthesize\nimport (\n\t\"knomit/internal/corpus/synthesize/step\"\n\tst \"knomit/internal/corpus/synthesize/step\"\n\t\"knomit/internal/corpus/synthesize/internal/inner\"\n\t\"knomit/internal/corpus/other\"\n\t\"time\"\n)\ntype Service struct{ stp *step.Runner; o *other.T; n int }\n"
	run := func(src string) []parentFwd {
		return parentForwards(t, token.NewFileSet(), parent, map[string]any{"a.go": src})
	}
	flagged := map[string]struct{ src, name string }{
		"type alias":             {imp + "type Result = step.Result\n", "Result"},
		"type alias, aliased":    {imp + "type Result = st.Result\n", "Result"},
		"type alias, pointer":    {imp + "type Result = *step.Result\n", "Result"},
		"type alias, generic":    {imp + "type Result = step.Box[int]\n", "Result"},
		"type alias, internal/":  {imp + "type Result = inner.Result\n", "Result"},
		"var forward":            {imp + "var ErrX = step.ErrX\n", "ErrX"},
		"var forward, func":      {imp + "var F = step.Run\n", "F"},
		"var forward, group":     {imp + "var (\n\ta = 1\n\tB = step.B\n)\n", "B"},
		"var forward, paren":     {imp + "var B = (step.B)\n", "B"},
		"const forward":          {imp + "const K = step.K\n", "K"},
		"func wrapper":           {imp + "func Run(a int, b string) error { return step.Run(a, b) }\n", "Run"},
		"func wrapper, stmt":     {imp + "func Go(a int) { step.Go(a) }\n", "Go"},
		"func wrapper, no args":  {imp + "func Name() string { return step.Name() }\n", "Name"},
		"func wrapper, internal": {imp + "func Run(a int) error { return inner.Run(a) }\n", "Run"},
		"func wrapper, bind":     {imp + "func Run(a int) (int, error) { n, err := step.Run(a); return n, err }\n", "Run"},
		"func wrapper, variadic": {imp + "func Run(a int, rest ...int) error { return step.Run(a, rest...) }\n", "Run"},
		"method, field":          {imp + "func (s *Service) Run(a int) error { return s.stp.Run(a) }\n", "Service.Run"},
		"method, field, stmt":    {imp + "func (s Service) Poke() { s.stp.Poke() }\n", "Service.Poke"},
		"method, pkg call":       {imp + "func (s *Service) Run(a int) error { return step.Run(a) }\n", "Service.Run"},
		"method, call on call":   {imp + "func (s *Service) Run(a int) error { return s.helper().Run(a) }\nfunc (s *Service) helper() *step.Runner { return s.stp }\n", "Service.Run"},
		"method, local copy":     {imp + "func (s *Service) Run() error { r := s.stp; return r.Run() }\n", "Service.Run"},
		"method, var decl":       {imp + "func (s *Service) Run() error { var r = s.stp; return r.Run() }\n", "Service.Run"},
		"method, parenthesised":  {imp + "func (s *Service) Run() error { return (s.stp).Run() }\n", "Service.Run"},
		"method, bind-return":    {imp + "func (s *Service) Run() (int, error) { n, err := s.stp.Run(); return n, err }\n", "Service.Run"},
	}
	for name, c := range flagged {
		got := run(c.src)
		if len(got) != 1 || got[0].Name != c.name {
			t.Errorf("%s: got %+v, want exactly %s", name, got, c.name)
		}
	}
	clean := map[string]string{
		"constructor from state":   imp + "func (s *Service) Checker() *step.Checker { return step.NewChecker(s.n) }\n",
		"constructor, receiver":    imp + "func (s *Service) Checker() *step.Checker { return step.NewChecker(s) }\n",
		"handle accessor":          imp + "func (s *Service) Runner() *step.Runner { return s.stp }\n",
		"wrapper adds an argument": imp + "func Run(a int) error { return step.Run(a, 1) }\n",
		"wrapper reorders":         imp + "func Run(a, b int) error { return step.Run(b, a) }\n",
		"wrapper does work":        imp + "func Run(a int) error { if a < 0 { return nil }; return step.Run(a) }\n",
		"two statements":           imp + "func Run(a int) error { _ = a; return step.Run(a) }\n",
		"unexported func":          imp + "func run(a int) error { return step.Run(a) }\n",
		"unexported method":        imp + "func (s *Service) run() error { return s.stp.Run() }\n",
		"method of non-child":      imp + "func (s *Service) Run() int { return s.o.Run() }\n",
		"method, local, then work": imp + "func (s *Service) Run() error { r := s.stp; _ = s.n; return r.Run() }\n",
		"non-child package":        imp + "func Run(a int) error { return other.Run(a) }\n",
		"stdlib call":              imp + "func Now() time.Time { return time.Now() }\n",
		"stdlib alias":             imp + "type D = time.Duration\n",
		"non-child alias":          imp + "type T = other.T\n",
		"local alias":              imp + "type A int\ntype B = A\n",
		"var from a call":          imp + "var X = step.New()\n",
		"var, stdlib":              imp + "var X = time.Second\n",
		"method with params":       imp + "func (s *Service) Run(a int) error { return s.o.Run(a) }\n",
		"field not child typed":    imp + "func (s *Service) Run() int { return s.n.Abs() }\n",
		"plain function on field":  imp + "func Run(s *Service) error { return s.stp.Run() }\n",
	}
	for name, src := range clean {
		if got := run(src); len(got) != 0 {
			t.Errorf("%s: wrongly flagged: %+v", name, got)
		}
	}
	// A package with no child import has nothing to forward; a child of a
	// sibling (prefix without the slash) is not a child.
	if got := parentForwards(t, token.NewFileSet(), "knomit/internal/corpus/syn", map[string]any{"a.go": imp + "type R = step.Result\n"}); len(got) != 0 {
		t.Errorf("package syn treated synthesize/step as a child: %+v", got)
	}
}
