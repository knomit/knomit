package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// uiPathFiles are the files that carry the desktop webview's traffic to the
// server: the /api/ reverse proxy and the Wails Stream adapter, plus the
// supervisor's readiness/attach probe, which decides which server the UI talks
// to. They must reach the server through auth.DialLocal (the local socket /
// named pipe) and nothing else.
var uiPathFiles = []string{"tools/desktop/uiproxy.go", "tools/desktop/uistreams.go", "tools/desktop/supervisor.go"}

// forbiddenTCP are the selector expressions that open a network connection
// other than through the local dialer: a raw dial, a TLS dial, and the http
// package-level client/transport (whose default transport dials TCP).
var forbiddenTCP = map[string]bool{
	"net.Dial": true, "net.DialTimeout": true, "net.DialTCP": true, "net.Dialer": true,
	"tls.Dial": true, "tls.DialWithDialer": true, "tls.Dialer": true,
	"http.DefaultClient": true, "http.DefaultTransport": true,
	"http.Get": true, "http.Head": true, "http.Post": true, "http.PostForm": true,
}

// TestDesktopUIPathNeverDialsTCP pins that the webview's API and stream traffic
// has no TCP leg: neither UI-path file may name a TCP-capable dial, and each
// must call auth.DialLocal, directly or through newUpstreamTransport (the positive control — without it the absence
// checks would pass on a file that dials nothing at all).
func TestDesktopUIPathNeverDialsTCP(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()
	for _, rel := range uiPathFiles {
		file, err := parser.ParseFile(fset, filepath.Join(root, rel), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		pkgName := map[string]string{} // local name -> import path
		for _, imp := range file.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			name := p[strings.LastIndex(p, "/")+1:]
			if imp.Name != nil {
				name = imp.Name.Name
			}
			pkgName[name] = p
		}
		dialsLocal, usesSharedTransport := false, false
		ast.Inspect(file, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == "newUpstreamTransport" {
				usesSharedTransport = true
			}
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			imp, isPkg := pkgName[id.Name]
			if !isPkg {
				return true
			}
			short := imp[strings.LastIndex(imp, "/")+1:] + "." + sel.Sel.Name
			if forbiddenTCP[short] {
				t.Errorf("%s: %s opens a path to the server that is not the local socket/pipe", rel, short)
			}
			if imp == "knomit/internal/auth" && sel.Sel.Name == "DialLocal" {
				dialsLocal = true
			}
			return true
		})
		// uiproxy.go owns the dial; uistreams.go must build its transport from it.
		if rel == "tools/desktop/uiproxy.go" && !dialsLocal {
			t.Errorf("%s never calls auth.DialLocal; the guard would pass vacuously", rel)
		}
		if (rel == "tools/desktop/uistreams.go" || rel == "tools/desktop/supervisor.go") && !usesSharedTransport {
			t.Errorf("%s does not use newUpstreamTransport, so its dial is unchecked", rel)
		}
	}
	// The files exist and are the ones the desktop uses: a rename must move the list.
	for _, rel := range uiPathFiles {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
	}
}
