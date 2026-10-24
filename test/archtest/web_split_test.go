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
// the project owner ruled on 2026-10-24: each api/* package exports ONE
// Mount(r, deps) and keeps its own route table, the parent reaches the api
// packages only from router.go and server.go, and no web-family package keeps
// a local one-call shim into another one. They land before any code moves, so
// each has a detector self-test on synthetic source and a logged state for
// "the packages do not exist yet".

const webAPIPrefix = webPkg + "/api"

// isWebAPI reports whether an import path is internal/edge/web/api or a
// package under it.
func isWebAPI(p string) bool {
	return p == webAPIPrefix || strings.HasPrefix(p, webAPIPrefix+"/")
}

// webAPIImporters are the files of the parent package internal/edge/web that
// may import an api package: router.go mounts each one (api/x.Mount), and
// server.go names the field types the ruling lets an api package export
// (*jobs.JobRegistry, *origins.SessionManager).
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
// package and is mounted from router.go; server.go holds the two field types.
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
		t.Errorf("%s: only router.go (Mount) and server.go (the field types) may import an api package; route through them", b)
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

// webAPIExportAllowlist is, per api package (keyed by import path relative to
// internal/edge/web), every exported name it may declare besides Mount, with
// the reason. The ruling (2026-10-24): an api package exports ONE Mount(r,
// deps), plus the named types server.go needs as field types, listed here
// explicitly; there is no free-form allowance. A name is "T" for a type, func,
// var or const, and "T.M" / "T.F" for a method or an exported field of an
// exported type T.
//
// The entries are declared ahead of the packages, because the ruling fixes
// them now. An entry whose package go list does not return is PENDING: it
// allows nothing (there is nothing to allow), and the guard logs it. Once go
// list returns the package the entry is strict: every listed name must be
// exported there (a stale name fails), and every api package go list returns
// must have an entry (an api package the ruling did not name fails until an
// entry is added in review).
//
// Methods: none are listed. At 1a860912 no production code outside the jobs
// and origins handlers calls a JobRegistry or SessionManager method: server.go
// only names the two types as fields, app.go only constructs the session
// manager (internal/app/app.go:226 `web.NewSessionManager()`), and the job
// registry has no production constructor call at all (NewJobRegistry and
// Register/Complete/SetProgress are called only by web tests). So the methods
// stay or become unexported in the move, and the tests that call them move
// into the owning package (or use an export_test.go there). NewSessionManager
// is the one exported func beyond Mount: app.go builds Server.SessionManager
// with it, and Mount cannot, because the Server field is set by app.go before
// the router is built.
var webAPIExportAllowlist = map[string]map[string]string{
	"api/kb":     {},
	"api/manage": {},
	"api/jobs": {
		"JobRegistry": "field type of web.Server.JobRegistry (server.go)",
	},
	"api/origins": {
		"SessionManager":    "field type of web.Server.SessionManager (server.go)",
		"NewSessionManager": "internal/app/app.go constructs Server.SessionManager with it",
	},
}

// apiExport is one exported name a package declares.
type apiExport struct {
	Name string // "X", or "T.M" / "T.F" for a method or field of exported type T
	Kind string // func, type, var, const, method, field
	File string
}

// exportedNames parses files (name -> nil to read from disk, or the source
// text) and returns every exported name they declare: package-level funcs,
// types, vars and consts, methods of exported types, and exported fields of
// exported struct types (an embedded field counts by its type name). Methods
// of unexported types are not reported: no caller outside the package can
// name the type. It also returns each file's package clause.
func exportedNames(t *testing.T, files map[string]any) (out []apiExport, pkgs map[string]string) {
	t.Helper()
	pkgs = map[string]string{}
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
					out = append(out, apiExport{d.Name.Name, "func", base})
					continue
				}
				if len(d.Recv.List) != 1 {
					continue
				}
				rt := d.Recv.List[0].Type
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
				if id, ok := rt.(*ast.Ident); ok && id.IsExported() {
					out = append(out, apiExport{id.Name + "." + d.Name.Name, "method", base})
				}
			case *ast.GenDecl:
				for _, sp := range d.Specs {
					switch s := sp.(type) {
					case *ast.TypeSpec:
						if !s.Name.IsExported() {
							continue
						}
						out = append(out, apiExport{s.Name.Name, "type", base})
						st, ok := s.Type.(*ast.StructType)
						if !ok {
							continue
						}
						for _, fl := range st.Fields.List {
							names := fl.Names
							if len(names) == 0 { // embedded: the field is named by its type
								typ := fl.Type
								if x, ok := typ.(*ast.StarExpr); ok {
									typ = x.X
								}
								switch x := typ.(type) {
								case *ast.Ident:
									names = []*ast.Ident{x}
								case *ast.SelectorExpr:
									names = []*ast.Ident{x.Sel}
								}
							}
							for _, n := range names {
								if n.IsExported() {
									out = append(out, apiExport{s.Name.Name + "." + n.Name, "field", base})
								}
							}
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
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, pkgs
}

// apiExportProblems applies the ruling to one api package's exports: Mount
// must be declared, as a package-level func; every other exported name must
// be in allow; every name in allow must be exported (a stale entry fails).
func apiExportProblems(pkg string, exports []apiExport, allow map[string]string) []string {
	var out []string
	seen := map[string]bool{}
	mount := false
	for _, e := range exports {
		seen[e.Name] = true
		if e.Name == "Mount" {
			if e.Kind == "func" {
				mount = true
			} else {
				mount = true // reported here, not again as missing
				out = append(out, fmt.Sprintf("%s: Mount is a %s (%s); it must be the package's func Mount(r, deps)", pkg, e.Kind, e.File))
			}
			continue
		}
		if reason, ok := allow[e.Name]; ok {
			if strings.TrimSpace(reason) == "" {
				out = append(out, fmt.Sprintf("%s: webAPIExportAllowlist entry %s has no reason", pkg, e.Name))
			}
			continue
		}
		out = append(out, fmt.Sprintf("%s: exports %s %s (%s): an api package exports only Mount plus its webAPIExportAllowlist names; unexport it, or move it to httpx if two packages need it", pkg, e.Kind, e.Name, e.File))
	}
	if !mount {
		out = append(out, fmt.Sprintf("%s: declares no func Mount: each api package mounts its own routes through one Mount(r, deps)", pkg))
	}
	for name := range allow {
		if !seen[name] {
			out = append(out, fmt.Sprintf("%s: webAPIExportAllowlist lists %s but the package does not export it: delete the entry", pkg, name))
		}
	}
	sort.Strings(out)
	return out
}

// TestWebAPI_ExportsOnlyMount (guard M): every package under
// internal/edge/web/api (as go list returns them) exports only func Mount plus
// the names its webAPIExportAllowlist entry lists, counting funcs, types,
// vars, consts, methods of exported types and exported fields of exported
// struct types. A second exported handler constructor, a shared helper or an
// exported deps struct fails: a helper two packages need belongs in httpx,
// and a deps struct named in Mount's signature must be argued into the
// allowlist in review.
//
// Before the split creates an api package there is nothing to scan; the guard
// logs that state explicitly, and logs each allowlist entry still pending
// (declared, but its package does not exist yet). The detector and the policy
// are pinned by TestWebAPI_ExportDetector_Discriminates on synthetic source
// either way. Once a package exists: its files come from go list, the scanned
// count must equal go list's, every package clause must be the last element of
// its import path, and Mount must be found (so an empty or wrong scan fails).
func TestWebAPI_ExportsOnlyMount(t *testing.T) {
	var apis []goListPackage
	for _, p := range webFamily(t) {
		if isWebAPI(p.Path) {
			apis = append(apis, p)
		}
	}
	live := map[string]bool{}
	for _, p := range apis {
		rel := strings.TrimPrefix(p.Path, webPkg+"/")
		live[rel] = true
		allow, ok := webAPIExportAllowlist[rel]
		if !ok {
			t.Errorf("%s: no webAPIExportAllowlist entry: the ruling names api/kb, api/manage, api/jobs and api/origins; a new api package needs an entry (an empty one allows only Mount) added in review", p.Path)
			continue
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
				t.Errorf("%s/%s: package clause %q, want %s: the guard is scanning the wrong package", p.Path, file, got, clause)
			}
		}
		for _, b := range apiExportProblems(p.Path, exports, allow) {
			t.Error(b)
		}
	}
	var pending []string
	for rel := range webAPIExportAllowlist {
		if !live[rel] {
			pending = append(pending, rel)
		}
	}
	sort.Strings(pending)
	if len(apis) == 0 {
		t.Logf("no api packages yet: go list returns no package under %s; nothing to scan, the detector is pinned by TestWebAPI_ExportDetector_Discriminates", webAPIPrefix)
	}
	if len(pending) > 0 {
		t.Logf("pending webAPIExportAllowlist entries (package not created yet, so the entry allows nothing): %s", strings.Join(pending, ", "))
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
	exports, pkgs := exportedNames(t, src)
	var names []string
	for _, e := range exports {
		names = append(names, e.Kind+" "+e.Name)
	}
	want := []string{
		"type Box", "method Box.Put", "type Embedded", "var ErrX", "type JobRegistry",
		"field JobRegistry.Embedded", "method JobRegistry.Get", "field JobRegistry.Public",
		"const Kind", "func Mount", "func NewJobRegistry",
	}
	sort.Strings(want)
	got := append([]string(nil), names...)
	sort.Strings(got)
	if !slices.Equal(got, want) {
		t.Errorf("exports\n%q\nwant\n%q", got, want)
	}
	if pkgs["mount.go"] != "jobs" || pkgs["registry.go"] != "jobs" || len(pkgs) != 2 {
		t.Errorf("pkgs %v, want both files in package jobs", pkgs)
	}

	// Policy. Only Mount and the allowed type: clean.
	clean := []apiExport{{"Mount", "func", "a.go"}, {"JobRegistry", "type", "b.go"}}
	if p := apiExportProblems("jobs", clean, map[string]string{"JobRegistry": "field type"}); len(p) != 0 {
		t.Errorf("clean package flagged: %q", p)
	}
	cases := map[string]struct {
		exports []apiExport
		allow   map[string]string
		want    string
	}{
		"second func":         {[]apiExport{{"Mount", "func", "a.go"}, {"HandleList", "func", "a.go"}}, nil, "exports func HandleList"},
		"method of allowed":   {[]apiExport{{"Mount", "func", "a.go"}, {"JobRegistry", "type", "b.go"}, {"JobRegistry.Get", "method", "b.go"}}, map[string]string{"JobRegistry": "field type"}, "exports method JobRegistry.Get"},
		"field of allowed":    {[]apiExport{{"Mount", "func", "a.go"}, {"JobRegistry", "type", "b.go"}, {"JobRegistry.Public", "field", "b.go"}}, map[string]string{"JobRegistry": "field type"}, "exports field JobRegistry.Public"},
		"exported deps":       {[]apiExport{{"Mount", "func", "a.go"}, {"Deps", "type", "a.go"}}, nil, "exports type Deps"},
		"no Mount":            {[]apiExport{{"JobRegistry", "type", "b.go"}}, map[string]string{"JobRegistry": "field type"}, "declares no func Mount"},
		"Mount is a var":      {[]apiExport{{"Mount", "var", "a.go"}}, nil, "Mount is a var"},
		"stale allow entry":   {[]apiExport{{"Mount", "func", "a.go"}}, map[string]string{"JobRegistry": "field type"}, "lists JobRegistry but the package does not export it"},
		"allow with no reson": {[]apiExport{{"Mount", "func", "a.go"}, {"JobRegistry", "type", "b.go"}}, map[string]string{"JobRegistry": " "}, "entry JobRegistry has no reason"},
	}
	for name, c := range cases {
		p := apiExportProblems("jobs", c.exports, c.allow)
		if len(p) != 1 || !strings.Contains(p[0], c.want) {
			t.Errorf("%s: problems %q, want exactly one containing %q", name, p, c.want)
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
// guard P shapes, forwardsInto with unexported set). The case it closes is
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
	}
	for name, c := range flagged {
		got := run(c.src)
		if len(got) != 1 || got[0].Name != c.name {
			t.Errorf("%s: got %+v, want exactly %s", name, got, c.name)
		}
	}
	clean := map[string]string{
		"adds an argument":   imp + "func writeNotFound(w http.ResponseWriter) { hal.WriteProblem(w, 404, \"not found\", \"\", \"\") }\n",
		"reorders":           imp + "func f(a, b int) { httpx.F(b, a) }\n",
		"does work":          imp + "func f(a int) error { if a < 0 { return nil }; return httpx.F(a) }\n",
		"outside the family": imp + "func f(a int) error { return mcp.F(a) }\n",
		"stdlib":             imp + "func f(w http.ResponseWriter, c int) { w.WriteHeader(c) }\n",
		"own package call":   imp + "func f(a int) error { return g(a) }\nfunc g(a int) error { return nil }\n",
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
