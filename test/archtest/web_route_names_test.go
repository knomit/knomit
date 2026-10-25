package archtest

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// routeTableGolden is the route-table golden, relative to the module root.
const routeTableGolden = "internal/edge/web/testdata/route_table.golden"

// goldenNames returns every function name the route-table golden mentions, in
// the spelling declaredFuncNames uses: each " > "-separated middleware and the
// final handler column, with the method-value decoration of a receiver
// ("(*Compressor).Handler-fm") reduced to "compressor.Handler" and the first
// letter lowered the way the golden's mwFingerprint lowers it.
func goldenNames(golden string) map[string]bool {
	out := map[string]bool{}
	for _, line := range strings.Split(golden, "\n") {
		cols := strings.Split(line, " | ")
		if len(cols) != 3 {
			continue
		}
		names := append(strings.Split(cols[1], " > "), cols[2])
		for _, n := range names {
			n = strings.TrimSuffix(strings.TrimSpace(n), "-fm")
			n = strings.NewReplacer("(*", "", ")", "").Replace(n)
			if n != "" {
				out[lowerFirst(n)] = true
			}
		}
	}
	return out
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// declaredFuncNames maps each function name declared in the given source files
// (a package path -> file name -> source) to the packages that declare it. A
// plain func is "name"; a method is "recv.Method"; the first letter is lowered
// as in the golden.
func declaredFuncNames(t *testing.T, pkgs map[string]map[string]any) map[string][]string {
	t.Helper()
	out := map[string]map[string]bool{}
	fset := token.NewFileSet()
	for pkg, files := range pkgs {
		for name, src := range files {
			f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse %s: %v", name, err)
			}
			for _, d := range f.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok {
					continue
				}
				n := fd.Name.Name
				if fd.Recv != nil && len(fd.Recv.List) == 1 {
					recv := fd.Recv.List[0].Type
					if s, ok := recv.(*ast.StarExpr); ok {
						recv = s.X
					}
					if ix, ok := recv.(*ast.IndexExpr); ok {
						recv = ix.X
					}
					id, ok := recv.(*ast.Ident)
					if !ok {
						continue
					}
					n = id.Name + "." + n
				}
				n = lowerFirst(n)
				if out[n] == nil {
					out[n] = map[string]bool{}
				}
				out[n][pkg] = true
			}
		}
	}
	res := map[string][]string{}
	for n, ps := range out {
		for p := range ps {
			res[n] = append(res[n], p)
		}
		sort.Strings(res[n])
	}
	return res
}

// sameNameProblems lists every golden name declared in more than one package.
func sameNameProblems(golden map[string]bool, declared map[string][]string) []string {
	var out []string
	for n := range golden {
		if ps := declared[n]; len(ps) > 1 {
			out = append(out, fmt.Sprintf("%s is declared in %s", n, strings.Join(ps, ", ")))
		}
	}
	sort.Strings(out)
	return out
}

// TestWebFamily_RouteTableNamesAreUnique closes the gap the route-table golden
// leaves (mwFingerprint drops the package and the export case): a handler or
// middleware name that appears in testdata/route_table.golden must be declared
// in exactly one package of the web family (internal/edge/web and everything
// under it, non-test files). Otherwise a route's handler can be swapped for a
// same-named one from another package and the golden stays byte-identical.
//
// Known gaps: names are compared by declaration (package-level funcs and
// "recv.Method"), not by what a route actually resolves to, and a same name
// declared outside the web family is not seen. Test files are not scanned.
//
// Non-vacuity: a fixed sample of golden names must be present and declared in
// exactly one family package, and the detector is pinned on synthetic source
// by TestWebFamily_RouteNameDetector_Discriminates.
func TestWebFamily_RouteTableNamesAreUnique(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(moduleRoot(t), routeTableGolden))
	if err != nil {
		t.Fatal(err)
	}
	golden := goldenNames(string(raw))
	pkgs := map[string]map[string]any{}
	for _, p := range webFamily(t) {
		files := map[string]any{}
		for _, f := range p.Files {
			files[filepath.Join(p.Dir, f)] = nil
		}
		pkgs[strings.TrimPrefix(p.Path, "knomit/")] = files
	}
	declared := declaredFuncNames(t, pkgs)
	// Positive control: names the golden holds that the scan must see once.
	for _, n := range []string{"handleFactDelete", "authMiddleware"} {
		if !golden[n] {
			t.Errorf("non-vacuity: golden names do not include %s (%d names): the golden parse is blind", n, len(golden))
		}
		if got := len(declared[n]); got != 1 {
			t.Errorf("non-vacuity: %s declared in %d web-family packages %v, want exactly 1: the scan is blind, the name moved, or it is now declared twice", n, got, declared[n])
		}
	}
	for _, b := range sameNameProblems(golden, declared) {
		t.Errorf("route-table name %s: the golden cannot tell them apart; rename one", b)
	}
}

func TestWebFamily_RouteNameDetector_Discriminates(t *testing.T) {
	golden := goldenNames("GET /a | mw > (*Compressor).Handler-fm | HandleX\nGET /b |  | handleY\n")
	for _, n := range []string{"mw", "compressor.Handler", "handleX", "handleY"} {
		if !golden[n] {
			t.Errorf("goldenNames missing %s: %v", n, golden)
		}
	}
	src := func(s string) map[string]any { return map[string]any{"a.go": "package p\n" + s} }
	decl := declaredFuncNames(t, map[string]map[string]any{
		"web/api/kb":     src("func handleX() {}\nfunc mw() {}\nfunc (c *Compressor) Handler() {}\n"),
		"web/api/manage": src("func HandleX() {}\nfunc (c Compressor) Handler() {}\n"),
		"web/httpx":      src("func handleZ() {}\nfunc handleY() {}\n"),
	})
	got := sameNameProblems(golden, decl)
	want := []string{
		"compressor.Handler is declared in web/api/kb, web/api/manage",
		"handleX is declared in web/api/kb, web/api/manage",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("problems %q, want %q", got, want)
	}
	// A name the golden does not mention is not a problem, even when duplicated.
	if p := sameNameProblems(map[string]bool{"other": true}, decl); len(p) != 0 {
		t.Errorf("unmentioned duplicates flagged: %q", p)
	}
}
