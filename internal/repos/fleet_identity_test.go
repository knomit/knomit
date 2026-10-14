package repos

import (
	"net/url"
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

// The read-only URI must be the empty-authority form file:///C:/... for a
// Windows drive path. The hand-built url.URL{Scheme: "file", Path: ...} it
// replaced rendered file://C:/..., which SQLite rejects ("invalid uri
// authority: C:"). Pinned as strings so it runs on every OS, not only on the
// windows CI leg — the same approach fileuri's own TestNew takes.
func TestControlReadOnlyDSN_Form(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"C:/Users/x/.knomit/control.db", "file:///C:/Users/x/.knomit/control.db?mode=ro&_busy_timeout=5000"},
		{"/home/a b/.knomit/control.db", "file:///home/a%20b/.knomit/control.db?mode=ro&_busy_timeout=5000"},
	} {
		got := controlReadOnlyDSN(tc.path)
		if got != tc.want {
			t.Errorf("controlReadOnlyDSN(%q) = %q, want %q", tc.path, got, tc.want)
		}
		u, err := url.Parse(got)
		if err != nil {
			t.Fatalf("url.Parse(%q): %v", got, err)
		}
		if u.Host != "" || u.Query().Get("mode") != "ro" {
			t.Errorf("%q: authority %q, mode %q; want empty authority and mode=ro", got, u.Host, u.Query().Get("mode"))
		}
	}
}

// A relative path must still open read-only. fileuri.New keeps a relative
// path relative (no "file:" prefix), and go-sqlite3 drops the query string of
// a non-"file:" DSN — so without the filepath.Abs in openControlReadOnly,
// mode=ro is lost and the open creates control.db in the working directory.
func TestOpenControlReadOnly_RelativePathStaysReadOnly(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	db, err := openControlReadOnly("control.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ok, qerr := tableExists(db, "instance_identity")
	db.Close()
	if qerr == nil {
		t.Fatalf("query on a missing relative control.db succeeded (ok=%v); a read-only open must fail", ok)
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
		t.Fatalf("read-only open of a relative path created %v", names)
	}
}
