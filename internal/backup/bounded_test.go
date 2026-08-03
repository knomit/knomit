package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// shrinkTimeouts makes the real bounding logic observable in seconds instead of
// minutes. It changes only the NUMBERS — every code path under test is the
// production one.
//
// CALL IT BEFORE opening a Manager. t.Cleanup runs LIFO, so a Manager opened
// first would have its Close cleanup run AFTER the restore here — closing a
// deaf agent with the production 45s grace, and turning a two-second test into
// a minute-long one (or a timeout). Every call site below opens afterwards.
func shrinkTimeouts(t *testing.T, budget, grace time.Duration) {
	t.Helper()
	oldBudget, oldDefault := methodBudget, defaultMethodBudget
	oldGrace, oldReply := shutdownGrace, closeReplyGrace
	methodBudget = map[string]time.Duration{}
	defaultMethodBudget = budget
	shutdownGrace = grace
	closeReplyGrace = grace
	t.Cleanup(func() {
		methodBudget, defaultMethodBudget = oldBudget, oldDefault
		shutdownGrace, closeReplyGrace = oldGrace, oldReply
	})
}

// TestCallsAgainstADeafAgentAreBounded is the regression guard for an
// unbounded round trip.
//
// An agent that ACCEPTS a request and never answers is not hypothetical: it is
// any agent wedged in a syscall, or one whose stderr reader stopped draining
// the pipe. Without a bound the caller waits forever — and because Track,
// Untrack and Pause hold opMu across the call, one wedged request freezes every
// mutation for EVERY database, not just the one that asked.
func TestCallsAgainstADeafAgentAreBounded(t *testing.T) {
	shrinkTimeouts(t, 500*time.Millisecond, time.Second)
	m, home := newFakeManager(t, fakeDeafAfterOpen)

	done := make(chan error, 1)
	go func() { done <- m.Track("core", filepath.Join(home, "core.db")) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Track against a deaf agent returned nil")
		}
		if !errors.Is(err, errAgentUnresponsive) {
			t.Errorf("Track = %v, want errAgentUnresponsive so the cause is legible", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Track never returned against an agent that accepts requests and never answers")
	}

	// And the manager is still usable: one wedged call is not a wedged manager.
	statusDone := make(chan []DBStatus, 1)
	go func() { statusDone <- m.Status(context.Background()) }()
	select {
	case <-statusDone:
	case <-time.After(15 * time.Second):
		t.Fatal("Status never returned either: one deaf request wedged the whole manager")
	}
}

// TestCloseAgainstADeafAgentIsBoundedAndLeavesNoOrphan is the other half, and
// the one that mattered most.
//
// cmd/serve.go closes with context.Background() and no deadline. If Close sends
// its shutdown request and waits for the reply BEFORE closing the pipe, a deaf
// agent is never killed at all — the reply never comes, the grace-then-kill is
// never reached, knomit hangs at shutdown, and the agent it was supposed to
// stop is still running and still writing to the replica prefix the next knomit
// will claim. That is the two-writers case knomit deliberately never
// auto-repairs — litestream resolves it by RESETTING the replica, discarding
// backup history — manufactured by its own shutdown path.
func TestCloseAgainstADeafAgentIsBoundedAndLeavesNoOrphan(t *testing.T) {
	shrinkTimeouts(t, 500*time.Millisecond, time.Second)
	m, _ := newFakeManager(t, fakeDeafAfterOpen)
	pid := m.cl.currentPID()
	if pid == 0 {
		t.Fatal("no agent process")
	}

	done := make(chan error, 1)
	go func() { done <- m.Close(context.Background()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Close = %v, want nil: the agent IS stopped, and an unanswered "+
				"shutdown request should not be reported as a failed shutdown", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Close never returned against a deaf agent")
	}

	deadline := time.Now().Add(5 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("agent pid %d outlived Close: an orphan is replicating to the prefix "+
				"the next knomit will claim", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestBootIsBoundedWhenTheAgentGoesDeaf: Open must not hang either. establish
// runs on the boot path with no deadline of its own, so the per-method budget
// is the only thing between a deaf child and a server that never finishes
// starting.
func TestBootIsBoundedWhenTheAgentGoesDeaf(t *testing.T) {
	shrinkTimeouts(t, 500*time.Millisecond, time.Second)

	done := make(chan error, 1)
	go func() {
		_, _, err := openFakeManager(t, fakeDeafAlways)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Open succeeded against an agent that never answers open")
		}
		if !strings.Contains(err.Error(), "backup.Open") {
			t.Errorf("Open = %v, want the failure attributed to Open", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Open never returned: boot hangs on a deaf agent")
	}
}

// TestCloseAsksTheAgentToShutDown pins the half of Close's contract that had no
// coverage — and that was, in fact, broken.
//
// Firing the shutdown request and closing the pipe on the very next line means
// the goroutine never gets scheduled before the write end goes away: the write
// fails, the error is mapped to nil, and Close returns success for every agent
// without ever having asked one. Measured 0 deliveries in 20 healthy cycles.
//
// The symptom is invisible from outside — Close still returns nil, replication
// is still correct because the EOF path performs the same final syncs — which
// is precisely why it needs a marker file to observe. Twenty cycles, because a
// scheduling bug that bites 100% of the time is still a scheduling bug and one
// run could flatter it.
func TestCloseAsksTheAgentToShutDown(t *testing.T) {
	for i := 0; i < 20; i++ {
		marker := filepath.Join(t.TempDir(), "closed")
		m, _ := newFakeManager(t, fakeNormal, fakeCloseMarkerEnv+"="+marker)
		if err := m.Close(context.Background()); err != nil {
			t.Fatalf("iteration %d: Close: %v", i, err)
		}
		if _, err := os.Stat(marker); err != nil {
			t.Fatalf("iteration %d: the agent never received the shutdown request (%v); "+
				"Close is reporting a clean shutdown it never asked for", i, err)
		}
	}
}

// TestCloseReturnsTheAgentsShutdownError is the reason delivery matters. The
// agent's reply follows its final replica syncs, so a failing sync at shutdown
// is reported HERE or nowhere — and "nowhere" means an operator is told the
// backup is current as of shutdown when it is not.
func TestCloseReturnsTheAgentsShutdownError(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "closed")
	m, _ := newFakeManager(t, fakeCloseFails, fakeCloseMarkerEnv+"="+marker)

	err := m.Close(context.Background())
	if err == nil {
		t.Fatal("Close = nil while the agent reported its shutdown had failed")
	}
	if !strings.Contains(err.Error(), "scripted final sync failure") {
		t.Errorf("Close = %v, want the agent's own message", err)
	}
	if _, serr := os.Stat(marker); serr != nil {
		t.Errorf("the shutdown request was never delivered: %v", serr)
	}
}

// TestStatusDoesNotAlarmAboutATrackThatIsStillInFlight guards the credibility
// of the alarm added for the "agent lost a database" case.
//
// Track records its entry BEFORE calling the agent, so for the duration of that
// call knomit legitimately believes in a database the agent has genuinely not
// been told about. A Status that compares the agent's reply against knomit's
// map afterwards sees exactly that gap and shouts "NOT being backed up" about a
// repo that is being tracked perfectly well. It is reachable by any ops poll
// that overlaps a repo creation, and an alarm that cries wolf is worse than no
// alarm, because it teaches people to skip the real one.
//
// The scripted agent answers with an empty list and stalls until released, so
// the reply is guaranteed to predate the Track.
func TestStatusDoesNotAlarmAboutATrackThatIsStillInFlight(t *testing.T) {
	release, unblock := releaseFile(t)
	m, home := newFakeManager(t, fakeStatusSlowAndEmpty, fakeSlowReleaseEnv+"="+release)

	statusDone := make(chan []DBStatus, 1)
	go func() { statusDone <- m.Status(context.Background()) }()
	time.Sleep(200 * time.Millisecond) // let the request reach the agent

	if err := m.Track("core", filepath.Join(home, "core.db")); err != nil {
		t.Fatalf("Track: %v", err)
	}
	unblock()

	for _, st := range <-statusDone {
		if st.Name == "core" && st.LastError != "" {
			t.Fatalf("Status raised %q about a database whose Track began after the request went out; "+
				"an alarm that fires on an ordinary repo creation will be ignored when it matters", st.LastError)
		}
	}
}

// TestStatusDoesNotAlarmAboutADatabaseReTrackedDuringTheCall is the same
// property for the case a pending flag alone cannot catch.
//
// A Pause resume untracks and fully re-tracks a name. If both halves land while
// a Status round trip is in flight, the entry is SETTLED again by the time the
// reply arrives — not pending — and the agent's older reply could not possibly
// have mentioned it. The seq stamp is what distinguishes "the agent should have
// known about this" from "this changed while I was asking".
func TestStatusDoesNotAlarmAboutADatabaseReTrackedDuringTheCall(t *testing.T) {
	release, unblock := releaseFile(t)
	m, home := newFakeManager(t, fakeStatusSlowAndEmpty, fakeSlowReleaseEnv+"="+release)
	dbPath := filepath.Join(home, "core.db")
	if err := m.Track("core", dbPath); err != nil {
		t.Fatalf("Track: %v", err)
	}

	statusDone := make(chan []DBStatus, 1)
	go func() { statusDone <- m.Status(context.Background()) }()
	time.Sleep(200 * time.Millisecond)

	// Exactly what a Pause resume does, start to finish, inside the window.
	if err := m.Untrack("core"); err != nil {
		t.Fatalf("Untrack: %v", err)
	}
	if err := m.Track("core", dbPath); err != nil {
		t.Fatalf("re-Track: %v", err)
	}
	unblock()

	for _, st := range <-statusDone {
		if st.Name == "core" && st.LastError != "" {
			t.Fatalf("Status raised %q about a database re-tracked while the request was in flight: %s",
				st.LastError, "the reply predates the re-track, so it could not have mentioned it")
		}
	}
}

// TestSlowStatusDoesNotBlockATrack pins the CLIENT's lock discipline — the
// property the agent-side TestStatusDoesNotHoldLockAcrossNetworkCall pins for
// the agent.
//
// Status drives one remote LIST per tracked database inside the agent, so it is
// the client call most likely to stall. A client that held m.mu across the
// round trip would block every Track and Untrack for the duration, which on a
// stalled object store means indefinitely.
//
// The scripted agent wedges status until this test releases it, so a client
// that took the lock fails on the deadline rather than by timing luck.
func TestSlowStatusDoesNotBlockATrack(t *testing.T) {
	release, unblock := releaseFile(t)
	m, home := newFakeManager(t, fakeSlowStatus, fakeSlowReleaseEnv+"="+release)

	statusDone := make(chan []DBStatus, 1)
	go func() { statusDone <- m.Status(context.Background()) }()

	// Give Status time to be in flight inside the agent before probing.
	time.Sleep(200 * time.Millisecond)

	trackDone := make(chan error, 1)
	go func() { trackDone <- m.Track("core", filepath.Join(home, "core.db")) }()

	select {
	case err := <-trackDone:
		if err != nil {
			unblock()
			<-statusDone
			t.Fatalf("Track: %v", err)
		}
	case <-time.After(3 * time.Second):
		unblock()
		<-statusDone
		t.Fatal("Track blocked while a Status round trip was in flight: the client holds its lock across it")
	}

	unblock()
	<-statusDone
}

// TestStatusReportsADatabaseTheAgentDoesNotKnow closes the last place a
// database can stop replicating with nobody saying so.
//
// A track that fails during re-establishment after a crash is logged and then
// abandoned — deliberately, so one bad database does not take the rest down
// with it. But if Status is built from the agent's reply alone, that name
// simply vanishes from the report, and "not in the list" reads exactly like
// "not configured". The same silent-all-clear argument that makes an empty list
// wrong when the agent is DOWN makes an incomplete list wrong when it is up.
func TestStatusReportsADatabaseTheAgentDoesNotKnow(t *testing.T) {
	m, home := newFakeManager(t, fakeNormal)
	if err := m.Track("core", filepath.Join(home, "core.db")); err != nil {
		t.Fatalf("Track: %v", err)
	}

	// A database knomit believes in and the agent has never heard of — exactly
	// the state a failed re-establishment leaves behind.
	m.mu.Lock()
	m.dbs["orphaned"] = dbEntry{path: filepath.Join(home, "orphaned.db")}
	m.mu.Unlock()

	var found *DBStatus
	st := m.Status(context.Background())
	for i := range st {
		if st[i].Name == "orphaned" {
			found = &st[i]
		}
	}
	if found == nil {
		t.Fatalf("Status = %+v: a database the agent does not know is missing entirely, "+
			"so it has stopped being backed up with nothing to say so", st)
	}
	if found.LastError == "" {
		t.Error("the unknown database is reported as healthy")
	}
	if !strings.Contains(found.LastError, "not registered") {
		t.Errorf("LastError = %q, want it to name the cause", found.LastError)
	}
	if found.InSync {
		t.Error("the unknown database is reported as in sync")
	}
}

// TestArchivedDatabasesAreReEstablishedAfterACrash: an archived database must
// come back under the ARCHIVE store, where retention is disabled. Losing that
// distinction across a restart would turn "archive" back into "delete on a
// delay" — silently, and only after the retention window elapsed.
func TestArchivedDatabasesAreReEstablishedAfterACrash(t *testing.T) {
	m, home := newTestManager(t)
	archivePath := filepath.Join(home, "arc.db")
	makeDBWithValue(t, archivePath, "archived-content")
	if err := m.TrackArchived("arc-1", archivePath); err != nil {
		t.Fatalf("TrackArchived: %v", err)
	}
	waitInSync(t, m, ArchiveName("arc-1"))

	oldPID := m.cl.currentPID()
	killAgent(t, m)

	// Wait for the tracked set to come back, then prove it came back ARCHIVED:
	// the agent routes untrack by the flag it recorded at track time, so a
	// database re-established as live could not be untracked from the archive
	// store at all.
	waitForNewAgent(t, m, oldPID)
	waitInSync(t, m, ArchiveName("arc-1"))
	if err := m.UntrackArchived("arc-1"); err != nil {
		t.Fatalf("UntrackArchived after the crash: %v (it was re-established under the wrong store)", err)
	}
	if st := m.Status(context.Background()); len(st) != 0 {
		t.Fatalf("Status = %+v, want empty", st)
	}
}
