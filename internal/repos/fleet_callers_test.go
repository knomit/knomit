package repos

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestFleetRecordWriters_OnlyFleetCallers (F10): the functions that write
// this instance's member record are called only from the fleet files (boot
// reconcile and register/unregister), never from a repository's open, sync or
// write path.
func TestFleetRecordWriters_OnlyFleetCallers(t *testing.T) {
	writers := map[string]bool{"writeRecord": true, "writeOwnRecord": true, "refreshOwnRecord": true, "registerOwnRecord": true, "fleetBootReconcile": true, "startFleetBootReconcile": true}
	allowed := map[string]bool{"fleet.go": true, "fleet_reconcile.go": true, "manager.go": true}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var sites []string
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				if sel, ok := c.Fun.(*ast.SelectorExpr); ok && writers[sel.Sel.Name] {
					sites = append(sites, name+":"+sel.Sel.Name)
					if !allowed[name] {
						t.Errorf("%s calls %s: only the fleet files may write the member record", name, sel.Sel.Name)
					}
				}
			}
			return true
		})
	}
	sort.Strings(sites)
	// The exact call sites: a new one (a second automatic writer) must be a
	// deliberate edit here. manager.go's one call is Start's.
	want := "fleet.go:registerOwnRecord fleet.go:writeOwnRecord fleet.go:writeOwnRecord fleet.go:writeRecord fleet.go:writeRecord " +
		"fleet_reconcile.go:fleetBootReconcile fleet_reconcile.go:refreshOwnRecord fleet_reconcile.go:writeRecord manager.go:startFleetBootReconcile"
	if got := strings.Join(sites, " "); got != want {
		t.Fatalf("member record write call sites changed:\n got %s\nwant %s", got, want)
	}
}
