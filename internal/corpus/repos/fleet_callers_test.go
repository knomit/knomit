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
// reconcile, create, join and leave), never from a repository's open, sync or
// write path.
func TestFleetRecordWriters_OnlyFleetCallers(t *testing.T) {
	writers := map[string]bool{"writeRecord": true, "writeOwnRecord": true, "refreshOwnRecord": true, "fleetBootReconcile": true, "startFleetBootReconcile": true}
	allowed := map[string]bool{"fleet.go": true, "fleet_create.go": true, "fleet_join.go": true, "fleet_leave.go": true, "fleet_reconcile.go": true, "manager.go": true}
	// WALKED, not globbed: a writer call in a child package of internal/corpus/repos
	// would otherwise be invisible. Keys are slash paths relative to this
	// directory, so `allowed` names only the top-level files and a child's file
	// is `dir/file.go`.
	var sites []string
	walked := 0
	err := filepath.WalkDir(".", func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := filepath.ToSlash(p)
		if d.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		walked++
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, src, 0)
		if err != nil {
			return err
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
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if walked <= 20 {
		t.Fatalf("the guard walked only %d files: it is not reading its subject", walked)
	}
	sort.Strings(sites)
	// The exact call sites: a new one (a second automatic writer) must be a
	// deliberate edit here. manager.go's one call is Start's.
	want := "fleet.go:writeRecord fleet_create.go:writeOwnRecord fleet_join.go:writeOwnRecord fleet_leave.go:writeOwnRecord " +
		"fleet_reconcile.go:fleetBootReconcile fleet_reconcile.go:refreshOwnRecord fleet_reconcile.go:writeRecord manager.go:startFleetBootReconcile"
	if got := strings.Join(sites, " "); got != want {
		t.Fatalf("member record write call sites changed:\n got %s\nwant %s", got, want)
	}
}
