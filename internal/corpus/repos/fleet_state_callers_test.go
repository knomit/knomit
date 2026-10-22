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

// TestFleetStateWriters_OnlyFleetCallers (G1): the six functions that write
// the fleet's control.db state are called only from the fleet files.
// recordFleetRepo decides which repository uid mounts as the fleet at boot
// (invariant 4cdcf99e): a stray caller anywhere else in the package could
// make an arbitrary repository the fleet, or move the state machine, with no
// guard noticing.
//
// The writers are package-level functions, not methods; the guard matches a
// bare identifier callee (setFleetState(db, ...)) AND a selector callee
// (x.setFleetState(...)), and in a non-fleet file any mention of a writer's
// name at all (a function value taken without a call) is a violation.
func TestFleetStateWriters_OnlyFleetCallers(t *testing.T) {
	writers := map[string]bool{
		"setFleetState": true, "recordFleetAttempt": true, "recordFleetRepo": true,
		"writeFleetCreateMarker": true, "setFleetCreateRoot": true, "finishFleetCreate": true,
	}
	// Keys are slash paths relative to this directory. The writers are
	// declared in fleet_identity.go, which also calls setFleetState from
	// finishFleetCreate.
	const declFile = "fleet_identity.go"
	allowed := map[string]bool{
		"fleet.go": true, "fleet_create.go": true, "fleet_identity.go": true,
		"fleet_master.go": true, "fleet_membership.go": true, "fleet_refusal.go": true,
	}
	var sites []string
	declared := map[string]string{}
	walked := 0
	// WALKED recursively: a call from a child directory of internal/corpus/repos
	// (keyed `dir/file.go`) is as much a violation as one in a sibling file.
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
			switch v := n.(type) {
			case *ast.FuncDecl:
				if writers[v.Name.Name] && v.Recv == nil {
					declared[v.Name.Name] = name
				}
			case *ast.CallExpr:
				callee := ""
				switch fn := v.Fun.(type) {
				case *ast.Ident:
					callee = fn.Name
				case *ast.SelectorExpr:
					callee = fn.Sel.Name
				}
				if writers[callee] {
					sites = append(sites, name+":"+callee)
				}
			case *ast.Ident:
				if writers[v.Name] && !allowed[name] {
					t.Errorf("%s mentions %s: only the fleet files may touch the control.db fleet-state writers", name, v.Name)
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
	// Each writer is still declared where this guard expects it; a rename
	// would otherwise leave the allowlist guarding a name nothing uses.
	for w := range writers {
		if declared[w] != declFile {
			t.Errorf("%s is declared in %q, want %s: renamed or moved? update this guard, do not loosen it", w, declared[w], declFile)
		}
	}
	// Each writer is still called somewhere (non-vacuity per writer).
	for w := range writers {
		n := 0
		for _, s := range sites {
			if strings.HasSuffix(s, ":"+w) {
				n++
			}
		}
		if n == 0 {
			t.Errorf("%s has no call site at all: the guard would pass vacuously for it", w)
		}
	}
	sort.Strings(sites)
	// The exact call sites (one entry per call). A new one must be a
	// deliberate edit here; so must the removal of an old one.
	want := "fleet.go:recordFleetAttempt fleet.go:setFleetState " +
		"fleet_create.go:finishFleetCreate fleet_create.go:finishFleetCreate fleet_create.go:recordFleetAttempt " +
		"fleet_create.go:recordFleetAttempt fleet_create.go:recordFleetAttempt fleet_create.go:recordFleetRepo " +
		"fleet_create.go:setFleetCreateRoot fleet_create.go:setFleetState fleet_create.go:writeFleetCreateMarker " +
		"fleet_identity.go:setFleetState fleet_master.go:recordFleetAttempt fleet_master.go:recordFleetAttempt " +
		"fleet_membership.go:recordFleetAttempt fleet_membership.go:recordFleetAttempt fleet_membership.go:recordFleetAttempt " +
		"fleet_membership.go:recordFleetRepo fleet_membership.go:setFleetState fleet_membership.go:setFleetState " +
		"fleet_membership.go:setFleetState fleet_refusal.go:recordFleetAttempt"
	if got := strings.Join(sites, " "); got != want {
		t.Fatalf("fleet-state writer call sites changed:\n got %s\nwant %s", got, want)
	}
}
