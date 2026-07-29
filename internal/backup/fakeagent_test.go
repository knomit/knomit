package backup

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"knomit/internal/backupproto"
	"knomit/internal/config"
)

// The fake agent is the test binary re-executed with fakeAgentEnv set (see
// TestMain). It speaks the real protocol over real pipes as a real child
// process, so everything between the client and the wire is exercised — the
// supervisor, the correlation, the framing — while the AGENT's behaviour is
// scripted rather than real.
//
// That is the point: failures like "untrack errors" or "restore takes forever"
// are trivial to script and nearly impossible to provoke reliably against
// litestream. Tests that need real replication use the real agent instead
// (see newTestManager).
const (
	// fakeNormal answers everything successfully.
	fakeNormal = "normal"
	// fakeUntrackFails answers untrack with an error, and nothing else.
	fakeUntrackFails = "untrack-fails"
	// fakeSlowOps blocks restore and untrack until a release file appears,
	// modelling an object store that has stopped answering.
	fakeSlowOps = "slow-ops"
	// fakeSlowStatus blocks STATUS until a release file appears, modelling the
	// remote LIST per database that Status drives.
	fakeSlowStatus = "slow-status"
	// fakeDeafAlways answers nothing at all, not even open, so the BOOT path is
	// bounded by the same budget every other call is.
	fakeDeafAlways = "deaf-always"
	// fakeDeafAfterOpen answers open and then NOTHING — it reads every
	// subsequent request and never replies, and it ignores stdin's EOF, so it
	// only stops when it is killed. It is the shape that makes an unbounded
	// round trip fatal.
	fakeDeafAfterOpen = "deaf-after-open"
	// fakeOversized precedes its first status response with a line past
	// backupproto.MaxLineBytes.
	fakeOversized = "oversized"
	// fakeSlowReleaseEnv names the file whose appearance releases fakeSlowOps.
	fakeSlowReleaseEnv = "KNOMIT_TEST_FAKE_RELEASE"
)

// runFakeAgent serves the protocol with scripted behaviour, then exits.
func runFakeAgent(mode string) {
	protocol := os.Stdout
	os.Stdout = os.Stderr // same hygiene rule as the real agent

	var writeMu sync.Mutex
	respond := func(resp *backupproto.Response) {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = backupproto.WriteLine(protocol, resp)
	}

	var mu sync.Mutex
	tracked := map[string]string{}
	var statusCalls int

	waitForRelease := func() {
		p := os.Getenv(fakeSlowReleaseEnv)
		if p == "" {
			return
		}
		for {
			if _, err := os.Stat(p); err == nil {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	br := bufio.NewReader(os.Stdin)
	var wg sync.WaitGroup
	for {
		line, err := backupproto.ReadLine(br, backupproto.MaxLineBytes)
		if err != nil {
			break
		}
		line = []byte(strings.TrimSpace(string(line)))
		if len(line) == 0 {
			continue
		}
		var req backupproto.Request
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}
		if mode == fakeDeafAlways || (mode == fakeDeafAfterOpen && req.Method != backupproto.MethodOpen) {
			// Read it, acknowledge nothing. The client must bound the wait
			// itself; nothing here will ever end it.
			continue
		}
		wg.Add(1)
		go func(req backupproto.Request) {
			defer wg.Done()
			switch req.Method {
			case backupproto.MethodTrack:
				var p backupproto.TrackParams
				_ = json.Unmarshal(req.Params, &p)
				mu.Lock()
				tracked[p.Name] = p.Path
				mu.Unlock()
			case backupproto.MethodUntrack:
				if mode == fakeSlowOps {
					waitForRelease()
				}
				var p backupproto.UntrackParams
				_ = json.Unmarshal(req.Params, &p)
				if mode == fakeUntrackFails {
					respond(&backupproto.Response{ID: req.ID, OK: false,
						Code: backupproto.CodeInternal, Error: "scripted untrack failure"})
					return
				}
				mu.Lock()
				delete(tracked, p.Name)
				mu.Unlock()
			case backupproto.MethodRestore:
				if mode == fakeSlowOps {
					waitForRelease()
				}
				respond(&backupproto.Response{ID: req.ID, OK: true,
					Result: mustJSON(backupproto.RestoreResult{Restored: false})})
				return
			case backupproto.MethodStatus:
				if mode == fakeSlowStatus {
					waitForRelease()
				}
				mu.Lock()
				statusCalls++
				n := statusCalls
				out := make([]backupproto.DBStatus, 0, len(tracked))
				for name := range tracked {
					out = append(out, backupproto.DBStatus{Name: name, InSync: true, LocalTXID: 1, RemoteTXID: 1})
				}
				mu.Unlock()
				if mode == fakeOversized && n == 1 {
					// A line past the cap, then the real answer. The client must
					// discard the first and still deliver the second.
					writeMu.Lock()
					_, _ = protocol.Write([]byte(strings.Repeat("x", backupproto.MaxLineBytes+16) + "\n"))
					writeMu.Unlock()
				}
				respond(&backupproto.Response{ID: req.ID, OK: true,
					Result: mustJSON(backupproto.StatusResult{Databases: out})})
				return
			}
			respond(&backupproto.Response{ID: req.ID, OK: true})
		}(req)
	}
	wg.Wait()
	if mode == fakeDeafAfterOpen || mode == fakeDeafAlways {
		// Ignore EOF too. A well-behaved agent exits when its stdin closes, so
		// only a misbehaving one tests the kill — and the kill is the guarantee
		// that knomit never leaves an orphan replicating to the prefix its
		// successor will claim.
		select {}
	}
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// openFakeManager boots a Manager against the scripted agent, returning
// whatever Open returned. Tests that expect the boot to FAIL use this directly.
func openFakeManager(t *testing.T, mode string, env ...string) (*Manager, string, error) {
	t.Helper()
	home := t.TempDir()
	cfg := config.BackupConfig{
		Enabled:         true,
		URL:             "file://" + t.TempDir(),
		Instance:        "test",
		MonitorInterval: 50 * time.Millisecond,
	}
	full := append(os.Environ(), fakeAgentEnv+"="+mode)
	full = append(full, env...)
	m, err := openWithAgent(cfg, home, os.Args[0], full)
	if m != nil {
		t.Cleanup(func() { _ = m.Close(context.Background()) })
	}
	return m, home, err
}

// newFakeManager returns a Manager backed by the scripted agent.
func newFakeManager(t *testing.T, mode string, env ...string) (*Manager, string) {
	t.Helper()
	m, home, err := openFakeManager(t, mode, env...)
	if err != nil {
		t.Fatalf("openWithAgent: %v", err)
	}
	return m, home
}

// releaseFile returns a path the fake agent's slow operations wait on, plus the
// function that unblocks them.
func releaseFile(t *testing.T) (string, func()) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "release")
	return p, func() {
		if err := os.WriteFile(p, []byte("go"), 0o644); err != nil {
			t.Errorf("release: %v", err)
		}
	}
}

// TestStatusDoesNotQueueBehindASlowRestore is the concurrency requirement in
// its sharpest form. A restore can take minutes; status is the ops surface and
// must answer while one is in flight.
//
// A protocol that answered requests in order — the obvious first implementation
// — fails this outright, because the restore's response is written before the
// status request is even read. The scripted agent wedges the restore until this
// test releases it, so a serialising implementation deadlocks rather than
// merely being slow.
func TestStatusDoesNotQueueBehindASlowRestore(t *testing.T) {
	release, unblock := releaseFile(t)
	m, home := newFakeManager(t, fakeSlowOps, fakeSlowReleaseEnv+"="+release)

	if err := m.Track("core", filepath.Join(home, "core.db")); err != nil {
		t.Fatalf("Track: %v", err)
	}

	restoreDone := make(chan error, 1)
	go func() {
		_, err := m.restoreIfAbsent(t.Context(), "repos/core.db", filepath.Join(home, "restored.db"))
		restoreDone <- err
	}()

	// Give the restore time to be in flight before probing.
	time.Sleep(200 * time.Millisecond)

	statusDone := make(chan []DBStatus, 1)
	go func() { statusDone <- m.Status(t.Context()) }()

	select {
	case st := <-statusDone:
		if len(st) != 1 || st[0].Name != "core" {
			t.Errorf("Status = %+v, want the tracked database", st)
		}
	case <-time.After(3 * time.Second):
		unblock()
		<-restoreDone
		t.Fatal("Status blocked behind an in-flight restore: the protocol is serialising requests")
	}

	unblock()
	if err := <-restoreDone; err != nil {
		t.Fatalf("restore: %v", err)
	}
}

// TestSlowUntrackDoesNotBlockStatus guards the client's own locking. Untrack's
// reply waits on a final replica sync with retry (up to litestream's 30s
// shutdown timeout); a client holding the manager lock across that round trip
// would freeze every Status call for the duration of an object-store hiccup.
func TestSlowUntrackDoesNotBlockStatus(t *testing.T) {
	release, unblock := releaseFile(t)
	m, home := newFakeManager(t, fakeSlowOps, fakeSlowReleaseEnv+"="+release)

	if err := m.Track("core", filepath.Join(home, "core.db")); err != nil {
		t.Fatalf("Track: %v", err)
	}

	untrackDone := make(chan error, 1)
	go func() { untrackDone <- m.Untrack("core") }()
	time.Sleep(200 * time.Millisecond)

	statusDone := make(chan struct{})
	go func() { m.Status(t.Context()); close(statusDone) }()

	select {
	case <-statusDone:
	case <-time.After(3 * time.Second):
		unblock()
		<-untrackDone
		t.Fatal("Status blocked while an Untrack was in flight: the client holds its lock across a round trip")
	}

	unblock()
	if err := <-untrackDone; err != nil {
		t.Fatalf("Untrack: %v", err)
	}
}

// TestPauseKeepsReplicatingWhenUntrackFails guards the gap between "the swap
// was correctly aborted" and "the repo is still backed up". Untrack drops the
// database from the tracked set BEFORE the call that can fail, and the caller
// abandons the swap on a Pause error — so without a repair here, one failed
// pause silently ends replication for that repo until the process restarts.
func TestPauseKeepsReplicatingWhenUntrackFails(t *testing.T) {
	m, home := newFakeManager(t, fakeUntrackFails)
	dbPath := filepath.Join(home, "core.db")
	if err := m.Track("core", dbPath); err != nil {
		t.Fatalf("Track: %v", err)
	}

	resume, err := m.Pause("core")
	if err == nil {
		t.Fatal("Pause = nil, want the Untrack failure surfaced")
	}
	if !strings.Contains(err.Error(), "replication left running") {
		t.Errorf("Pause error = %v, want it to say replication was restored", err)
	}

	m.mu.RLock()
	entry, tracked := m.dbs["core"]
	m.mu.RUnlock()
	if !tracked {
		t.Error("a failed Pause left the database untracked: the repo stops being backed up with no further signal")
	} else if entry.path != dbPath {
		t.Errorf("restored entry path = %q, want %q", entry.path, dbPath)
	}
	if err := resume(); err != nil {
		t.Errorf("resume() after a failed Pause = %v, want a no-op", err)
	}
}

// TestOversizedAgentLineDoesNotWedgeTheChannel: one unreadable line must cost
// one response, not the connection. A reader built on bufio.Scanner STOPS on an
// over-long token, which would end replication for the process's lifetime with
// no error anywhere — the client would simply never hear from the agent again.
func TestOversizedAgentLineDoesNotWedgeTheChannel(t *testing.T) {
	m, home := newFakeManager(t, fakeOversized)
	if err := m.Track("core", filepath.Join(home, "core.db")); err != nil {
		t.Fatalf("Track: %v", err)
	}

	done := make(chan []DBStatus, 1)
	go func() { done <- m.Status(t.Context()) }()
	select {
	case st := <-done:
		if len(st) != 1 || st[0].Name != "core" || st[0].LastError != "" {
			t.Fatalf("Status = %+v, want the tracked database with no error", st)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the oversized line wedged the response channel")
	}

	// And the channel still works afterwards.
	if err := m.Track("second", filepath.Join(home, "second.db")); err != nil {
		t.Fatalf("Track after the oversized line: %v", err)
	}
	if st := m.Status(t.Context()); len(st) != 2 {
		t.Fatalf("Status = %+v, want both databases", st)
	}
}

// TestOpenNamesEveryPathItSearchedForTheAgent: a missing agent must fail the
// boot with a message an operator can act on, not degrade to "backup silently
// disabled". Naming the candidates is the difference between a one-minute fix
// and a bisect.
func TestOpenNamesEveryPathItSearchedForTheAgent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PATH", t.TempDir()) // nothing named knomit-backup anywhere

	_, err := Open(config.BackupConfig{Enabled: true, URL: "file://" + t.TempDir(), Instance: "test"}, home)
	if err == nil {
		t.Fatal("Open with no agent binary succeeded; backup would be silently disabled")
	}
	for _, want := range []string{agentBinary, filepath.Join(home, "bin"), "$PATH", "agent_path"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestOpenRejectsAConfiguredAgentThatIsNotThere: an explicit override that is
// wrong must say so rather than quietly falling through to a different binary.
func TestOpenRejectsAConfiguredAgentThatIsNotThere(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	_, err := Open(config.BackupConfig{
		Enabled:   true,
		URL:       "file://" + t.TempDir(),
		Instance:  "test",
		AgentPath: missing,
	}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("Open = %v, want a failure naming the configured path %q", err, missing)
	}
}

func TestOpenDisabledReturnsNil(t *testing.T) {
	m, err := Open(config.BackupConfig{Enabled: false}, t.TempDir())
	if err != nil {
		t.Fatalf("Open(disabled): %v", err)
	}
	if m != nil {
		t.Error("Open(disabled) returned a Manager; want nil so callers can no-op")
	}
}

// TestNilManagerIsANoOpEverywhere pins the property every caller relies on:
// backup disabled is a nil *Manager, and no call site guards for it.
func TestNilManagerIsANoOpEverywhere(t *testing.T) {
	var m *Manager
	if err := m.Track("core", "/tmp/x.db"); err != nil {
		t.Errorf("Track: %v", err)
	}
	if err := m.Untrack("core"); err != nil {
		t.Errorf("Untrack: %v", err)
	}
	if err := m.TrackArchived("a", "/tmp/x.db"); err != nil {
		t.Errorf("TrackArchived: %v", err)
	}
	if err := m.UntrackArchived("a"); err != nil {
		t.Errorf("UntrackArchived: %v", err)
	}
	if err := m.DeleteArchivedReplica("a"); err != nil {
		t.Errorf("DeleteArchivedReplica: %v", err)
	}
	if ok, err := m.RestoreArchived("a", "/tmp/x.db"); ok || err != nil {
		t.Errorf("RestoreArchived = (%v, %v)", ok, err)
	}
	if err := m.RestoreControl(t.Context()); err != nil {
		t.Errorf("RestoreControl: %v", err)
	}
	if _, err := m.RestoreRepos(t.Context(), nil); err != nil {
		t.Errorf("RestoreRepos: %v", err)
	}
	if err := m.Preflight(t.Context(), "core", "/tmp/x.db"); err != nil {
		t.Errorf("Preflight: %v", err)
	}
	// Pause is the one nil path that hands back a CLOSURE the caller then
	// invokes, so a nil-safe Pause returning a nil resume would still panic at
	// the call site — usually inside a deferred call, during a swap.
	resume, err := m.Pause("core")
	if err != nil {
		t.Errorf("Pause: %v", err)
	} else if resume == nil {
		t.Error("Pause returned a nil resume; every caller invokes it unconditionally")
	} else if err := resume(); err != nil {
		t.Errorf("resume(): %v", err)
	}
	if st := m.Status(t.Context()); st != nil {
		t.Errorf("Status = %v, want nil", st)
	}
	if err := m.Close(t.Context()); err != nil {
		t.Errorf("Close: %v", err)
	}
}
