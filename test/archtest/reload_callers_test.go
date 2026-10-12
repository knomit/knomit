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

// A reload re-binds the server's ports, so it must be reachable only from
// where listener config legitimately changes: the process' own signal/event
// loop (and so the CLI and the desktop Settings, which signal it), NEVER an
// HTTP route. Every route of the app handler is also served on the mTLS
// listener and on whatever interface `http` binds, so a reload route would let
// a fleet peer or a network neighbour re-bind them (invariant a46bbb69).
//
// The pin has three parts, each with a positive control so none can pass
// vacuously:
//   - outside internal/app, the only production file that calls a method named
//     Reload or Apply in a file importing internal/app is cmd/serve.go (the
//     reload trigger's callback);
//   - inside internal/app, Apply is called only by listeners_reload.go (Reload
//     calls it) -- no handler-shaped code in app reaches it;
//   - no file of internal/web imports internal/app at all, so no handler can
//     name Listeners.

func prodGoFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	for _, v := range []struct{ tags, goos string }{{"", ""}, {"desktop", ""}, {"", "windows"}, {"desktop", "windows"}, {"", "linux"}} {
		args := []string{"list", "-e", "-f", `{{$d := .Dir}}{{range .GoFiles}}{{$d}}/{{.}}{{"\n"}}{{end}}`}
		if v.tags != "" {
			args = append(args, "-tags", v.tags)
		}
		cmd := exec.Command("go", append(args, "knomit/...")...)
		if v.goos != "" {
			cmd.Env = append(os.Environ(), "GOOS="+v.goos)
		}
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("go list: %v", err)
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
	return files
}

func TestListenersApplyHasOnlyTheSignalLoopAsCaller(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()
	var outsideCallers, insideApplyCallers, webFiles, webImportsApp []string
	for _, f := range prodGoFiles(t, root) {
		file, err := parser.ParseFile(fset, f, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		rel, _ := filepath.Rel(root, f)
		rel = filepath.ToSlash(rel)
		importsApp := false
		for _, imp := range file.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == "knomit/internal/app" {
				importsApp = true
			}
		}
		inApp := strings.HasPrefix(rel, "internal/app/")
		if strings.HasPrefix(rel, "internal/web/") {
			webFiles = append(webFiles, rel)
			if importsApp {
				webImportsApp = append(webImportsApp, rel)
			}
		}
		if !importsApp && !inApp {
			continue
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch {
			case inApp && sel.Sel.Name == "Apply":
				insideApplyCallers = append(insideApplyCallers, rel)
			case !inApp && (sel.Sel.Name == "Reload" || sel.Sel.Name == "Apply"):
				outsideCallers = append(outsideCallers, rel)
			}
			return true
		})
	}
	slices.Sort(outsideCallers)
	outsideCallers = slices.Compact(outsideCallers)
	slices.Sort(insideApplyCallers)
	insideApplyCallers = slices.Compact(insideApplyCallers)

	if want := []string{"cmd/serve.go"}; !slices.Equal(outsideCallers, want) {
		t.Fatalf("Reload/Apply is called (in files importing internal/app) from %v; only %v may: a reload must be reachable from the signal loop alone, never from a route", outsideCallers, want)
	}
	if want := []string{"internal/app/listeners_reload.go"}; !slices.Equal(insideApplyCallers, want) {
		t.Fatalf("Apply is called inside internal/app from %v; only %v may", insideApplyCallers, want)
	}
	if len(webFiles) < 20 {
		t.Fatalf("parsed only %d internal/web files; the import check would pass vacuously", len(webFiles))
	}
	if len(webImportsApp) != 0 {
		t.Fatalf("internal/web files import internal/app: %v -- a handler that can name Listeners can reload it", webImportsApp)
	}
}
