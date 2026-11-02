package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const (
	desktopDir     = "tools/desktop"
	authImportPath = "knomit/internal/foundation/auth"
)

// uiPathAnchors are the files known to carry the desktop webview's traffic:
// the /api/ reverse proxy, the Wails Stream adapter, and the supervisor's
// readiness/attach probe (which decides which server the UI talks to). They
// are the floor of the derived set (see deriveUIPathFiles), not the whole of
// it: the test fails if a rename or a broken derivation drops one of them.
var uiPathAnchors = []string{"tools/desktop/uiproxy.go", "tools/desktop/uistreams.go", "tools/desktop/supervisor.go"}

// uiUpstreamIdents are the identifiers that tie a file to the UI upstream: a
// file naming one of them builds or serves UI traffic.
var uiUpstreamIdents = map[string]bool{"uiUpstream": true, "newUpstreamTransport": true, "apiProxy": true}

// forbiddenTCP are the selector expressions that open a network connection
// other than through the local dialer: a raw dial, a TLS dial, and the http
// package-level client/transport (whose default transport dials TCP).
var forbiddenTCP = map[string]bool{
	"net.Dial": true, "net.DialTimeout": true, "net.DialTCP": true, "net.Dialer": true,
	"tls.Dial": true, "tls.DialWithDialer": true, "tls.Dialer": true,
	"http.DefaultClient": true, "http.DefaultTransport": true,
	"http.Get": true, "http.Head": true, "http.Post": true, "http.PostForm": true,
}

// deriveUIPathFiles returns, relative to root, every non-test .go file under
// root/tools/desktop (subpackages included, the embedded ui/dist excluded)
// that either imports net/http or names a UI-upstream identifier. Why this
// rule: a file that can make or serve an HTTP request is a candidate for
// carrying webview traffic, and a file that names uiUpstream,
// newUpstreamTransport or apiProxy is wired into it by construction. A new
// file on the UI path is therefore checked without anyone remembering to add
// it to a list. HTTP code under tools/desktop is therefore treated as UI path:
// legitimate non-UI HTTP (e.g. a future self-updater) belongs in a package
// outside tools/desktop, or needs an explicit, reviewed allowlist entry here.
// Files that import neither (tray icons, autostart, paths)
// cannot dial the server over HTTP and stay out, which keeps the set quiet.
func deriveUIPathFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	base := filepath.Join(root, desktopDir)
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == desktopDir+"/ui/dist" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(token.NewFileSet(), p, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("parse %s: %v", rel, perr)
		}
		hit := false
		for _, imp := range f.Imports {
			if path, _ := strconv.Unquote(imp.Path.Value); path == "net/http" {
				hit = true
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && uiUpstreamIdents[id.Name] {
				hit = true
			}
			return !hit
		})
		if hit {
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", desktopDir, err)
	}
	slices.Sort(out)
	return out
}

// uiPathResult is what checkUIPathFile learned about one file.
type uiPathResult struct {
	violations          []string
	dialsLocal          bool // calls auth.DialLocal
	usesSharedTransport bool // names newUpstreamTransport
}

// checkUIPathFile applies the UI-path rules to one parsed file:
//  1. none of forbiddenTCP is named;
//  2. every http.Transport literal sets DialContext to a func that calls
//     auth.DialLocal (new(http.Transport) and a zero-value `var t http.Transport`
//     cannot, so they are forbidden), because an unset dial is a TCP dial;
//  3. every http.Client literal sets Transport to newUpstreamTransport(...) or
//     to an http.Transport literal (which rule 2 then checks); a client
//     without one uses http.DefaultTransport. new(http.Client) and a
//     zero-value `var c http.Client` are forbidden for the same reason;
//  4. every httputil.ReverseProxy literal sets Transport with the same
//     provenance as rule 3, and httputil.NewSingleHostReverseProxy is
//     forbidden (it leaves Transport nil, so it dials TCP);
//  5. a struct field of non-pointer type http.Client, http.Transport or
//     httputil.ReverseProxy is forbidden, like the zero-value `var`.
func checkUIPathFile(fset *token.FileSet, file *ast.File) uiPathResult {
	var r uiPathResult
	pkgs := map[string]string{} // local name -> import path
	for _, imp := range file.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		name := p[strings.LastIndex(p, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		pkgs[name] = p
	}
	// typeName resolves `pkg.Name` to "<import path>.Name" ("" otherwise).
	typeName := func(e ast.Expr) string {
		sel, ok := e.(*ast.SelectorExpr)
		if !ok {
			return ""
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok || pkgs[id.Name] == "" {
			return ""
		}
		return pkgs[id.Name] + "." + sel.Sel.Name
	}
	field := func(lit *ast.CompositeLit, key string) ast.Expr {
		for _, e := range lit.Elts {
			if kv, ok := e.(*ast.KeyValueExpr); ok {
				if id, ok := kv.Key.(*ast.Ident); ok && id.Name == key {
					return kv.Value
				}
			}
		}
		return nil
	}
	contains := func(e ast.Expr, pred func(ast.Node) bool) bool {
		found := false
		if e != nil {
			ast.Inspect(e, func(n ast.Node) bool {
				if pred(n) {
					found = true
				}
				return !found
			})
		}
		return found
	}
	isDialLocal := func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		return ok && typeName(sel) == authImportPath+".DialLocal"
	}
	isSharedCall := func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return false
		}
		id, ok := c.Fun.(*ast.Ident)
		return ok && id.Name == "newUpstreamTransport"
	}
	isTransportLit := func(n ast.Node) bool {
		l, ok := n.(*ast.CompositeLit)
		return ok && typeName(l.Type) == "net/http.Transport"
	}
	bad := func(pos token.Pos, msg string) {
		r.violations = append(r.violations, fset.Position(pos).String()+": "+msg)
	}

	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.Ident:
			if n.Name == "newUpstreamTransport" {
				r.usesSharedTransport = true
			}
		case *ast.SelectorExpr:
			if isDialLocal(n) {
				r.dialsLocal = true
			}
			if id, ok := n.X.(*ast.Ident); ok && pkgs[id.Name] != "" {
				p := pkgs[id.Name]
				short := p[strings.LastIndex(p, "/")+1:] + "." + n.Sel.Name
				if forbiddenTCP[short] {
					bad(n.Pos(), short+" opens a path to the server that is not the local socket/pipe")
				}
			}
		case *ast.CompositeLit:
			switch typeName(n.Type) {
			case "net/http.Transport":
				if !contains(field(n, "DialContext"), isDialLocal) {
					bad(n.Pos(), "http.Transport literal does not set DialContext to a func calling auth.DialLocal, so it dials TCP")
				}
			case "net/http.Client":
				tr := field(n, "Transport")
				if !contains(tr, isSharedCall) && !contains(tr, isTransportLit) {
					bad(n.Pos(), "http.Client literal does not set Transport to newUpstreamTransport(...) or a checked http.Transport literal, so it uses http.DefaultTransport")
				}
			case "net/http/httputil.ReverseProxy":
				tr := field(n, "Transport")
				if !contains(tr, isSharedCall) && !contains(tr, isTransportLit) {
					bad(n.Pos(), "httputil.ReverseProxy literal does not set Transport to newUpstreamTransport(...) or a checked http.Transport literal, so it uses http.DefaultTransport")
				}
			}
		case *ast.StructType:
			for _, f := range n.Fields.List {
				switch typeName(f.Type) {
				case "net/http.Transport", "net/http.Client", "net/http/httputil.ReverseProxy":
					bad(f.Pos(), "struct field of non-pointer type "+typeName(f.Type)+" is a zero value that dials TCP")
				}
			}
		case *ast.CallExpr:
			if typeName(n.Fun) == "net/http/httputil.NewSingleHostReverseProxy" {
				bad(n.Pos(), "httputil.NewSingleHostReverseProxy leaves Transport nil (http.DefaultTransport, TCP); build &httputil.ReverseProxy{Transport: newUpstreamTransport(...)}")
			}
			if id, ok := n.Fun.(*ast.Ident); ok && id.Name == "new" && len(n.Args) == 1 {
				switch typeName(n.Args[0]) {
				case "net/http.Transport", "net/http.Client", "net/http/httputil.ReverseProxy":
					bad(n.Pos(), "new("+typeName(n.Args[0])+") has no dial set, so it dials TCP")
				}
			}
		case *ast.ValueSpec:
			switch typeName(n.Type) {
			case "net/http.Transport", "net/http.Client", "net/http/httputil.ReverseProxy":
				bad(n.Pos(), "zero-value "+typeName(n.Type)+" has no dial set, so it dials TCP")
			}
		}
		return true
	})
	return r
}

// TestDesktopUIPathNeverDialsTCP pins that the webview's API and stream traffic
// has no TCP leg. The UI path is derived (deriveUIPathFiles), not listed. No
// file in it may name a TCP-capable dial or build an http.Client,
// http.Transport or httputil.ReverseProxy that is not tied to auth.DialLocal.
// Positive controls keep the absence checks from passing on a file that dials
// nothing: uiproxy.go must call auth.DialLocal, and uistreams.go and
// supervisor.go must build their transport from newUpstreamTransport.
func TestDesktopUIPathNeverDialsTCP(t *testing.T) {
	root := moduleRoot(t)
	files := deriveUIPathFiles(t, root)
	// The derivation must still find the files the desktop is known to use: a
	// rename must move uiPathAnchors, and a broken rule cannot shrink the set
	// to nothing and pass.
	for _, rel := range uiPathAnchors {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		if !slices.Contains(files, rel) {
			t.Fatalf("derived UI-path set %v does not contain %s", files, rel)
		}
	}
	fset := token.NewFileSet()
	for _, rel := range files {
		file, err := parser.ParseFile(fset, filepath.Join(root, rel), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		res := checkUIPathFile(fset, file)
		for _, v := range res.violations {
			t.Errorf("%s: %s", rel, v)
		}
		// uiproxy.go owns the dial; uistreams.go and supervisor.go must build
		// their transport from it.
		if rel == "tools/desktop/uiproxy.go" && !res.dialsLocal {
			t.Errorf("%s never calls auth.DialLocal; the guard would pass vacuously", rel)
		}
		if (rel == "tools/desktop/uistreams.go" || rel == "tools/desktop/supervisor.go") && !res.usesSharedTransport {
			t.Errorf("%s does not use newUpstreamTransport, so its dial is unchecked", rel)
		}
	}
}

// TestUIPathChecker_RulesFire runs the checker on synthetic sources: each bad
// one must be flagged and each good one (including the real shape of
// newUpstreamTransport) must pass, so the rules cannot go quiet unnoticed.
func TestUIPathChecker_RulesFire(t *testing.T) {
	const head = `package desktop
import (
	"context"
	"net"
	"net/http"
	"net/http/httputil"
	"knomit/internal/foundation/auth"
)
var _ = context.Background
var _ net.Conn
var _ *httputil.ReverseProxy
var _ = auth.DialLocal
func newUpstreamTransport() *http.Transport { return nil }
`
	cases := []struct {
		name, body string
		wantBad    bool
	}{
		{"transport literal without dial", `var x = &http.Transport{DisableKeepAlives: true}`, true},
		{"transport literal value", `var x = http.Transport{}`, true},
		{"transport via new", `var x = new(http.Transport)`, true},
		{"zero-value transport var", `var x http.Transport`, true},
		{"transport dial not via DialLocal", `var x = &http.Transport{DialContext: (&net.Dialer{}).DialContext}`, true},
		{"client literal without transport", `var x = &http.Client{}`, true},
		{"client literal with unknown transport", `var tr http.RoundTripper
var x = &http.Client{Transport: tr}`, true},
		{"client via new", `var x = new(http.Client)`, true},
		{"zero-value client var", `var x http.Client`, true},
		{"reverse proxy without transport", `var x = &httputil.ReverseProxy{}`, true},
		{"default client", `var x = http.DefaultClient`, true},
		{"http.Get", `func f() { _, _ = http.Get("http://x") }`, true},
		{"transport via DialLocal", `var x = &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
	return auth.DialLocal(ctx, "p", 0)
}}`, false},
		{"client via shared transport", `var x = &http.Client{Transport: newUpstreamTransport()}`, false},
		{"client with checked transport literal", `var x = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
	return auth.DialLocal(ctx, "p", 0)
}}}`, false},
		{"reverse proxy with transport", `var x = &httputil.ReverseProxy{Transport: newUpstreamTransport()}`, false},
		{"single host reverse proxy", `func f() { _ = httputil.NewSingleHostReverseProxy(nil) }`, true},
		{"reverse proxy with unknown transport", `var tr http.RoundTripper
var x = &httputil.ReverseProxy{Transport: tr}`, true},
		{"reverse proxy with http.DefaultTransport", `var x = &httputil.ReverseProxy{Transport: http.DefaultTransport}`, true},
		{"non-pointer client struct field", `type s struct{ c http.Client }`, true},
		{"non-pointer transport struct field", `type s struct{ t http.Transport }`, true},
		{"reverse proxy with checked transport literal", `var x = &httputil.ReverseProxy{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
	return auth.DialLocal(ctx, "p", 0)
}}}`, false},
		{"pointer client field", `type s struct{ c *http.Client }`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "synthetic.go", head+tc.body, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			got := checkUIPathFile(fset, f).violations
			if tc.wantBad && len(got) == 0 {
				t.Errorf("expected a violation, got none")
			}
			if !tc.wantBad && len(got) != 0 {
				t.Errorf("expected no violation, got %v", got)
			}
		})
	}
}

// TestUIPathDerivation_PicksUpNewFiles checks the derivation rule on a fixture
// tree: a new non-test file that imports net/http or names the upstream types
// is included, wherever it sits under tools/desktop; tests, the embedded
// ui/dist and unrelated files are not.
func TestUIPathDerivation_PicksUpNewFiles(t *testing.T) {
	root := t.TempDir()
	write := func(rel, src string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("tools/desktop/new_http.go", "package desktop\nimport \"net/http\"\nvar _ http.Client\n")
	write("tools/desktop/new_ident.go", "package desktop\nfunc f() { _ = newUpstreamTransport }\n")
	write("tools/desktop/sub/deep.go", "package sub\nimport \"net/http\"\nvar _ http.Client\n")
	write("tools/desktop/unrelated.go", "package desktop\nvar _ = 1\n")
	write("tools/desktop/skip_test.go", "package desktop\nimport \"net/http\"\nvar _ http.Client\n")
	write("tools/desktop/ui/dist/gen.go", "package dist\nimport \"net/http\"\nvar _ http.Client\n")
	got := deriveUIPathFiles(t, root)
	want := []string{"tools/desktop/new_http.go", "tools/desktop/new_ident.go", "tools/desktop/sub/deep.go"}
	if !slices.Equal(got, want) {
		t.Errorf("derived %v, want %v", got, want)
	}
}
