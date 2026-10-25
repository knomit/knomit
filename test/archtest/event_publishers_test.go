package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The repo event hubs are split: events.TaskHub and events.RepoEventHub let a
// consumer subscribe, read and start tasks, and nothing else; the methods that
// announce (BroadcastIndex, PublishTask, CancelTasks, RepoEventPublisher.Publish,
// ...) live only on events.TaskPublisher and events.RepoEventPublisher, which
// the owner (the repos Manager and RepoInstance) keeps in unexported fields.
// That is what keeps invariant "IndexEvents are published only by
// Machine.publish after a transition" and the job API's single-flight (a
// phantom running task wedges every POST with 409 and cannot be cancelled by
// DELETE) enforced by the compiler instead of by who happens to import events.
//
// The split only holds while nothing hands a publisher out. This guard fails
// when a non-test file outside the events package declares
//
//   - an exported func or method with a publisher in its results,
//   - an exported struct field (embedded or named) of a publisher type,
//   - an exported package-level var or const of a publisher type, or
//   - an exported type alias or definition naming a publisher.
//
// Parameters are not exposure: a constructor may TAKE a publisher. A
// non-exported accessor is the owner's own business. Build-constrained files
// are scanned too (go list IgnoredGoFiles).
//
// Non-vacuity: events must still declare both publisher types, and at least one
// scanned file must import events (the repos parent does); the self-test
// (TestEventPublishers_GuardDiscriminates) runs the detector on inline source
// with a flagged and a clean case per shape.
const eventsPkgPath = "knomit/internal/corpus/repos/events"

var publisherTypes = map[string]bool{"TaskPublisher": true, "RepoEventPublisher": true}

// exposedPublishers returns one description per exposure in src.
func exposedPublishers(t *testing.T, fset *token.FileSet, name string, src any) []string {
	t.Helper()
	f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	locals := map[string]bool{}
	for _, im := range f.Imports {
		p, _ := strconv.Unquote(im.Path.Value)
		if p != eventsPkgPath {
			continue
		}
		n := "events"
		if im.Name != nil {
			n = im.Name.Name
		}
		locals[n] = true
	}
	if len(locals) == 0 {
		return nil
	}
	names := func(e ast.Node) bool { // does e mention a publisher type?
		hit := false
		if e == nil {
			return false
		}
		ast.Inspect(e, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if x, ok := sel.X.(*ast.Ident); ok && locals[x.Name] && publisherTypes[sel.Sel.Name] {
					hit = true
				}
			}
			return !hit
		})
		return hit
	}
	line := func(p token.Pos) string { return name + ":" + strconv.Itoa(fset.Position(p).Line) }
	var out []string
	structFields := func(st *ast.StructType, owner string) {
		for _, fld := range st.Fields.List {
			if !names(fld.Type) {
				continue
			}
			if len(fld.Names) == 0 { // embedded: exported when the type name is
				if tn := embeddedName(fld.Type); ast.IsExported(tn) {
					out = append(out, line(fld.Pos())+": "+owner+" embeds a publisher")
				}
				continue
			}
			for _, n := range fld.Names {
				if n.IsExported() {
					out = append(out, line(n.Pos())+": field "+owner+"."+n.Name+" exposes a publisher")
				}
			}
		}
	}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Name.IsExported() && d.Type.Results != nil && names(d.Type.Results) {
				out = append(out, line(d.Pos())+": func "+d.Name.Name+" returns a publisher")
			}
		case *ast.GenDecl:
			for _, sp := range d.Specs {
				switch sp := sp.(type) {
				case *ast.TypeSpec:
					if st, ok := sp.Type.(*ast.StructType); ok {
						structFields(st, sp.Name.Name)
					} else if it, ok := sp.Type.(*ast.InterfaceType); ok {
						for _, m := range it.Methods.List {
							if ft, ok := m.Type.(*ast.FuncType); ok && ft.Results != nil && names(ft.Results) {
								for _, n := range m.Names {
									if n.IsExported() {
										out = append(out, line(n.Pos())+": interface method "+sp.Name.Name+"."+n.Name+" returns a publisher")
									}
								}
							}
						}
					} else if sp.Name.IsExported() && names(sp.Type) {
						out = append(out, line(sp.Pos())+": type "+sp.Name.Name+" names a publisher")
					}
				case *ast.ValueSpec:
					if names(sp.Type) {
						for _, n := range sp.Names {
							if n.IsExported() {
								out = append(out, line(n.Pos())+": var "+n.Name+" exposes a publisher")
							}
						}
					}
				}
			}
		}
	}
	return out
}

// embeddedName is the name an embedded field's type gives the field.
func embeddedName(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.StarExpr:
		return embeddedName(e.X)
	case *ast.SelectorExpr:
		return e.Sel.Name
	case *ast.Ident:
		return e.Name
	}
	return ""
}

func TestEventPublishers_NotExposed(t *testing.T) {
	fset := token.NewFileSet()
	var offenders []string
	importers, eventsDecl := 0, map[string]bool{}
	for _, p := range listModulePackages(t) {
		for _, file := range p.Files {
			full := filepath.Join(p.Dir, file)
			if p.Path == eventsPkgPath {
				f, err := parser.ParseFile(fset, full, nil, parser.SkipObjectResolution)
				if err != nil {
					t.Fatalf("parse %s: %v", full, err)
				}
				for _, d := range f.Decls {
					if g, ok := d.(*ast.GenDecl); ok {
						for _, sp := range g.Specs {
							if ts, ok := sp.(*ast.TypeSpec); ok && publisherTypes[ts.Name.Name] {
								eventsDecl[ts.Name.Name] = true
							}
						}
					}
				}
				continue
			}
			f, err := parser.ParseFile(fset, full, nil, parser.SkipObjectResolution|parser.ImportsOnly)
			if err != nil {
				t.Fatalf("parse %s: %v", full, err)
			}
			for _, im := range f.Imports {
				if im.Path.Value == strconv.Quote(eventsPkgPath) {
					importers++
				}
			}
			offenders = append(offenders, exposedPublishers(t, fset, full, nil)...)
		}
	}
	for n := range publisherTypes {
		if !eventsDecl[n] {
			t.Fatalf("events no longer declares %s: this guard's publisherTypes list is stale", n)
		}
	}
	if importers == 0 {
		t.Fatal("no non-test file imports events: the scan is vacuous")
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("a publisher of the repo event hubs is handed out; only the repos owner may hold events.TaskPublisher / events.RepoEventPublisher, in unexported fields (consumers get the *TaskHub / *RepoEventHub):\n  %s", strings.Join(offenders, "\n  "))
	}
}

func TestEventPublishers_GuardDiscriminates(t *testing.T) {
	const imp = "package repos\nimport \"knomit/internal/corpus/repos/events\"\nimport ev \"knomit/internal/corpus/repos/events\"\ntype M struct{}\n"
	run := func(src string) []string {
		return exposedPublishers(t, token.NewFileSet(), "a.go", imp+src)
	}
	flagged := map[string]string{
		"method returns task publisher":      "func (m *M) TaskPublisher() *events.TaskPublisher { return nil }\n",
		"func returns repo publisher":        "func Pub() (*events.RepoEventPublisher, error) { return nil, nil }\n",
		"aliased import":                     "func Pub() *ev.TaskPublisher { return nil }\n",
		"exported field":                     "type C struct{ Pub *events.TaskPublisher }\n",
		"embedded exported":                  "type C struct{ *events.TaskPublisher }\n",
		"exported var":                       "var Pub *events.TaskPublisher\n",
		"exported alias":                     "type P = events.TaskPublisher\n",
		"interface method":                   "type I interface{ Pub() *events.TaskPublisher }\n",
		"exported method on unexported type": "type m struct{}\nfunc (m) Pub() *events.RepoEventPublisher { return nil }\n",
	}
	for name, src := range flagged {
		if len(run(src)) == 0 {
			t.Errorf("%s: not flagged", name)
		}
	}
	clean := map[string]string{
		"unexported field":      "type C struct{ pub *events.TaskPublisher }\n",
		"unexported accessor":   "func (m *M) pub() *events.TaskPublisher { return nil }\n",
		"parameter":             "func New(p *events.TaskPublisher) {}\n",
		"hub is fine":           "func (m *M) TaskHub() *events.TaskHub { return nil }\nfunc (m *M) RepoEventHub() *events.RepoEventHub { return nil }\n",
		"unexported var":        "var pub *events.TaskPublisher\n",
		"other package's names": "func X() *other.TaskPublisher { return nil }\n",
	}
	for name, src := range clean {
		if got := run(src); len(got) != 0 {
			t.Errorf("%s: flagged %v", name, got)
		}
	}
}
