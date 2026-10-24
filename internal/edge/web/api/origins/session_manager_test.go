package origins

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

// sessionDirsOnDisk lists the session folders under root.
func sessionDirsOnDisk(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read %s: %v", root, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), sessionDirPrefix) {
			out = append(out, filepath.Join(root, e.Name()))
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
	deadline := time.Now().Add(10 * time.Second)
	for {
		sm2.mu.Lock()
		done := len(sm2.sessions) == 0 && !sm2.sweeping
		sm2.mu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the sweeper did not evict an expired session and exit")
		}
		time.Sleep(time.Millisecond)
	}
	if exists(t, s2.TempDir) {
		t.Fatalf("swept session's folder %s is still on disk", s2.TempDir)
	}
}

// When the manager's lifetime ends, every session and its folder is removed
// and Create is refused; a manager given no root also removes the private
// root it made.
//
// SABOTAGE: drop context.AfterFunc(ctx, sm.close) from newSessionManager, or
// the session-folder loop from close -> red.
func TestSessionManager_LifetimeEndRemovesEverySession(t *testing.T) {
	for _, tc := range []struct {
		name string
		root func(t *testing.T) string
	}{
		{"given root", func(t *testing.T) string { return t.TempDir() }},
		{"private root", func(t *testing.T) string {
			for _, k := range []string{"TMPDIR", "TMP", "TEMP"} {
				t.Setenv(k, t.TempDir())
			}
			return ""
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sm := newSessionManager(ctx, tc.root(t))
			t.Cleanup(sm.close)
			a, err := sm.Create("alpha", "u", authConfig{})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			b, err := sm.Create("beta", "u", authConfig{})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			sm.mu.Lock()
			root, owned := sm.root, sm.rootOwned
			sm.mu.Unlock()
			if owned != (tc.name == "private root") {
				t.Fatalf("rootOwned = %v", owned)
			}
			if !exists(t, a.TempDir) || !exists(t, b.TempDir) {
				t.Fatal("session folders not made")
			}

			cancel()
			deadline := time.Now().Add(10 * time.Second)
			for exists(t, a.TempDir) || exists(t, b.TempDir) || (owned && exists(t, root)) {
				if time.Now().After(deadline) {
					t.Fatal("session folders (or the private root) still on disk after the lifetime ended")
				}
				time.Sleep(time.Millisecond)
			}
			if !owned && !exists(t, root) {
				t.Fatal("a given root was removed: the manager owns its session folders, not the root")
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

// At construction the manager removes the session folders a previous run
// left in its root, and nothing else: not a non-session folder in the root, a
// session-named FILE, or a session-named folder outside the root.
//
// SABOTAGE: drop removeStale from newSessionManager -> red (stale folder
// kept); drop the prefix check in removeStale or isSessionDir -> red (other
// folder removed).
func TestSessionManager_RemovesStaleSessionFoldersOnlyInItsRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "origin-sessions")
	stale := filepath.Join(root, sessionDirPrefix+"old")
	other := filepath.Join(root, "keep-me")
	fileNamed := filepath.Join(root, sessionDirPrefix+"file")
	outside := filepath.Join(parent, sessionDirPrefix+"outside")
	for _, d := range []string{stale, other, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(stale, "clone.db"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileNamed, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	sm := newSessionManager(context.Background(), root)
	t.Cleanup(sm.close)

	if exists(t, stale) {
		t.Errorf("stale session folder %s survived construction", stale)
	}
	for _, p := range []string{other, fileNamed, outside} {
		if !exists(t, p) {
			t.Errorf("%s was removed: only session folders in the root may be", p)
		}
	}

	// And a removal aimed outside the root is refused.
	removeSessionDir(root, outside)
	removeSessionDir(root, filepath.Join(root, "keep-me"))
	removeSessionDir(root, filepath.Join(root, sessionDirPrefix+"x", ".."))
	if !exists(t, outside) || !exists(t, other) || !exists(t, root) {
		t.Error("removeSessionDir removed a path that is not a session folder of its root")
	}
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
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sm := newSessionManager(ctx, root)
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
	t.Logf("sweeps=%d", sweeps.Load())
	if sweeps.Load() == 0 {
		t.Fatal("no sweep ran: the test raced nothing")
	}

	sm.mu.Lock()
	var held []string
	for _, s := range sm.sessions {
		held = append(held, s.TempDir)
	}
	sm.mu.Unlock()
	sort.Strings(held)
	onDisk := sessionDirsOnDisk(t, root)
	if strings.Join(onDisk, "\n") != strings.Join(held, "\n") {
		t.Fatalf("session folders on disk differ from the held sessions' folders\non disk (%d):\n%s\nheld (%d):\n%s",
			len(onDisk), strings.Join(onDisk, "\n"), len(held), strings.Join(held, "\n"))
	}

	cancel()
	deadline := time.Now().Add(10 * time.Second)
	for len(sessionDirsOnDisk(t, root)) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("session folders left after the lifetime ended: %v", sessionDirsOnDisk(t, root))
		}
		time.Sleep(time.Millisecond)
	}
}
