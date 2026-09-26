package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// installCallers are the ONLY production files that may call pki.InstallBundle:
// the CLI (`knomit identity install`, run by the operator in a terminal) and
// the desktop's Settings window binding (gated to that window). Trust
// material — the fleet root, this instance's certificate, the CRL — must
// never be installable over HTTP: every route on the app handler is also
// served on the mTLS listener and by `knomit serve`, so an install route
// would be callable by remote peers (knomit#256).
//
// A route-list snapshot would catch a NEW route only; this catches any
// production code, route or not, that starts calling install.
var installCallers = []string{
	"cmd/identity.go",
	"tools/desktop/identity.go",
}

// TestInstallBundleHasOnlyTheTwoFrontEnds parses every non-test Go file of
// every package in the module — the desktop's included, which needs its
// build tag to be listed — and fails on a call to pki.InstallBundle (through
// whatever name the file imports internal/pki as) outside installCallers.
func TestInstallBundleHasOnlyTheTwoFrontEnds(t *testing.T) {
	root := moduleRoot(t)
	var files []string
	// Every tag set and GOOS the module builds for: go list reports only the
	// files the current platform compiles, and a caller in a _windows.go file
	// would otherwise be invisible from a mac.
	for _, v := range []struct{ tags, goos string }{{"", ""}, {"desktop", ""}, {"", "windows"}, {"desktop", "windows"}, {"", "linux"}} {
		tags := v.tags
		args := []string{"list", "-e", "-f", `{{$d := .Dir}}{{range .GoFiles}}{{$d}}/{{.}}{{"\n"}}{{end}}`}
		if tags != "" {
			args = append(args, "-tags", tags)
		}
		// stdout only: go list warns on stderr (e.g. about a symlinked dist/).
		cmd := exec.Command("go", append(args, "knomit/...")...)
		if v.goos != "" {
			cmd.Env = append(os.Environ(), "GOOS="+v.goos)
		}
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("go list: %v\n%s", err, stderr.String())
		}
		for _, f := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if f != "" && !slices.Contains(files, f) {
				files = append(files, f)
			}
		}
	}
	if len(files) < 100 {
		t.Fatalf("listed only %d files; the guard would pass vacuously", len(files))
	}

	var found []string
	fset := token.NewFileSet()
	for _, f := range files {
		file, err := parser.ParseFile(fset, f, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		name := ""
		for _, imp := range file.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == "knomit/internal/pki" {
				name = "pki"
				if imp.Name != nil {
					name = imp.Name.Name
				}
			}
		}
		rel, _ := filepath.Rel(root, f)
		rel = filepath.ToSlash(rel)
		inPKI := strings.HasPrefix(rel, "internal/pki/")
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fn := call.Fun.(type) {
			case *ast.SelectorExpr:
				if id, ok := fn.X.(*ast.Ident); ok && name != "" && id.Name == name && fn.Sel.Name == "InstallBundle" {
					found = append(found, rel)
				}
			case *ast.Ident:
				if inPKI && fn.Name == "InstallBundle" {
					found = append(found, rel)
				}
			}
			return true
		})
	}
	slices.Sort(found)
	found = slices.Compact(found)
	if !slices.Equal(found, installCallers) {
		t.Fatalf("pki.InstallBundle is called from %v; only %v may call it — trust material is never installable over HTTP, and each front-end is a reviewed decision", found, installCallers)
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}").Output()
	if err != nil {
		t.Fatalf("go list -m: %v", err)
	}
	return strings.TrimSpace(string(out))
}
