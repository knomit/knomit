package origins

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"knomit/internal/engine/store"
	"knomit/internal/foundation/pki"
)

// TestOriginSession_RemoteBranchSurvivesGet regression-tests PR #61 review
// finding #1: the user's chosen upstream branch (set by /apply) was lost
// before /commit could read it, because originSession did not carry a
// dedicated field. Both handlers now read/write originSession.RemoteBranch.
//
// This test pins the data-model contract: writing RemoteBranch on the
// session and looking it back up via the manager must round-trip the value.
func TestOriginSession_RemoteBranchSurvivesGet(t *testing.T) {
	sm := newTestSessionManager(t)

	sess, err := sm.Create("alpha", "https://example.com/repo.git", authConfig{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	sess.mu.Lock()
	sess.RemoteBranch = "master"
	sess.mu.Unlock()

	got, ok := sm.Get("alpha", sess.ID)
	if !ok {
		t.Fatalf("session not retrievable by id")
	}
	if got.RemoteBranch != "master" {
		t.Errorf("RemoteBranch: got %q, want %q", got.RemoteBranch, "master")
	}
}

// The idle sweeper runs only while the manager holds a session (the LIFETIME
// note on sessionManager): a new manager owns no goroutine, Create starts one,
// the sweeper's exit check keeps it while a session is held and stops it once
// none is, and the next Create starts it again.
//
// SABOTAGE: drop sm.ensureSweeperLocked() from Create, or make stopIfEmpty
// report true with a session held -> red.
func TestSessionManager_SweeperRunsOnlyWhileSessionsExist(t *testing.T) {
	sweeping := func(sm *sessionManager) bool {
		sm.mu.Lock()
		defer sm.mu.Unlock()
		return sm.sweeping
	}
	sm := newTestSessionManager(t)
	if sweeping(sm) {
		t.Fatal("a new manager is sweeping: it owns a goroutine before any session exists")
	}
	sess, err := sm.Create("alpha", "https://example.com/repo.git", authConfig{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !sweeping(sm) {
		t.Fatal("Create did not start the sweeper")
	}
	if sm.stopIfEmpty() || !sweeping(sm) {
		t.Fatal("the sweeper would stop with a session held")
	}
	sm.Delete("alpha", sess.ID)
	if !sm.stopIfEmpty() || sweeping(sm) {
		t.Fatal("the sweeper would keep running with no session held")
	}
	if _, err := sm.Create("alpha", "https://example.com/repo.git", authConfig{}); err != nil {
		t.Fatalf("create again: %v", err)
	}
	if !sweeping(sm) {
		t.Fatal("Create after the sweeper stopped did not start it again")
	}
}

// fakeClock is a settable clock for sessionManager.now.
type fakeClock struct{ ns atomic.Int64 }

func newFakeClock() *fakeClock {
	c := &fakeClock{}
	c.ns.Store(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
	return c
}
func (c *fakeClock) now() time.Time          { return time.Unix(0, c.ns.Load()) }
func (c *fakeClock) advance(d time.Duration) { c.ns.Add(int64(d)) }

func exists(t *testing.T, p string) bool {
	t.Helper()
	_, err := os.Stat(p)
	if err == nil {
		return true
	}
	if !os.IsNotExist(err) {
		t.Fatalf("stat %s: %v", p, err)
	}
	return false
}

// eventually fails t unless cond turns true within 10s.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(time.Millisecond)
	}
}

// runOf returns sm's run directory and lock file path ("" before the first
// Create).
func runOf(sm *sessionManager) (run, lock string) {
	sm.runMu.Lock()
	defer sm.runMu.Unlock()
	if sm.runLock != nil {
		lock = sm.runLock.Name()
	}
	return sm.run, lock
}

// sessionDirsOnDisk lists the session folders in run.
func sessionDirsOnDisk(t *testing.T, run string) []string {
	t.Helper()
	entries, err := os.ReadDir(run)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read %s: %v", run, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), sessionDirPrefix) {
			out = append(out, filepath.Join(run, e.Name()))
		}
	}
	sort.Strings(out)
	return out
}

// S6: a session is evicted, with its folder, once it has been idle for MORE
// than sessionExpiry (10 minutes), and not before; a Get resets the idle
// clock. Driven by an injected clock, not real time: the sweep is run by hand
// at each instant, and then once by the sweeper itself on a 1ms tick.
//
// SABOTAGE: make runCleanup use sessionExpiry/2, or drop the LastAccess touch
// from Get -> red.
func TestSessionManager_S6_IdleExpiryAfterTenMinutes(t *testing.T) {
	if sessionExpiry != 10*time.Minute {
		t.Fatalf("sessionExpiry = %v, want 10m", sessionExpiry)
	}
	clk := newFakeClock()
	sm := newTestSessionManager(t)
	sm.now = clk.now
	sm.interval = time.Hour // the hand-driven half: the sweeper never ticks

	s, err := sm.Create("alpha", "https://example.com/repo.git", authConfig{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	clk.advance(sessionExpiry) // idle for exactly 10m: not MORE than 10m
	sm.runCleanup()
	if _, ok := sm.Get("alpha", s.ID); !ok {
		t.Fatal("evicted at exactly 10 minutes idle; expiry is idle for more than 10 minutes")
	}
	// That Get touched the session, so its idle clock restarted now.
	clk.advance(sessionExpiry - time.Second)
	sm.runCleanup()
	if len(sm.ListByRepo("alpha")) != 1 || !exists(t, s.TempDir) {
		t.Fatal("evicted 9m59s after a Get: Get did not reset the idle clock")
	}
	clk.advance(2 * time.Second) // 10m01s since the Get
	sm.runCleanup()
	if len(sm.ListByRepo("alpha")) != 0 {
		t.Fatal("not evicted after 10m01s idle")
	}
	if exists(t, s.TempDir) {
		t.Fatalf("evicted session's folder %s is still on disk", s.TempDir)
	}

	// The sweeper itself, on a fast tick: it evicts the idle session and,
	// with nothing left, exits.
	sm2 := newTestSessionManager(t)
	sm2.now = clk.now
	sm2.interval = time.Millisecond
	s2, err := sm2.Create("beta", "https://example.com/repo.git", authConfig{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	clk.advance(sessionExpiry + time.Second)
	eventually(t, "the sweeper did not evict an expired session and exit", func() bool {
		sm2.mu.Lock()
		defer sm2.mu.Unlock()
		return len(sm2.sessions) == 0 && !sm2.sweeping
	})
	if exists(t, s2.TempDir) {
		t.Fatalf("swept session's folder %s is still on disk", s2.TempDir)
	}
}

// When the manager's lifetime ends, every session and its folder is removed,
// then its run directory and lock file, and Create is refused; a manager
// given no base also removes the private base it made, and one given a base
// leaves the base itself.
//
// SABOTAGE: drop context.AfterFunc(ctx, sm.close) from newSessionManager, or
// the run removal from close -> red.
func TestSessionManager_LifetimeEndRemovesEverySession(t *testing.T) {
	for _, tc := range []struct {
		name string
		base func(t *testing.T) string
	}{
		{"given base", func(t *testing.T) string { return t.TempDir() }},
		{"private base", func(t *testing.T) string {
			for _, k := range []string{"TMPDIR", "TMP", "TEMP"} {
				t.Setenv(k, t.TempDir())
			}
			return ""
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sm := newSessionManager(ctx, tc.base(t))
			t.Cleanup(sm.close)
			a, err := sm.Create("alpha", "u", authConfig{})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			b, err := sm.Create("beta", "u", authConfig{})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			sm.runMu.Lock()
			base, owned := sm.base, sm.baseOwned
			sm.runMu.Unlock()
			run, lock := runOf(sm)
			if owned != (tc.name == "private base") {
				t.Fatalf("baseOwned = %v", owned)
			}
			for _, p := range []string{a.TempDir, b.TempDir, run, lock} {
				if !exists(t, p) {
					t.Fatalf("%s not made", p)
				}
			}

			cancel()
			eventually(t, "session folders, run or private base still on disk after the lifetime ended", func() bool {
				return !exists(t, a.TempDir) && !exists(t, b.TempDir) && !exists(t, run) && !exists(t, lock) &&
					(!owned || !exists(t, base))
			})
			if !owned && !exists(t, base) {
				t.Fatal("a given base was removed: the manager owns its run, not the base")
			}
			if n := len(sm.ListByRepo("alpha")) + len(sm.ListByRepo("beta")); n != 0 {
				t.Fatalf("%d sessions still held after the lifetime ended", n)
			}
			if _, err := sm.Create("alpha", "u", authConfig{}); err != errSessionsClosed {
				t.Fatalf("Create after the lifetime ended: err = %v, want errSessionsClosed", err)
			}
		})
	}
}

// released (what Mount's WaitGroup reports) is the signal an owner that removes the
// sessions dir waits on, so it may close only when the manager holds no file
// at all: at that moment, with no polling, every session store is closed, the
// run's lock file handle is closed, and nothing the manager made is on disk.
// On Windows any one still open fails the owner's removal of the dir (#649);
// here the open handle is checked directly, since unix unlinks it anyway.
//
// SABOTAGE: close released at the top of shutdown instead of the end -> red;
// close it before the lock is closed -> red (the handle is still open);
// remove `<-sm.staleDone` from shutdown -> red every run (the held lock below
// keeps the startup sweep inside its runProbeWait for 50ms, far longer than
// the rest of shutdown takes).
func TestSessionManager_ReleasedMeansNothingIsHeld(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base := t.TempDir()
	// A previous run's lock file that another handle holds: the startup sweep
	// opens it and waits runProbeWait for the lock, so it is still running
	// when the lifetime ends, whatever the scheduler does.
	held, err := os.Create(filepath.Join(base, runPrefix+"held"+runLockSuffix))
	if err != nil {
		t.Fatal(err)
	}
	if err := pki.LockFile(context.Background(), held); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })
	sm := newSessionManager(ctx, base)
	t.Cleanup(sm.close)
	s, err := sm.Create("alpha", "u", authConfig{})
	if err != nil {
		t.Fatal(err)
	}
	st := openTestStore(t, s)
	sm.runMu.Lock()
	lockFile := sm.runLock
	sm.runMu.Unlock()
	run, lock := runOf(sm)

	select {
	case <-sm.released:
		t.Fatal("released is closed while the lifetime is still live")
	default:
	}
	cancel()
	select {
	case <-sm.released:
	case <-time.After(10 * time.Second):
		t.Fatal("released not closed 10s after the lifetime ended")
	}
	if !isClosed(st) {
		t.Error("released, but a session's store is still open")
	}
	// Read, not Stat: on Windows Stat skips the closed check and asks the OS
	// about the stale handle ("The handle is invalid"); Read reports
	// os.ErrClosed on every platform, and an open, empty file gives io.EOF.
	if _, err := lockFile.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
		t.Errorf("released, but the run's lock file handle is still open (Read err = %v)", err)
	}
	for _, p := range []string{s.TempDir, run, lock} {
		if exists(t, p) {
			t.Errorf("released, but %s is still on disk", p)
		}
	}
	select {
	case <-sm.staleDone:
	default:
		t.Error("released before the startup sweep finished")
	}
}

// Two managers over ONE base — a `knomit serve` beside the desktop's, on one
// home — must not touch each other's sessions: the second's startup sweep
// leaves the first's live run, its lock and its session folder alone, because
// the first holds the run's lock.
//
// SABOTAGE: make reclaimRun ignore the lock (remove whatever it finds) -> red.
func TestSessionManager_StartupSweepLeavesAnotherLiveManagersRun(t *testing.T) {
	base := t.TempDir()
	first := newSessionManager(context.Background(), base)
	t.Cleanup(first.close)
	s1, err := first.Create("alpha", "u", authConfig{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := os.WriteFile(filepath.Join(s1.TempDir, "clone.db"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	run1, lock1 := runOf(first)

	second := newSessionManager(context.Background(), base)
	t.Cleanup(second.close)
	<-second.staleDone

	for _, p := range []string{run1, lock1, s1.TempDir, filepath.Join(s1.TempDir, "clone.db")} {
		if !exists(t, p) {
			t.Fatalf("the second manager's startup sweep removed %s, which the first (alive) still holds", p)
		}
	}
	if _, ok := first.Get("alpha", s1.ID); !ok {
		t.Fatal("the first manager lost its session")
	}
	// And the second makes a run of its own beside it.
	if _, err := second.Create("alpha", "u", authConfig{}); err != nil {
		t.Fatalf("second create: %v", err)
	}
	run2, _ := runOf(second)
	if run2 == run1 || !exists(t, run2) || !exists(t, s1.TempDir) {
		t.Fatalf("the second manager's run %q collides with or disturbs the first's %q", run2, run1)
	}
}

// A run whose owner died (its lock released, its files left — what a crash
// leaves) is reclaimed by the next manager's startup sweep; nothing outside
// the layout is touched: a non-run folder, a run folder with no lock file, a
// non-run .lock file, an old-layout session folder directly in the base, or a
// session-named folder outside the base.
//
// SABOTAGE: drop the sweep goroutine from newSessionManager, or make
// reclaimRun treat every lock as held -> red (dead run kept); drop the prefix
// or suffix check in isChild -> red (something outside the layout removed).
func TestSessionManager_StartupSweepReclaimsADeadRun(t *testing.T) {
	parent := t.TempDir()
	base := filepath.Join(parent, "origin-sessions")

	dead := newSessionManager(context.Background(), base)
	// dead's own startup sweep must be over before its run exists. A late one
	// lists the run, opens its lock file and probes it; once the "crash" below
	// frees the lock, that sweep and next's race for the same run, and on
	// Windows the handle the loser still has open makes the winner's unlink
	// of the lock file fail (seen once in CI, #649).
	<-dead.staleDone
	ds, err := dead.Create("alpha", "u", authConfig{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	deadRun, deadLock := runOf(dead)
	// The process dies: the kernel drops its lock; its files stay.
	dead.runMu.Lock()
	_ = pki.UnlockFile(dead.runLock)
	_ = dead.runLock.Close()
	dead.runLock, dead.run = nil, ""
	dead.runClosed = true
	dead.runMu.Unlock()

	keep := []string{
		filepath.Join(base, "keep-me"),
		filepath.Join(base, runPrefix+"nolock"),
		filepath.Join(base, sessionDirPrefix+"old-layout"),
		filepath.Join(parent, runPrefix+"outside"),
	}
	for _, d := range keep {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	otherLock := filepath.Join(base, "other.lock")
	if err := os.WriteFile(otherLock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	outsideLock := filepath.Join(parent, runPrefix+"outside"+runLockSuffix)
	if err := os.WriteFile(outsideLock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	keep = append(keep, otherLock, outsideLock)

	next := newSessionManager(context.Background(), base)
	t.Cleanup(next.close)
	<-next.staleDone

	for _, p := range []string{ds.TempDir, deadRun, deadLock} {
		if exists(t, p) {
			t.Errorf("the dead run's %s survived the next manager's startup sweep", p)
		}
	}
	for _, p := range keep {
		if !exists(t, p) {
			t.Errorf("%s was removed: only dead runs in the base may be", p)
		}
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
	sweeper := &runHooks{
		reclaimLocked:   func(string) { close(sweeperLocked) },
		reclaimUnlocked: func(string) { <-claimerDone }, // the claimer finishes its whole claim in the gap
	}
	var once sync.Once
	var firstLock string
	claimer := &runHooks{claimCreated: func(lockPath string) {
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
		lock, dir, claimErr = claimRun(base, claimer)
		close(claimerDone)
	}()
	<-sweeperDone
	<-claimerDone
	if claimErr != nil {
		t.Fatalf("claimRun: %v", claimErr)
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
	reclaimDeadRuns(base)
	if !exists(t, dir) || !sameFileAt(fi, lock.Name()) {
		t.Fatal("a later sweep removed the live run")
	}
}

// removeChild refuses every path that is not the layout's.
func TestSessionManager_RemovalStaysInsideTheLayout(t *testing.T) {
	base := t.TempDir()
	run := filepath.Join(base, runPrefix+"x")
	for _, p := range []string{
		filepath.Join(run, "keep-me"),
		filepath.Join(run, sessionDirPrefix+"y"),
	} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct{ parent, p, prefix, suffix string }{
		{run, filepath.Join(run, "keep-me"), sessionDirPrefix, ""},
		{run, filepath.Join(run, sessionDirPrefix+"y", ".."), sessionDirPrefix, ""}, // cleans to run
		{run, filepath.Join(run, sessionDirPrefix+"y", "z"), sessionDirPrefix, ""},  // a grandchild
		{run, run, sessionDirPrefix, ""},
		{base, run, runPrefix, runLockSuffix}, // a run dir is not a lock file
		{base, filepath.Join(base, runPrefix), runPrefix, ""},
		{"", run, runPrefix, ""},
	} {
		if removeChild(c.parent, c.p, c.prefix, c.suffix) {
			t.Errorf("removeChild(%q, %q, %q, %q) removed a path outside the layout", c.parent, c.p, c.prefix, c.suffix)
		}
	}
	if !exists(t, filepath.Join(run, "keep-me")) || !exists(t, filepath.Join(run, sessionDirPrefix+"y")) {
		t.Fatal("a refused removal removed something")
	}
}

// openTestStore opens a real store in s's folder as its RemoteStore.
func openTestStore(t *testing.T, s *originSession) *store.Service {
	t.Helper()
	st, err := store.Open(filepath.Join(s.TempDir, "clone.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	s.mu.Lock()
	s.RemoteStore = st
	s.mu.Unlock()
	return st
}

// isClosed reports whether st's database is closed.
func isClosed(st *store.Service) bool { return st.Checkpoint() != nil }

// Every path that drops a session closes its RemoteStore and removes its
// folder: Delete, replace (a new Create for the repo), expiry, lifetime end.
//
// SABOTAGE: drop the store Close from finalize -> red on every path.
func TestSessionManager_DroppingASessionClosesItsStore(t *testing.T) {
	for _, path := range []string{"delete", "replace", "expiry", "lifetime end"} {
		t.Run(path, func(t *testing.T) {
			clk := newFakeClock()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sm := newSessionManager(ctx, t.TempDir())
			t.Cleanup(sm.close)
			sm.now = clk.now
			sm.interval = time.Hour
			s, err := sm.Create("alpha", "u", authConfig{})
			if err != nil {
				t.Fatal(err)
			}
			st := openTestStore(t, s)
			if isClosed(st) {
				t.Fatal("a fresh store reads as closed: the probe is broken")
			}
			switch path {
			case "delete":
				sm.Delete("alpha", s.ID)
			case "replace":
				if _, err := sm.Create("alpha", "u", authConfig{}); err != nil {
					t.Fatal(err)
				}
			case "expiry":
				clk.advance(sessionExpiry + time.Second)
				sm.runCleanup()
			case "lifetime end":
				cancel()
			}
			eventually(t, "the dropped session's store is still open or its folder still on disk", func() bool {
				return isClosed(st) && !exists(t, s.TempDir)
			})
		})
	}
}

// S6, in use: a session whose use is held is not idle, however long the
// step takes, so the sweeper leaves it alone; and the idle clock restarts
// when the step ends, so the 10 minutes count from the release.
//
// SABOTAGE: drop the !s.inUse skip from runCleanup -> red (evicted mid-step);
// drop the LastAccess touch from release -> red (evicted right after the
// release, 30 minutes after the step began).
func TestSessionManager_S6_InUseSessionIsNotIdle(t *testing.T) {
	clk := newFakeClock()
	sm := newTestSessionManager(t)
	sm.now = clk.now
	sm.interval = time.Hour
	s, err := sm.Create("alpha", "u", authConfig{})
	if err != nil {
		t.Fatal(err)
	}
	st := openTestStore(t, s)
	_, unuse, ok := sm.Use(context.Background(), "alpha", s.ID)
	if !ok {
		t.Fatal("Use failed")
	}
	defer unuse()

	clk.advance(3 * sessionExpiry) // a 30-minute step
	sm.runCleanup()
	if len(sm.ListByRepo("alpha")) != 1 || isClosed(st) || !exists(t, s.TempDir) {
		t.Fatal("a session was idle-expired while a handler held its use")
	}

	unuse() // the step ends: the idle clock restarts here
	clk.advance(sessionExpiry - time.Second)
	sm.runCleanup()
	if len(sm.ListByRepo("alpha")) != 1 {
		t.Fatal("evicted 9m59s after its step ended: release did not restart the idle clock")
	}
	clk.advance(2 * time.Second)
	sm.runCleanup()
	if len(sm.ListByRepo("alpha")) != 0 || !isClosed(st) || exists(t, s.TempDir) {
		t.Fatal("not evicted (store closed, folder removed) 10m01s after its step ended")
	}
}

// A handler holding a session's use keeps its store open and its folder in
// place through a Delete or the lifetime's end; the drop takes effect when the
// handler releases. A new Use of a dropped session fails. (Expiry skips a
// session in use: TestSessionManager_S6_InUseSessionIsNotIdle.)
//
// SABOTAGE: make retire finalize regardless of inUse -> red.
func TestSessionManager_DropWaitsForTheHandlerUsingTheSession(t *testing.T) {
	for _, path := range []string{"delete", "lifetime end"} {
		t.Run(path, func(t *testing.T) {
			clk := newFakeClock()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sm := newSessionManager(ctx, t.TempDir())
			t.Cleanup(sm.close)
			sm.now = clk.now
			sm.interval = time.Hour
			s, err := sm.Create("alpha", "u", authConfig{})
			if err != nil {
				t.Fatal(err)
			}
			st := openTestStore(t, s)
			_, unuse, ok := sm.Use(context.Background(), "alpha", s.ID)
			if !ok {
				t.Fatal("Use failed on a live session")
			}
			defer unuse() // idempotent; keeps a failing test from blocking close at Cleanup
			switch path {
			case "delete":
				sm.Delete("alpha", s.ID)
			case "lifetime end":
				cancel()
			}
			time.Sleep(20 * time.Millisecond) // give an asynchronous drop the chance to misbehave
			if isClosed(st) || !exists(t, s.TempDir) {
				t.Fatal("the store was closed, or the folder removed, under the handler using it")
			}
			if _, ok := sm.Get("alpha", s.ID); ok {
				t.Fatal("the dropped session is still in the manager")
			}
			if _, ok := s.acquire(context.Background()); ok {
				t.Fatal("a dropped session's use was granted again")
			}
			unuse()
			eventually(t, "the drop did not take effect when the handler released the session", func() bool {
				return isClosed(st) && !exists(t, s.TempDir)
			})
		})
	}
}

// The use under concurrency: handlers using a session's store while other
// goroutines Delete, replace and expire sessions never see a closed store,
// and every store ends closed and every folder removed. Run with -race
// -count=50.
//
// SABOTAGE: make retire finalize regardless of inUse, or finalize without
// claiming finalized -> red.
func TestSessionManager_StoreNeverClosedUnderAUser(t *testing.T) {
	clk := newFakeClock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sm := newSessionManager(ctx, t.TempDir())
	sm.now = clk.now
	sm.interval = time.Hour

	var mu sync.Mutex
	var stores []*store.Service
	var wg sync.WaitGroup
	var failures atomic.Int64
	for i := 0; i < 6; i++ {
		s, err := sm.Create("alpha", "u", authConfig{})
		if err != nil {
			t.Fatal(err)
		}
		// Open the store under a use, as /test does: an earlier iteration's
		// dropper may already be expiring this very session.
		_, setup, ok := sm.Use(context.Background(), "alpha", s.ID)
		if !ok {
			continue // dropped before it could be set up
		}
		st := openTestStore(t, s)
		setup()
		mu.Lock()
		stores = append(stores, st)
		mu.Unlock()
		for u := 0; u < 2; u++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				got, unuse, ok := sm.Use(context.Background(), "alpha", s.ID)
				if !ok {
					return // already dropped: nothing to use
				}
				defer unuse()
				for k := 0; k < 5; k++ {
					got.mu.Lock()
					cur := got.RemoteStore
					got.mu.Unlock()
					if cur == nil || cur.Checkpoint() != nil {
						failures.Add(1)
					}
				}
			}()
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch i % 3 {
			case 0:
				sm.Delete("alpha", s.ID)
			case 1:
				clk.advance(sessionExpiry + time.Second)
				sm.runCleanup()
			default: // the next iteration's Create replaces it
			}
		}(i)
	}
	wg.Wait()
	if n := failures.Load(); n != 0 {
		t.Fatalf("%d uses saw their session's store closed (or gone) under them", n)
	}
	cancel()
	mu.Lock()
	defer mu.Unlock()
	eventually(t, "a store was left open after every session was dropped", func() bool {
		for _, st := range stores {
			if !isClosed(st) {
				return false
			}
		}
		return true
	})
}

// Concurrent Create, Delete and sweep never leave a session folder behind
// that no held session refers to, and never hold a session whose folder is
// gone: the folders on disk are EXACTLY the held sessions' folders, at the
// end and after the lifetime ends (none). Run with -race -count=50.
//
// The clock jumps past the expiry on every sweep, so every sweep races
// Creates that are replacing the very sessions it found expired.
//
// SABOTAGE: release mu between Create's replace and its store (the old
// deleteByRepo-then-Store race), or make runCleanup delete by key after
// re-locking (an eviction that can take a newer session) -> red.
func TestSessionManager_ConcurrentLifecycleLeavesNoOrphanFolder(t *testing.T) {
	clk := newFakeClock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sm := newSessionManager(ctx, t.TempDir())
	sm.now = clk.now
	sm.interval = time.Hour

	repos := []string{"alpha", "beta"}
	var wg, sweepers sync.WaitGroup
	stop := make(chan struct{})
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 40; i++ {
				repo := repos[(w+i)%len(repos)]
				s, err := sm.Create(repo, "u", authConfig{})
				if err != nil {
					t.Errorf("create: %v", err)
					return
				}
				if i%3 == 0 {
					sm.Delete(repo, s.ID)
				}
				sm.Get(repo, s.ID)
			}
		}(w)
	}
	// The sweepers run for as long as the workers do, so every Create and
	// Delete overlaps sweeps.
	var sweeps atomic.Int64
	for w := 0; w < 2; w++ {
		sweepers.Add(1)
		go func() {
			defer sweepers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				clk.advance(sessionExpiry + time.Second)
				sm.runCleanup()
				sweeps.Add(1)
			}
		}()
	}
	wg.Wait()
	close(stop)
	sweepers.Wait()
	if sweeps.Load() == 0 {
		t.Fatal("no sweep ran: the test raced nothing")
	}

	run, _ := runOf(sm)
	sm.mu.Lock()
	var held []string
	for _, s := range sm.sessions {
		held = append(held, s.TempDir)
	}
	sm.mu.Unlock()
	sort.Strings(held)
	onDisk := sessionDirsOnDisk(t, run)
	if strings.Join(onDisk, "\n") != strings.Join(held, "\n") {
		t.Fatalf("session folders on disk differ from the held sessions' folders\non disk (%d):\n%s\nheld (%d):\n%s",
			len(onDisk), strings.Join(onDisk, "\n"), len(held), strings.Join(held, "\n"))
	}

	cancel()
	eventually(t, "session folders or the run left after the lifetime ended", func() bool {
		return len(sessionDirsOnDisk(t, run)) == 0 && !exists(t, run)
	})
}

// /test runs once per session: a second /test on a tested session is refused
// with a 409 problem document before it touches anything, so the session's
// state and store are exactly as the first /test left them and /apply still
// works. (Before the refusal, the re-run opened a second handle on clone.db,
// its clone failed on the existing one, and the session went to stateError,
// which /apply and /commit refuse.) The session's drop then closes the store.
//
// SABOTAGE: drop the refusal at the top of handleTestConnectivity -> red
// (the re-run streams and the session ends in stateError); drop the store
// Close from finalize -> red.
func TestHandleTestConnectivity_RerunIsRefusedAndLeavesTheSessionGood(t *testing.T) {
	f := newCommitFixture(t, "", nil)
	sess, err := f.sm.Create("alpha", f.remoteURL, authConfig{})
	if err != nil {
		t.Fatal(err)
	}
	runTest := func() *streamRecorder {
		rec := newStreamRecorder()
		f.s.ServeHTTP(rec, httptest.NewRequest("GET", "/repos/alpha/origin-sessions/"+sess.ID+"/test", nil))
		return rec
	}
	if rec := runTest(); !strings.Contains(rec.body(), `"phase":"done"`) {
		t.Fatalf("first /test did not complete: %s", rec.body())
	}
	sess.mu.Lock()
	first, stateBefore := sess.RemoteStore, sess.State
	sess.mu.Unlock()
	if first == nil || isClosed(first) || stateBefore != stateTested {
		t.Fatalf("first /test left store=%v state=%q, want an open store and %q", first, stateBefore, stateTested)
	}

	rec := runTest()
	if rec.Code != 409 || !strings.Contains(rec.Header().Get("Content-Type"), "problem+json") {
		t.Fatalf("second /test: status %d, content type %q, want a 409 problem document; body %s",
			rec.Code, rec.Header().Get("Content-Type"), rec.body())
	}
	sess.mu.Lock()
	still, stateAfter := sess.RemoteStore, sess.State
	sess.mu.Unlock()
	if still != first || isClosed(first) || stateAfter != stateBefore {
		t.Fatalf("the refused re-run changed the session: store replaced or closed=%v, state %q -> %q",
			still != first || isClosed(first), stateBefore, stateAfter)
	}

	// /apply still works on the session the refusal left alone.
	apply := newStreamRecorder()
	f.s.ServeHTTP(apply, newJSONRequest("POST", "/repos/alpha/origin-sessions/"+sess.ID+"/apply",
		strings.NewReader(`{"conflict_strategy":"local_wins"}`)))
	if apply.Code != 200 || !strings.Contains(apply.body(), `"phase":"done"`) {
		t.Fatalf("/apply after the refused re-run: status %d, body %s", apply.Code, apply.body())
	}

	f.sm.Delete("alpha", sess.ID)
	if !isClosed(first) || exists(t, sess.TempDir) {
		t.Fatal("deleting the session left the first /test's store open or its folder on disk")
	}
}

// The four steps that touch a session's store or folder (test, preview,
// apply, commit) take the session's use: while another holder has it, a step
// waits, and a step whose request ends while waiting answers 404 without
// running. A step that did not take the use would run, and answer from its
// state check instead (a 409 for this fresh session) or start streaming.
//
// SABOTAGE: switch any of the four handlers back to sm.Get -> red.
func TestHandlers_StoreStepsTakeTheSessionsUse(t *testing.T) {
	f := newCommitFixture(t, "", nil)
	sess, err := f.sm.Create("alpha", f.remoteURL, authConfig{})
	if err != nil {
		t.Fatal(err)
	}
	_, unuse, ok := f.sm.Use(context.Background(), "alpha", sess.ID)
	if !ok {
		t.Fatal("Use failed")
	}
	defer unuse()
	for _, step := range []struct{ method, path string }{
		{"GET", "/test"}, {"GET", "/preview"}, {"POST", "/apply"}, {"POST", "/commit"},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		req := httptest.NewRequest(step.method, "/repos/alpha/origin-sessions/"+sess.ID+step.path, nil).WithContext(ctx)
		rec := newStreamRecorder()
		f.s.ServeHTTP(rec, req)
		cancel()
		if rec.Code != 404 {
			t.Errorf("%s %s with the use held elsewhere: status %d, want 404 (it ran without the use); body %s",
				step.method, step.path, rec.Code, rec.body())
		}
	}
}
