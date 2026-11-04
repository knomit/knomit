package httpx

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"knomit/internal/foundation/pki"
)

func exists(t *testing.T, p string) bool {
	t.Helper()
	_, err := os.Stat(p)
	return err == nil
}

// Two runs: one alive (its lock held by AcquireRunDir), one dead (what a
// crash leaves: files, no lock). The sweep removes exactly the dead one.
//
// SABOTAGE: make reclaimRun ignore the lock -> red (the live run is removed).
func TestSweepDeadRuns_RemovesOnlyUnlocked(t *testing.T) {
	base := t.TempDir()
	live, release, err := AcquireRunDir(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := os.WriteFile(filepath.Join(live, "keep.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	deadLock, deadDir, err := ClaimRun(base, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deadDir, "gone.txt"), []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The process dies: the kernel drops the lock, the files stay.
	_ = pki.UnlockFile(deadLock)
	_ = deadLock.Close()

	n, err := SweepDeadRuns(base)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("SweepDeadRuns removed %d runs, want 1", n)
	}
	if exists(t, deadDir) || exists(t, deadDir+runLockSuffix) {
		t.Error("the dead run survived the sweep")
	}
	if !exists(t, filepath.Join(live, "keep.txt")) {
		t.Error("the sweep removed the run whose lock is held")
	}

	release()
	if exists(t, live) || exists(t, live+runLockSuffix) {
		t.Error("release left the run on disk")
	}
	release() // idempotent
}

func TestSweepDeadRuns_MissingBaseIsNotAnError(t *testing.T) {
	n, err := SweepDeadRuns(filepath.Join(t.TempDir(), "absent"))
	if err != nil || n != 0 {
		t.Fatalf("SweepDeadRuns(absent) = %d, %v; want 0, nil", n, err)
	}
}

func TestAcquireRunDir_RefusesACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	base := t.TempDir()
	if _, _, err := AcquireRunDir(ctx, base); err == nil {
		t.Fatal("a cancelled context claimed a run")
	}
	if entries, _ := os.ReadDir(base); len(entries) != 0 {
		t.Fatalf("a refused claim left %d entries", len(entries))
	}
}

// The claim/reclaim race, forced step by step with the test hooks: a claimer
// creates its lock file; a sweeper locks it first (taking it for a dead run's)
// and removes the (absent) run directory; the claimer waits on the lock; the
// sweeper releases it; the claimer then takes it, checks it and makes its run
// directory, all before the sweeper goes on. Whatever the sweeper does after
// that, the claimer's run must end up with its lock file in place, held by
// the claimer. The sweeper used to unlink the file only after releasing the
// lock, which here strips the new live run of its lock file.
//
// Unix only in effect: on Windows the claimer's open handle makes any unlink
// fail, which the test then also passes.
//
// SABOTAGE: move reclaimRun's unlink after pki.UnlockFile -> red.
func TestReclaimRun_NeverStripsANewRunOfItsLock(t *testing.T) {
	base := t.TempDir()
	sweeperLocked := make(chan struct{})
	claimerDone := make(chan struct{})
	sweeperDone := make(chan struct{})
	sweeper := &RunHooks{
		ReclaimLocked:   func(string) { close(sweeperLocked) },
		ReclaimUnlocked: func(string) { <-claimerDone }, // the claimer finishes its whole claim in the gap
	}
	var once sync.Once
	var firstLock string
	claimer := &RunHooks{ClaimCreated: func(lockPath string) {
		once.Do(func() {
			firstLock = lockPath
			go func() {
				defer close(sweeperDone)
				reclaimRun(base, lockPath, sweeper) // the sweeper, probing the brand-new file
			}()
			<-sweeperLocked // the sweeper holds the lock before the claimer tries it
		})
	}}

	var lock *os.File
	var dir string
	var claimErr error
	go func() {
		lock, dir, claimErr = ClaimRun(base, claimer)
		close(claimerDone)
	}()
	<-sweeperDone
	<-claimerDone
	if claimErr != nil {
		t.Fatalf("ClaimRun: %v", claimErr)
	}
	defer func() {
		_ = pki.UnlockFile(lock)
		_ = lock.Close()
	}()

	fi, err := lock.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if !exists(t, dir) {
		t.Fatalf("the claimed run's directory %s is missing", dir)
	}
	if !sameFileAt(fi, lock.Name()) {
		t.Fatalf("the live run %s lost its lock file to the sweeper", filepath.Base(dir))
	}
	// The race really ran: on Unix the sweeper unlinked the first file under
	// its lock, so the claimer had to start over on a new one. (On Windows the
	// claimer's open handle blocks that unlink, and it keeps the first.)
	if runtime.GOOS != "windows" && lock.Name() == firstLock {
		t.Fatalf("the claimer kept its first lock file %s: the sweeper never unlinked it under the lock", filepath.Base(firstLock))
	}
	// And the next sweep sees it as alive.
	if _, err := SweepDeadRuns(base); err != nil {
		t.Fatal(err)
	}
	if !exists(t, dir) || !sameFileAt(fi, lock.Name()) {
		t.Fatal("a later sweep removed the live run")
	}
}

// RemoveChild refuses every path that is not the layout's.
func TestRemoveChild_StaysInsideTheLayout(t *testing.T) {
	base := t.TempDir()
	run := filepath.Join(base, runPrefix+"x")
	const sessionPrefix = "session-"
	for _, p := range []string{
		filepath.Join(run, "keep-me"),
		filepath.Join(run, sessionPrefix+"y"),
	} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct{ parent, p, prefix, suffix string }{
		{run, filepath.Join(run, "keep-me"), sessionPrefix, ""},
		{run, filepath.Join(run, sessionPrefix+"y", ".."), sessionPrefix, ""}, // cleans to run
		{run, filepath.Join(run, sessionPrefix+"y", "z"), sessionPrefix, ""},  // a grandchild
		{run, run, sessionPrefix, ""},
		{base, run, runPrefix, runLockSuffix}, // a run dir is not a lock file
		{base, filepath.Join(base, runPrefix), runPrefix, ""},
		{"", run, runPrefix, ""},
	} {
		if RemoveChild(c.parent, c.p, c.prefix, c.suffix) {
			t.Errorf("RemoveChild(%q, %q, %q, %q) removed a path outside the layout", c.parent, c.p, c.prefix, c.suffix)
		}
	}
	if !exists(t, filepath.Join(run, "keep-me")) || !exists(t, filepath.Join(run, sessionPrefix+"y")) {
		t.Fatal("a refused removal removed something")
	}
}
