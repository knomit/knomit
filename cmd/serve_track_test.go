package cmd

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"knomit/internal/backup"
)

// fakeTracker records what the boot-time loop asked to replicate, and can fail
// on a chosen name so the error path is exercised without an object store.
type fakeTracker struct {
	live     map[string]string
	archived map[string]string
	failOn   string
}

func newFakeTracker() *fakeTracker {
	return &fakeTracker{live: map[string]string{}, archived: map[string]string{}}
}

var errTrackRefused = errors.New("replica refused the database")

func (f *fakeTracker) Track(name, dbPath string) error {
	if name == f.failOn {
		return errTrackRefused
	}
	f.live[name] = dbPath
	return nil
}

func (f *fakeTracker) TrackArchived(archiveID, dbPath string) error {
	if archiveID == f.failOn {
		return errTrackRefused
	}
	f.archived[archiveID] = dbPath
	return nil
}

// fakeTargets is the database set a started repo manager would report.
type fakeTargets struct {
	open        map[string]string
	archived    map[string]string
	archivedErr error
}

func (f fakeTargets) OpenDBPaths() map[string]string { return f.open }
func (f fakeTargets) ArchivedDBPaths() (map[string]string, error) {
	return f.archived, f.archivedErr
}

func names(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestTrackForReplicationRegistersEverythingThatOpened pins the shipped
// server's replication wiring.
//
// It is the answer to a review finding rather than a routine unit test: before
// this existed, disabling the loop in serve.go entirely — so the real binary
// tracked nothing and replicated nothing — passed the whole suite, including the
// end-to-end recovery story test. That story test mirrors this loop (it must:
// neither app.New nor repos.Manager.Start does the tracking, because the desktop
// build must not replicate), so it proves the loop's SHAPE is right while
// proving nothing about the copy the server actually runs.
//
// Three things have to be true together, and a bug in any one is silent:
// control.db is replicated (it is the only record of which repos exist), every
// live repo is replicated, and every archived database still on the volume is
// re-registered under the archive namespace (otherwise an archive is replicated
// only for the lifetime of the process that made it).
func TestTrackForReplicationRegistersEverythingThatOpened(t *testing.T) {
	home := t.TempDir()
	tr := newFakeTracker()
	targets := fakeTargets{
		open:     map[string]string{"core": "/k/repos/core.db", "notes": "/k/repos/notes.db"},
		archived: map[string]string{"2abc": "/k/repos/archive/2abc.db"},
	}

	if err := trackForReplication(tr, targets, home); err != nil {
		t.Fatalf("trackForReplication: %v", err)
	}

	wantLive := []string{"control", "core", "notes"}
	if got := names(tr.live); !equal(got, wantLive) {
		t.Errorf("replicated live databases = %v, want %v", got, wantLive)
	}
	if got, want := tr.live["control"], filepath.Join(home, "control.db"); got != want {
		t.Errorf("control.db tracked at %q, want %q — the registry inside it is the only record of "+
			"which repo databases should exist after a volume is lost", got, want)
	}
	if got := tr.live["core"]; got != "/k/repos/core.db" {
		t.Errorf("core tracked at %q, want the path the manager reported", got)
	}
	if got := names(tr.archived); !equal(got, []string{"2abc"}) {
		t.Errorf("replicated archives = %v, want [2abc] — an archive that stops being tracked after a "+
			"restart makes Purge's untrack a permanent no-op", got)
	}
	// The archive must go through TrackArchived, not Track: its namespace is the
	// one with retention disabled, and under ordinary retention an archive
	// silently becomes a deletion on a delay.
	if _, wrong := tr.live["2abc"]; wrong {
		t.Error("the archived database was tracked under the LIVE namespace, where retention would expire it")
	}
}

// TestTrackForReplicationFailsTheBoot: a database that cannot be replicated must
// stop the server, not be skipped. Coming up with one database unreplicated is
// exactly the state that looks healthy and loses data.
func TestTrackForReplicationFailsTheBoot(t *testing.T) {
	targets := fakeTargets{
		open:     map[string]string{"core": "/k/repos/core.db"},
		archived: map[string]string{"2abc": "/k/repos/archive/2abc.db"},
	}
	for _, tc := range []struct{ name, failOn, wantIn string }{
		{"control.db", "control", "track control.db"},
		{"a live repo", "core", "track core"},
		{"an archived database", "2abc", "track archived 2abc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := newFakeTracker()
			tr.failOn = tc.failOn
			err := trackForReplication(tr, targets, t.TempDir())
			if err == nil {
				t.Fatalf("%s could not be replicated and the boot continued anyway", tc.name)
			}
			if !errors.Is(err, errTrackRefused) {
				t.Errorf("err = %v, does not wrap the tracker's failure", err)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("err = %q, should name what failed (%q)", err, tc.wantIn)
			}
		})
	}
}

// TestTrackForReplicationSurfacesAnArchiveListingFailure: if the archive set
// cannot be read, the boot fails rather than replicating the live repos and
// quietly leaving the archives behind.
func TestTrackForReplicationSurfacesAnArchiveListingFailure(t *testing.T) {
	sentinel := errors.New("archive dir unreadable")
	err := trackForReplication(newFakeTracker(), fakeTargets{archivedErr: sentinel}, t.TempDir())
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the listing failure to refuse the boot", err)
	}
}

// TestTrackForReplicationIsANoOpWithoutABackupManager covers the nil case that
// used to be the `if boot.Backup != nil` at the call site. Moving it in here is
// what lets serve.go call this unconditionally, so there is no condition left to
// short-circuit.
func TestTrackForReplicationIsANoOpWithoutABackupManager(t *testing.T) {
	if err := trackForReplication(nil, fakeTargets{}, t.TempDir()); err != nil {
		t.Fatalf("trackForReplication(nil) = %v, want nil — replication off is not an error", err)
	}
}

// TestReplicationTrackerForMapsNilToANilInterface pins the typed-nil trap. A nil
// *backup.Manager stored straight into the interface would make it NON-nil, so
// trackForReplication would take the replicating path and call Track on a nil
// manager during a boot with backup disabled.
func TestReplicationTrackerForMapsNilToANilInterface(t *testing.T) {
	if got := replicationTrackerFor(nil); got != nil {
		t.Errorf("replicationTrackerFor(nil) = %v, want a nil interface — a typed nil pointer in an "+
			"interface is not nil, and would send a backup-disabled boot down the replicating path", got)
	}
	// And the other direction: a real manager must survive the conversion, or
	// replication is off in every build while every test of the loop still passes.
	if got := replicationTrackerFor(&backup.Manager{}); got == nil {
		t.Error("replicationTrackerFor dropped a live backup manager; the server would replicate nothing")
	}
}

// TestServeCallsTrackForReplication is the other half of the pin, and the only
// kind of test that can be written for this call site.
//
// The behavioural tests above prove the loop is correct; nothing proves the
// server still RUNS it. Exercising serve's RunE for real is not available: it
// goes through app.New, which requires a genuine ONNX embedder and downloads a
// model, and no test in this repository calls it successfully. So the wiring is
// asserted at the source level instead — over the parsed AST rather than the
// text, so a mention in a comment or a string cannot satisfy it.
//
// This exists because the alternative was measured, not assumed: with the call
// removed, every other test in the repository still passed.
func TestServeCallsTrackForReplication(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "serve.go", nil, 0)
	if err != nil {
		t.Fatalf("parse serve.go: %v", err)
	}
	var calls int
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "trackForReplication" {
			calls++
		}
		return true
	})
	if calls != 1 {
		t.Fatalf("serve.go calls trackForReplication %d times, want exactly 1. "+
			"That call is the shipped server's ONLY replication wiring: without it knomit boots, "+
			"serves, restores from the replica on the next start — and has been replicating nothing "+
			"the entire time, with no error and no log line to say so", calls)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
