package repos

import (
	"os"
	"path/filepath"
	"testing"
)

// StoredAgentID's "never creates control.db" must not rest on its os.Stat
// existence check alone (check-then-act: the file can vanish in between). The
// read-only open is what makes it structural: used on a missing file it fails
// and leaves nothing behind — no control.db, no WAL/SHM/journal sidecars.
func TestOpenControlReadOnly_NeverCreatesTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "control.db")
	db, err := openControlReadOnly(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ok, qerr := tableExists(db, "instance_identity")
	db.Close()
	if qerr == nil {
		t.Fatalf("query on a missing control.db succeeded (ok=%v); a read-only open must fail", ok)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("read-only open created %v", names)
	}
}
