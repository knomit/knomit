package archtest

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Guards that hold the web split (internal/edge/web into httpx, api/kb,
// api/manage, api/jobs and api/origins beside the existing hal) to the shape
// the project owner ruled on 2026-10-24 and in the follow-up on PR #621: each
// api/* package exports exactly func Mount (positional parameters, no
// exported Deps type) and keeps its own route table, the parent reaches the api
// packages only from router.go and server.go, and no web-family package keeps
// a local one-call shim into another one. They landed before any code moved,
// so each has a detector self-test on synthetic source. Since 12f every planned
// api package exists, and guard M fails when one of them is missing.

const webAPIPrefix = webPkg + "/api"

// isWebAPI reports whether an import path is internal/edge/web/api or a
// package under it.
func isWebAPI(p string) bool {
	return p == webAPIPrefix || strings.HasPrefix(p, webAPIPrefix+"/")
}

// webAPIImporters are the files of the parent package internal/edge/web that
// may import an api package: router.go mounts each one (api/x.Mount), and
// server.go is the router's other half (Server.Handler mounts the API router).
// Under the follow-up ruling an api package exports nothing but Mount, so
// server.go no longer needs one for a field type: 12d removed
// Server.SessionManager (api/origins' Mount owns its manager, and router.go is
// the one importer of api/origins), and the job registry is deleted. It stays
// listed because the ruling names the pair; dropping it is a one-entry change
// once api/jobs lands without a server.go import.
var webAPIImporters = map[string]bool{"router.go": true, "server.go": true}

// importScan is what one pass over the parent's files saw: each file's package
// clause and its import paths (by file base name).
type importScan struct {
	pkgs    map[string]string
	imports map[string][]string
}

// webAPIImportViolations parses files (name -> nil to read from disk, or the
// source text) and returns one "file imports path" per import of an api
// package from a file whose base name is not in webAPIImporters, plus what the
// pass saw. Any import form counts (named, blank, dot).
func webAPIImportViolations(t *testing.T, files map[string]any) (bad []string, scan importScan) {
	t.Helper()
	scan = importScan{pkgs: map[string]string{}, imports: map[string][]string{}}
	fset := token.NewFileSet()
	for name, src := range files {
		f, err := parser.ParseFile(fset, name, src, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		base := filepath.Base(name)
		scan.pkgs[base] = f.Name.Name
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			scan.imports[base] = append(scan.imports[base], p)
			if isWebAPI(p) && !webAPIImporters[base] {
				bad = append(bad, base+" imports "+p)
			}
		}
	}
	sort.Strings(bad)
	return bad, scan
}

// TestWeb_OnlyRouterAndServerImportAPI (guard R): inside the parent package
// internal/edge/web, only router.go and server.go import a package under
// internal/edge/web/api. The route table of each api package lives in that
// package and is mounted from router.go (server.go is the one other file allowed).
// Every other parent file (a handler, a middleware, a helper) reaching into an
// api package would turn the parent back into the place handlers live. Test
// files are not scanned (go list's non-test files, build-constrained ones
// included); children of web are not this guard's concern (TestLayers lists
// every edge between them).
//
// Non-vacuity: it holds today, when no api package exists, so it proves it
// read the parent: the file list comes from go list, the scanned count must
// equal go list's, every package clause must be web, router.go and server.go
// must both be among the scanned files, and the scan must have recorded
// router.go importing chi (the router it builds). TestWeb_APIImportDetector_
// Discriminates pins the detector on synthetic source.
func TestWeb_OnlyRouterAndServerImportAPI(t *testing.T) {
	web := webFamily(t)[0]
	files := map[string]any{}
	for _, f := range web.Files {
		files[filepath.Join(web.Dir, f)] = nil
	}
	bad, scan := webAPIImportViolations(t, files)
	if len(scan.pkgs) != len(web.Files) {
		t.Fatalf("scanned %d files, go list returned %d", len(scan.pkgs), len(web.Files))
	}
	for file, pkg := range scan.pkgs {
		if pkg != "web" {
			t.Errorf("%s: package clause %q, want web: the guard is scanning the wrong package", file, pkg)
		}
	}
	for f := range webAPIImporters {
		if _, ok := scan.pkgs[f]; !ok {
			t.Errorf("non-vacuity: %s was not scanned: the guard is not reading the parent's files", f)
		}
	}
	if !slices.Contains(scan.imports["router.go"], "github.com/go-chi/chi/v5") {
		t.Errorf("non-vacuity: the scan did not record router.go importing github.com/go-chi/chi/v5 (got %v): imports are not being read", scan.imports["router.go"])
	}
	for _, b := range bad {
		t.Errorf("%s: only router.go (Mount) and server.go may import an api package; route through them", b)
	}
}

func TestWeb_APIImportDetector_Discriminates(t *testing.T) {
	const kb, jobs = webAPIPrefix + "/kb", webAPIPrefix + "/jobs"
	src := map[string]any{
		"internal/edge/web/router.go":       "package web\nimport (\n\t\"" + kb + "\"\n\t\"" + jobs + "\"\n)\n",
		"internal/edge/web/server.go":       "package web\nimport \"" + jobs + "\"\n",
		"internal/edge/web/handlers_x.go":   "package web\nimport \"" + kb + "\"\n",
		"internal/edge/web/named.go":        "package web\nimport k \"" + kb + "\"\n",
		"internal/edge/web/blank.go":        "package web\nimport _ \"" + webAPIPrefix + "/origins\"\n",
		"internal/edge/web/api_root.go":     "package web\nimport \"" + webAPIPrefix + "\"\n",
		"internal/edge/web/sub/router.go":   "package web\nimport \"" + kb + "\"\n", // base name decides: same package dir in practice
		"internal/edge/web/apiish.go":       "package web\nimport \"" + webPkg + "/apix\"\n",
		"internal/edge/web/httpx_user.go":   "package web\nimport \"" + webPkg + "/httpx\"\n",
		"internal/edge/web/hal_user.go":     "package web\nimport \"" + webPkg + "/hal\"\n",
		"internal/edge/web/router_extra.go": "package web\nimport \"" + kb + "\"\n",
	}
	bad, scan := webAPIImportViolations(t, src)
	want := []string{
		"api_root.go imports " + webAPIPrefix,
		"blank.go imports " + webAPIPrefix + "/origins",
		"handlers_x.go imports " + kb,
		"named.go imports " + kb,
		"router_extra.go imports " + kb,
	}
	if !slices.Equal(bad, want) {
		t.Errorf("violations\n%q\nwant\n%q", bad, want)
	}
	if len(scan.pkgs) != 10 { // sub/router.go and router.go share a base name
		t.Errorf("scan.pkgs has %d files, want 10: %v", len(scan.pkgs), scan.pkgs)
	}
	if !slices.Equal(scan.imports["server.go"], []string{jobs}) {
		t.Errorf("scan.imports[server.go] = %v, want [%s]", scan.imports["server.go"], jobs)
	}
}

// webAPIExportAllowlist declares the api packages the web split creates, keyed
// by import path relative to internal/edge/web, and for each one every
// exported name it may declare besides Mount, with the reason. The rulings
// (2026-10-24, and the follow-up on PR #621): every api package exports
// exactly func Mount, which takes positional parameters, so there is no
// exported Deps type, and returns nothing. api/origins' Mount creates and owns
// the session manager (12d removed Server.SessionManager). api/jobs' registry is deleted:
// jobs go through TaskHub only. So every list is EMPTY. A future name needs a
// reviewed entry with a reason. Names are "T" for a type, func, var or const,
// and "T.M" / "T.F" for a method or field reachable through exported type T,
// including members reached through an alias or promoted from an embedded
// type.
//
// The keys are the api packages: {kb, manage, jobs, origins}, all created by
// 12f. A key whose package go list does not return fails
// TestWebAPI_ExportsOnlyMount, so a removed or renamed package cannot leave its
// entry parked here, and a package created somewhere else instead is caught by
// webFamilyLayout, which fails a web-family package outside api/ that it does
// not list.
var webAPIExportAllowlist = map[string]map[string]string{
	"api/kb":      {},
	"api/manage":  {},
	"api/jobs":    {},
	"api/origins": {},
}

// webFamilyLayout lists the non-api packages the web family may contain, keyed
// by path relative to internal/edge/web ("" is the parent), with the reason.
// Every other web-family package outside api/ fails. A resource package placed
// outside api/ (internal/edge/web/jobs, say) therefore cannot escape the api
// rules by its location. Guard W does not cover this: it only requires a
// webKnownTypes entry, which any new package can add for itself. httpx is
// listed before it exists (12b creates it). An entry allows the package to
// exist but does not require it.
var webFamilyLayout = map[string]string{
	"":      "the parent: server, router (mounts the api packages), middleware, static, embed",
	"hal":   "HAL wire types and writers, stdlib only",
	"httpx": "the shared HTTP helpers of the web split (12b)",
}

// apiExport is one exported name a package declares or makes reachable.
type apiExport struct {
	Name string // "X", or "T.M" / "T.F" for a method or field reachable through exported type T
	Kind string // func ("func returning N" when it has results), type, var, const, method, field; a reached member says how ("method via alias of t", "field promoted from e")
	File string
}

// localTypeName unwraps *T, T[A] and T[A, B] and returns T when it is a
// package-local identifier ("" for a qualified or composite type).
func localTypeName(e ast.Expr) string {
	for {
		switch x := e.(type) {
		case *ast.StarExpr:
			e = x.X
			continue
		case *ast.IndexExpr:
			e = x.X
			continue
		case *ast.IndexListExpr:
			e = x.X
			continue
		case *ast.ParenExpr:
			e = x.X
			continue
		}
		break
	}
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// exportedNames parses files (name -> nil to read from disk, or the source
// text) and returns every exported name they declare or make reachable:
//
//   - package-level funcs, types, vars and consts;
//   - for every exported type T, its exported fields and methods (an embedded
//     field counts by its type name). When T is an alias of a local type
//     (type T = t), it also gets t's members. It also gets the exported fields
//     and methods promoted from every local type it embeds, transitively and
//     whether the embedded type is exported or not. That closes the
//     `type JobRegistry = jobRegistry` and `type Outer struct{ inner }` routes
//     to an unexported type's exported members.
//
// Methods and fields of an unexported type that no exported type aliases or
// embeds are not reported: no caller outside the package can reach them (a
// ServeHTTP on an unexported handler type is fine). It also returns each
// file's package clause.
func exportedNames(t *testing.T, files map[string]any) (out []apiExport, pkgs map[string]string) {
	t.Helper()
	type typeDecl struct {
		file   string
		alias  string   // local target of `type T = t`, "" otherwise
		embeds []string // local types embedded in T's struct
		fields []string // T's exported fields (embedded ones by type name)
	}
	type member struct{ name, kind, file string }
	pkgs = map[string]string{}
	decls := map[string]*typeDecl{}
	methods := map[string][]member{} // receiver type -> exported methods
	fset := token.NewFileSet()
	for name, src := range files {
		f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		base := filepath.Base(name)
		pkgs[base] = f.Name.Name
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if !d.Name.IsExported() {
					continue
				}
				if d.Recv == nil {
					kind := "func"
					if n := d.Type.Results.NumFields(); n > 0 {
						kind = fmt.Sprintf("func returning %d", n)
					}
					out = append(out, apiExport{d.Name.Name, kind, base})
					continue
				}
				if len(d.Recv.List) == 1 {
					if rt := localTypeName(d.Recv.List[0].Type); rt != "" {
						methods[rt] = append(methods[rt], member{d.Name.Name, "method", base})
					}
				}
			case *ast.GenDecl:
				for _, sp := range d.Specs {
					switch s := sp.(type) {
					case *ast.TypeSpec:
						td := &typeDecl{file: base}
						decls[s.Name.Name] = td
						if s.Assign.IsValid() {
							td.alias = localTypeName(s.Type)
						}
						if st, ok := s.Type.(*ast.StructType); ok {
							for _, fl := range st.Fields.List {
								if len(fl.Names) == 0 { // embedded: named by its type
									typ := fl.Type
									if x, ok := typ.(*ast.StarExpr); ok {
										typ = x.X
									}
									var n string
									switch x := typ.(type) {
									case *ast.SelectorExpr:
										n = x.Sel.Name
									default:
										n = localTypeName(typ)
										if n != "" {
											td.embeds = append(td.embeds, n)
										}
									}
									if n != "" && ast.IsExported(n) {
										td.fields = append(td.fields, n)
									}
									continue
								}
								for _, n := range fl.Names {
									if n.IsExported() {
										td.fields = append(td.fields, n.Name)
									}
								}
							}
						}
						if s.Name.IsExported() {
							out = append(out, apiExport{s.Name.Name, "type", base})
						}
					case *ast.ValueSpec:
						kind := "var"
						if d.Tok == token.CONST {
							kind = "const"
						}
						for _, n := range s.Names {
							if n.IsExported() {
								out = append(out, apiExport{n.Name, kind, base})
							}
						}
					}
				}
			}
		}
	}
	// The members reachable through local type name: its own fields and
	// methods, an alias target's, and every embedded type's (promoted),
	// transitively. how says the route for the report.
	var reach func(name, how string, seen map[string]bool) []member
	reach = func(name, how string, seen map[string]bool) []member {
		if seen[name] {
			return nil
		}
		seen[name] = true
		td := decls[name]
		var ms []member
		for _, m := range methods[name] {
			ms = append(ms, member{m.name, m.kind + how, m.file})
		}
		if td == nil {
			return ms
		}
		for _, f := range td.fields {
			ms = append(ms, member{f, "field" + how, td.file})
		}
		if td.alias != "" {
			ms = append(ms, reach(td.alias, " via alias of "+td.alias, seen)...)
		}
		for _, e := range td.embeds {
			ms = append(ms, reach(e, " promoted from "+e, seen)...)
		}
		return ms
	}
	for name := range decls {
		if !ast.IsExported(name) {
			continue
		}
		done := map[string]bool{}
		for _, m := range reach(name, "", map[string]bool{}) {
			if done[m.name] {
				continue // the shallowest route wins (a field shadows a promoted one)
			}
			done[m.name] = true
			out = append(out, apiExport{name + "." + m.name, m.kind, m.file})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, pkgs
}

// apiExportProblems applies the ruling to one api package's exports. Mount
// must be declared, as a package-level func with no results: it registers
// routes on the router it is given, so a returned handler, router, closer or
// error would be a second export by another name. Every other exported name
// must be in allow. Every name in allow must be exported (a stale entry fails).
func apiExportProblems(pkg string, exports []apiExport, allow map[string]string) []string {
	var out []string
	seen := map[string]bool{}
	mount := false
	for _, e := range exports {
		seen[e.Name] = true
		if e.Name == "Mount" {
			mount = true
			switch {
			case strings.HasPrefix(e.Kind, "func returning "):
				out = append(out, fmt.Sprintf("%s: Mount is a %s (%s); Mount registers its routes on r and returns nothing", pkg, e.Kind, e.File))
			case e.Kind != "func":
				out = append(out, fmt.Sprintf("%s: Mount is a %s (%s); it must be the package's func Mount(r, ...)", pkg, e.Kind, e.File))
			}
			continue
		}
		if reason, ok := allow[e.Name]; ok {
			if strings.TrimSpace(reason) == "" {
				out = append(out, fmt.Sprintf("%s: webAPIExportAllowlist entry %s has no reason", pkg, e.Name))
			}
			continue
		}
		out = append(out, fmt.Sprintf("%s: exports %s %s (%s): an api package exports only func Mount (positional parameters, no exported Deps type); unexport it, or move it to httpx if two packages need it", pkg, e.Kind, e.Name, e.File))
	}
	if !mount {
		out = append(out, fmt.Sprintf("%s: declares no func Mount: each api package mounts its own routes through one Mount", pkg))
	}
	for name := range allow {
		if !seen[name] {
			out = append(out, fmt.Sprintf("%s: webAPIExportAllowlist lists %s but the package does not export it: delete the entry", pkg, name))
		}
	}
	sort.Strings(out)
	return out
}

// webLayoutProblems checks the web family's package paths (relative to
// internal/edge/web, "" for the parent). An api package must be a key of
// apis. Any other package must be a key of layout.
func webLayoutProblems(rels []string, apis map[string]map[string]string, layout map[string]string) []string {
	var out []string
	for _, rel := range rels {
		if isWebAPI(webPkg + "/" + rel) {
			if _, ok := apis[rel]; !ok {
				out = append(out, fmt.Sprintf("%s/%s: not one of the planned api packages (webAPIExportAllowlist keys); a new api package needs an entry, an empty one allows only Mount, added in review", webPkg, rel))
			}
			continue
		}
		if _, ok := layout[rel]; !ok {
			out = append(out, fmt.Sprintf("%s/%s: the web family holds only the parent, hal, httpx and api/* packages (webFamilyLayout); a resource package goes under api/, anything else needs a reviewed webFamilyLayout entry", webPkg, rel))
		}
	}
	sort.Strings(out)
	return out
}

// TestWebAPI_ExportsOnlyMount (guard M) checks two rules.
//
// Layout: every package go list returns under internal/edge/web is the parent,
// a webFamilyLayout child (hal, httpx), or one of the planned api packages
// (the webAPIExportAllowlist keys).
//
// Exports: every package under internal/edge/web/api exports only func Mount,
// which has no results, plus its webAPIExportAllowlist names, and those lists
// are empty. Counted
// are funcs, types, vars and consts, plus every exported method or field
// reachable through an exported type, directly or via an alias or an
// embedded type (see exportedNames). So a second handler constructor, a
// shared helper, an exported Deps struct or an exported type of any kind
// fails. A helper two packages need belongs in httpx.
//
// Every webAPIExportAllowlist key must be a package go list returns; a missing
// one fails (since 12f, the last move, there is no pending state).
// TestWebAPI_ExportDetector_Discriminates pins the detector and both policies
// on synthetic source. For every package, non-vacuity holds: its files come
// from go list, the scanned count must equal go list's, every package clause
// must be the last element of the import path, and Mount must be found (so an
// empty or wrong scan fails).
func TestWebAPI_ExportsOnlyMount(t *testing.T) {
	var rels []string
	var apis []goListPackage
	for _, p := range webFamily(t) {
		rels = append(rels, strings.TrimPrefix(strings.TrimPrefix(p.Path, webPkg), "/"))
		if isWebAPI(p.Path) {
			apis = append(apis, p)
		}
	}
	for _, b := range webLayoutProblems(rels, webAPIExportAllowlist, webFamilyLayout) {
		t.Error(b)
	}
	live := map[string]bool{}
	for _, p := range apis {
		rel := strings.TrimPrefix(p.Path, webPkg+"/")
		live[rel] = true
		allow, ok := webAPIExportAllowlist[rel]
		if !ok {
			continue // reported by webLayoutProblems
		}
		if len(p.Files) == 0 {
			t.Errorf("go list returned %s with no Go files: the guard would examine nothing there", p.Path)
			continue
		}
		files := map[string]any{}
		for _, f := range p.Files {
			files[filepath.Join(p.Dir, f)] = nil
		}
		exports, pkgs := exportedNames(t, files)
		if len(pkgs) != len(p.Files) {
			t.Errorf("%s: scanned %d files, go list returned %d", p.Path, len(pkgs), len(p.Files))
		}
		clause := p.Path[strings.LastIndex(p.Path, "/")+1:]
		for file, got := range pkgs {
			if got != clause {
				t.Errorf("%s (in %s): package clause %q, want %s: the guard is scanning the wrong package", file, p.Path, got, clause)
			}
		}
		for _, b := range apiExportProblems(p.Path, exports, allow) {
			t.Error(b)
		}
	}
	var missing []string
	for rel := range webAPIExportAllowlist {
		if !live[rel] {
			missing = append(missing, rel)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("webAPIExportAllowlist names api packages go list does not return: %s; a removed or renamed package takes its entry with it", strings.Join(missing, ", "))
	}
}

func TestWebAPI_ExportDetector_Discriminates(t *testing.T) {
	src := map[string]any{
		"mount.go": `package jobs
import "net/http"
func Mount(r Router, d deps) {}
func handleList() http.HandlerFunc { return nil }
type deps struct{ X int }
`,
		"registry.go": `package jobs
type JobRegistry struct{ jobs map[string]int; Public int; *Embedded; inner }
type Embedded struct{}
type inner struct{}
func (jr *JobRegistry) Get(id string) int { return 0 }
func (jr *JobRegistry) get(id string) int { return 0 }
func (d deps) Exported() {}
func NewJobRegistry() *JobRegistry { return nil }
var ErrX = 1
const Kind = "k"
var internal = 2
type Box[T any] struct{}
func (b *Box[T]) Put() {}
`,
	}
	exportKinds := func(src map[string]any) []string {
		exports, _ := exportedNames(t, src)
		var got []string
		for _, e := range exports {
			got = append(got, e.Kind+" "+e.Name)
		}
		sort.Strings(got)
		return got
	}
	want := []string{
		"type Box", "method Box.Put", "type Embedded", "var ErrX", "type JobRegistry",
		"field JobRegistry.Embedded", "method JobRegistry.Get", "field JobRegistry.Public",
		"const Kind", "func Mount", "func returning 1 NewJobRegistry",
	}
	sort.Strings(want)
	if got := exportKinds(src); !slices.Equal(got, want) {
		t.Errorf("exports\n%q\nwant\n%q", got, want)
	}
	if _, pkgs := exportedNames(t, src); pkgs["mount.go"] != "jobs" || pkgs["registry.go"] != "jobs" || len(pkgs) != 2 {
		t.Errorf("pkgs %v, want both files in package jobs", pkgs)
	}

	// A Mount with results is told apart from one without, by count, named
	// results included; a method named Mount is not the package's func.
	for src, want := range map[string]string{
		"package o\nfunc Mount(r Router) {}\n":                                               "func Mount",
		"package o\nimport \"net/http\"\nfunc Mount(r Router) http.Handler { return nil }\n": "func returning 1 Mount",
		"package o\nfunc Mount(r Router) (h Handler, err error) { return }\n":                "func returning 2 Mount",
		"package o\nfunc Mount(r Router) (a, b, c int) { return }\n":                         "func returning 3 Mount",
	} {
		if got := exportKinds(map[string]any{"m.go": src}); !slices.Equal(got, []string{want}) {
			t.Errorf("%q: exports %q, want [%q]", src, got, want)
		}
	}

	// M9 (an alias of an unexported type), M10 (an unexported type embedded,
	// transitively, by pointer and generic), and M19 (methods of an unexported
	// type that nothing exported reaches, e.g. ServeHTTP).
	reached := map[string]any{"a.go": `package jobs
import "net/http"
func Mount() {}
type jobRegistry struct{ Public int; hidden int }
func (j *jobRegistry) Get() {}
func (j *jobRegistry) get() {}
type JobRegistry = jobRegistry
type base struct{ Shared int }
func (b *base) Do() {}
type deeper struct{ *base; Own int }
type Outer struct{ base; mine int }
type Outer2 struct{ *deeper }
type gen[T any] struct{ G T }
func (g gen[T]) Gen() {}
type Outer3 struct{ gen[int] }
type Shadow struct{ base; Shared string }
type handler struct{ Field int }
func (h handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {}
func (h *handler) Exported() {}
`}
	wantReached := []string{
		"func Mount",
		"type JobRegistry", "field via alias of jobRegistry JobRegistry.Public", "method via alias of jobRegistry JobRegistry.Get",
		"type Outer", "field promoted from base Outer.Shared", "method promoted from base Outer.Do",
		"type Outer2", "field promoted from deeper Outer2.Own", "field promoted from base Outer2.Shared", "method promoted from base Outer2.Do",
		"type Outer3", "field promoted from gen Outer3.G", "method promoted from gen Outer3.Gen",
		"type Shadow", "field Shadow.Shared", "method promoted from base Shadow.Do",
	}
	sort.Strings(wantReached)
	if got := exportKinds(reached); !slices.Equal(got, wantReached) {
		t.Errorf("reached exports\n%q\nwant\n%q", got, wantReached)
	}

	// Policy, with the ruled empty lists.
	if p := apiExportProblems("jobs", []apiExport{{"Mount", "func", "a.go"}}, map[string]string{}); len(p) != 0 {
		t.Errorf("Mount only, empty list: flagged %q", p)
	}
	cases := map[string]struct {
		exports []apiExport
		allow   map[string]string
		want    string
	}{
		"second func":                {[]apiExport{{"Mount", "func", "a.go"}, {"HandleList", "func", "a.go"}}, map[string]string{}, "exports func HandleList"},
		"exported deps (M13)":        {[]apiExport{{"Mount", "func", "a.go"}, {"Deps", "type", "a.go"}}, map[string]string{}, "exports type Deps"},
		"exported registry type":     {[]apiExport{{"Mount", "func", "a.go"}, {"JobRegistry", "type", "b.go"}}, map[string]string{}, "exports type JobRegistry"},
		"no Mount":                   {[]apiExport{}, map[string]string{}, "declares no func Mount"},
		"Mount is a var":             {[]apiExport{{"Mount", "var", "a.go"}}, map[string]string{}, "Mount is a var"},
		"Mount returns a value":      {[]apiExport{{"Mount", "func returning 1", "a.go"}}, map[string]string{}, "Mount is a func returning 1 (a.go); Mount registers its routes on r and returns nothing"},
		"Mount returns two values":   {[]apiExport{{"Mount", "func returning 2", "a.go"}}, map[string]string{}, "returns nothing"},
		"reviewed entry, its method": {[]apiExport{{"Mount", "func", "a.go"}, {"T", "type", "b.go"}, {"T.Get", "method via alias of t", "b.go"}}, map[string]string{"T": "reviewed"}, "exports method via alias of t T.Get"},
		"stale entry":                {[]apiExport{{"Mount", "func", "a.go"}}, map[string]string{"T": "reviewed"}, "lists T but the package does not export it"},
		"entry with no reason":       {[]apiExport{{"Mount", "func", "a.go"}, {"T", "type", "b.go"}}, map[string]string{"T": " "}, "entry T has no reason"},
	}
	for name, c := range cases {
		p := apiExportProblems("jobs", c.exports, c.allow)
		if len(p) != 1 || !strings.Contains(p[0], c.want) {
			t.Errorf("%s: problems %q, want exactly one containing %q", name, p, c.want)
		}
	}

	// Layout (M15: a resource package outside api/).
	layout := webLayoutProblems([]string{"", "hal", "httpx", "api/kb", "api/jobs"}, webAPIExportAllowlist, webFamilyLayout)
	if len(layout) != 0 {
		t.Errorf("planned layout flagged: %q", layout)
	}
	for rel, want := range map[string]string{
		"jobs":       "a resource package goes under api/",
		"api/zz":     "not one of the planned api packages",
		"api":        "not one of the planned api packages",
		"httpx/deep": "a resource package goes under api/",
	} {
		p := webLayoutProblems([]string{"", rel}, webAPIExportAllowlist, webFamilyLayout)
		if len(p) != 1 || !strings.Contains(p[0], want) {
			t.Errorf("layout %s: problems %q, want exactly one containing %q", rel, p, want)
		}
	}
	// isWebAPI: the api tree only.
	for p, want := range map[string]bool{webAPIPrefix: true, webAPIPrefix + "/kb": true, webAPIPrefix + "x": false, webPkg + "/hal": false, webPkg: false} {
		if isWebAPI(p) != want {
			t.Errorf("isWebAPI(%s) = %v, want %v", p, !want, want)
		}
	}
}

// TestWebFamily_NoForwardsWithinFamily (guard F): no package of the web family
// (internal/edge/web and every package under it) declares a forward into
// another package of the family: a type alias, a var or const that is only
// the other package's name, or a func or method, EXPORTED OR NOT, whose body
// is one call into the other package with the parameters passed through (the
// guard P shapes, forwardsInto with strict set). A method expression of
// another family package's type counts as its name (`var repos =
// hal.URLBuilder.Repos`, `return hal.URLBuilder.Repos(b, r)`), the pointer
// form included (`var repos = (*hal.URLBuilder).Repos`, `return
// (*hal.URLBuilder).Repos(b, r)`). The case it closes is
// guard P's unexported gap, the shim a split leaves behind:
//
//	func writeStoreError(w http.ResponseWriter, r *http.Request, err error) {
//		httpx.WriteStoreError(w, r, err)
//	}
//
// The callers get rewritten to call httpx directly instead.
//
// Scope, deliberately the web family and not all of internal/: (1) for web the
// targets are every other family package, children AND siblings, because after
// the split the shim would sit in api/kb or api/manage pointing at httpx, which
// is a sibling, not a child, and guard P only looks at children; (2) widening
// the unexported check to every parent under internal/ is not trivially safe:
// it would rule on the engine and corpus trees' unexported helpers, which no
// split plan has reviewed, and a red there is a production-code change outside
// this PR's scope. A local helper that does work (adds or computes an
// argument, reorders, branches) is not a forward and is not matched.
//
// Known gaps, the shape check's and not closed here (each needs data flow):
// a parameter copied into a local before the call (`x := a; return
// httpx.F(x)`); a forward through a closure or a func-typed var (`var f =
// func(a int) error { return httpx.F(a) }`, the closure gap guard P's fact
// ba1210d3 already names); method values (`f := b.Repos; return f(r)`); named
// results assigned from the call; and calls whose arguments are wrapped or
// computed. A green run is therefore not proof that no shim exists; the
// reviewer still reads new helpers.
//
// Non-vacuity: the packages come from go list with the parent among them;
// each package's file map must hold go list's file count, its package clause,
// and its webKnownTypes each in its own file (the map forwardsInto reads); the
// detector, every file of a multi-file package included, is pinned on
// synthetic source by TestWebFamily_ForwardDetector_Discriminates.
func TestWebFamily_NoForwardsWithinFamily(t *testing.T) {
	family := webFamily(t)
	fset := token.NewFileSet()
	var offenders []string
	parsed := 0
	for _, p := range family {
		files := map[string]any{}
		for _, f := range p.Files {
			files[filepath.Join(p.Dir, f)] = nil
		}
		if len(files) == 0 {
			t.Errorf("%s: no file to parse: the scan is blind there", p.Path)
			continue
		}
		// The file map is the package's own: go list's count, the package's
		// clause, and the package's webKnownTypes each in its file (the same
		// map is what forwardsInto reads).
		_, _, scan := declaredInterfaces(t, token.NewFileSet(), files)
		if len(scan.pkgs) != len(p.Files) {
			t.Errorf("%s: scanned %d files, go list returned %d", p.Path, len(scan.pkgs), len(p.Files))
		}
		clause := p.Path[strings.LastIndex(p.Path, "/")+1:]
		for file, got := range scan.pkgs {
			if got != clause {
				t.Errorf("%s: package clause %q, want %s: the guard is scanning the wrong package", file, got, clause)
			}
		}
		for typ, want := range webKnownTypes[p.Path] {
			if got, ok := scan.types[typ]; !ok || filepath.Base(got) != want {
				t.Errorf("non-vacuity: %s: type %s not seen in %s (got %q): the scan did not read the package's files", p.Path, typ, want, got)
			}
		}
		parsed++
		self := p.Path
		isTarget := func(imp string) bool { return isWebFamily(imp) && imp != self }
		for _, h := range forwardsInto(t, fset, isTarget, files, true) {
			where := fmt.Sprintf("%s: %s %s", p.Path, h.Kind, h.Name)
			if h.Line > 0 {
				where += fmt.Sprintf(" (%s:%d) -> %s", filepath.Base(h.File), h.Line, strings.TrimPrefix(h.Child, "knomit/"))
			} else {
				where += fmt.Sprintf(" (%s) -> %s", filepath.Base(h.File), h.Detail)
			}
			offenders = append(offenders, where)
		}
	}
	if parsed != len(family) {
		t.Errorf("parsed %d of the %d web-family packages go list returned", parsed, len(family))
	}
	t.Logf("scanned %d web-family packages", parsed)
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("web-family packages forward another family package's names: delete the shim and rewrite its callers to call the owner directly:\n  %s", strings.Join(offenders, "\n  "))
	}
}

func TestWebFamily_ForwardDetector_Discriminates(t *testing.T) {
	const self = webPkg + "/api/kb"
	isTarget := func(imp string) bool { return isWebFamily(imp) && imp != self }
	const imp = "package kb\nimport (\n\t\"net/http\"\n\t\"knomit/internal/edge/web/httpx\"\n\t\"knomit/internal/edge/web/hal\"\n\t\"knomit/internal/edge/web/api/kb/inner\"\n\t\"knomit/internal/edge/mcp\"\n)\ntype h struct{ b *hal.URLBuilder; n int }\n"
	run := func(src string) []parentFwd {
		return forwardsInto(t, token.NewFileSet(), isTarget, map[string]any{"a.go": src}, true)
	}
	flagged := map[string]struct{ src, name string }{
		"unexported sibling func":  {imp + "func writeStoreError(w http.ResponseWriter, r *http.Request, err error) { httpx.WriteStoreError(w, r, err) }\n", "writeStoreError"},
		"unexported return":        {imp + "func decode(r *http.Request, v any) error { return httpx.DecodeJSON(r, v) }\n", "decode"},
		"exported sibling func":    {imp + "func Write(w http.ResponseWriter) { hal.Write(w) }\n", "Write"},
		"unexported child func":    {imp + "func run(a int) error { return inner.Run(a) }\n", "run"},
		"unexported method":        {imp + "func (x *h) self(r *http.Request) string { return x.b.Self(r) }\n", "h.self"},
		"unexported state forward": {imp + "func (x *h) put(a int) { httpx.Put(x.n, a) }\n", "h.put"},
		"unexported alias":         {imp + "type problem = hal.Problem\n", "problem"},
		"unexported var":           {imp + "var writeProblem = hal.WriteProblem\n", "writeProblem"},
		"unexported const":         {imp + "const apiBase = httpx.APIBase\n", "apiBase"},
		"method expression call":   {imp + "func repos(b hal.URLBuilder, r string) string { return hal.URLBuilder.Repos(b, r) }\n", "repos"},
		"method expression var":    {imp + "var repos = hal.URLBuilder.Repos\n", "repos"},
		"pointer method expr call": {imp + "func repos(b *hal.URLBuilder, r string) string { return (*hal.URLBuilder).Repos(b, r) }\n", "repos"},
		"pointer method expr var":  {imp + "var repos = (*hal.URLBuilder).Repos\n", "repos"},
	}
	for name, c := range flagged {
		got := run(c.src)
		if len(got) != 1 || got[0].Name != c.name {
			t.Errorf("%s: got %+v, want exactly %s", name, got, c.name)
		}
	}
	clean := map[string]string{
		"adds an argument":       imp + "func writeNotFound(w http.ResponseWriter) { hal.WriteProblem(w, 404, \"not found\", \"\", \"\") }\n",
		"reorders":               imp + "func f(a, b int) { httpx.F(b, a) }\n",
		"does work":              imp + "func f(a int) error { if a < 0 { return nil }; return httpx.F(a) }\n",
		"outside the family":     imp + "func f(a int) error { return mcp.F(a) }\n",
		"stdlib":                 imp + "func f(w http.ResponseWriter, c int) { w.WriteHeader(c) }\n",
		"own package call":       imp + "func f(a int) error { return g(a) }\nfunc g(a int) error { return nil }\n",
		"method expr, adds":      imp + "func repos(b hal.URLBuilder) string { return hal.URLBuilder.Repos(b, \"x\") }\n",
		"method expr, other":     imp + "func f(a int) error { return mcp.T.F(a) }\n",
		"ptr method expr, adds":  imp + "func repos(b *hal.URLBuilder) string { return (*hal.URLBuilder).Repos(b, \"x\") }\n",
		"ptr method expr, other": imp + "func f(a *mcp.T) error { return (*mcp.T).F(a) }\n",
	}
	for name, src := range clean {
		if got := run(src); len(got) != 0 {
			t.Errorf("%s: wrongly flagged: %+v", name, got)
		}
	}
	// Every file is read, not the first only: the shim sits in the file that
	// sorts last.
	multi := map[string]any{
		"a.go": "package kb\nfunc ok() {}\n",
		"z.go": imp + "func writeStoreError(w http.ResponseWriter, r *http.Request, err error) { httpx.WriteStoreError(w, r, err) }\n",
	}
	if got := forwardsInto(t, token.NewFileSet(), isTarget, multi, true); len(got) != 1 || got[0].Name != "writeStoreError" || got[0].File != "z.go" {
		t.Errorf("multi-file: got %+v, want writeStoreError in z.go", got)
	}
	// A package's own import path is not a target (a package cannot import
	// itself, but the predicate must not count it), and a prefix sibling
	// (webx) is outside the family.
	if isTarget(self) || isTarget(webPkg+"x/httpx") || !isTarget(webPkg) || !isTarget(webPkg+"/httpx") {
		t.Errorf("isTarget misclassifies: self %v, webx %v, web %v, httpx %v", isTarget(self), isTarget(webPkg+"x/httpx"), isTarget(webPkg), isTarget(webPkg+"/httpx"))
	}
}
