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

// installCallers are the ONLY production files that may call pki.InstallBundle.
// Trust material (the fleet root, this instance's certificate, the CRL) is
// never installable over a NETWORK route: every route on the app handler is
// also served on the mTLS listener and by `knomit serve`, so an install
// reachable there would be callable by remote peers (knomit#256). Only the
// local listener (the unix socket, or the named pipe on Windows) may reach
// one, and only behind RequireLocalVia. The desktop app has no install
// binding of its own: its main window joins through POST /fleet/join. The
// callers are:
//
//   - internal/repos/fleet_create.go, the fleet create. It installs this
//     instance's OWN leaf, which it has just issued from the fresh root it
//     minted, so the material never comes from a request;
//   - internal/repos/fleet_membership.go, the fleet join. It installs the
//     bundle the operator brought from the master's enroll, after checking
//     it is for THIS instance's key and agent id, and only on an instance
//     with no trust material installed; a refused join uninstalls it.
//
// Both routes, POST /api/v1/fleet/create and /join, are behind
// RequireLocalVia: neither a remote peer nor a browser on loopback TCP can
// reach them.
//
// A route-list snapshot would catch a NEW route only; this catches any
// production code, route or not, that starts calling install.
var installCallers = []string{
	"internal/repos/fleet_create.go",
	"internal/repos/fleet_membership.go",
}

// installCRLCallers are the ONLY production files that may call
// pki.InstallCRL, the CRL-only install (fleet design D13). It writes trust
// material like InstallBundle and must never be reachable over a network
// route. The one caller is the master's revoke (POST /api/v1/fleet/revoke,
// behind RequireLocalVia), which installs the CRL it just signed under its
// own installed root; the members' tick will be the next, a reviewed
// decision, not a silent new path.
var installCRLCallers = []string{
	"internal/repos/fleet_membership.go",
}

// TestInstallBundleHasOnlyTheListedCallers parses every non-test Go file of
// every package in the module — the desktop's included, which needs its
// build tag to be listed — and fails on a call to pki.InstallBundle (through
// whatever name the file imports internal/pki as) outside installCallers.
func TestInstallBundleHasOnlyTheListedCallers(t *testing.T) {
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

	pinned := map[string][]string{"InstallBundle": installCallers, "InstallCRL": installCRLCallers}
	found := map[string][]string{}
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
				if id, ok := fn.X.(*ast.Ident); ok && name != "" && id.Name == name {
					if _, ok := pinned[fn.Sel.Name]; ok {
						found[fn.Sel.Name] = append(found[fn.Sel.Name], rel)
					}
				}
			case *ast.Ident:
				if _, ok := pinned[fn.Name]; ok && inPKI {
					found[fn.Name] = append(found[fn.Name], rel)
				}
			}
			return true
		})
	}
	for fn, allowed := range pinned {
		got := found[fn]
		slices.Sort(got)
		got = slices.Compact(got)
		if !slices.Equal(got, allowed) {
			t.Errorf("pki.%s is called from %v; only %v may call it — trust material is never installable over HTTP, and each front-end is a reviewed decision", fn, got, allowed)
		}
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
