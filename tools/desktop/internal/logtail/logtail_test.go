package logtail_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"knomit/tools/desktop/internal/logtail"
)

// collector accumulates emitted lines from the tailer goroutine.
type collector struct {
	mu    sync.Mutex
	lines []string
}

func (c *collector) emit(batch []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, batch...)
}

func (c *collector) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.lines...)
}

// waitFor polls until cond holds or the deadline passes, so the tests do not
// depend on a fixed sleep outrunning the tailer's poll interval.
func waitFor(t *testing.T, c *collector, cond func([]string) bool) []string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := c.snapshot(); cond(got) {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	got := c.snapshot()
	t.Fatalf("condition never held; collected %d lines: %v", len(got), got)
	return nil
}

func startTailer(t *testing.T, path string, opts logtail.Options) *collector {
	t.Helper()
	c := &collector{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan struct{})
	go func() {
		defer close(done)
		logtail.New(path, opts).Run(ctx, c.emit)
	}()
	t.Cleanup(func() { cancel(); <-done })
	return c
}

func fastOpts() logtail.Options {
	return logtail.Options{BacklogBytes: 64 << 10, Poll: 5 * time.Millisecond}
}

// Opening the window should show what already happened, not just what happens
// next — that history is the whole reason we tail the file instead of tee-ing
// the logger.
func TestTailerEmitsExistingBacklogFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("first\nsecond\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := startTailer(t, path, fastOpts())
	got := waitFor(t, c, func(l []string) bool { return len(l) >= 2 })

	if got[0] != "first" || got[1] != "second" {
		t.Errorf("backlog = %v, want [first second]", got[:2])
	}
}

func TestTailerEmitsAppendedLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := startTailer(t, path, fastOpts())
	waitFor(t, c, func(l []string) bool { return len(l) >= 1 })

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("second\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	got := waitFor(t, c, func(l []string) bool { return len(l) >= 2 })
	if got[1] != "second" {
		t.Errorf("got %v, want second appended", got)
	}
}

// lumberjack renames the live file and creates a fresh one. Without rotation
// handling the tailer follows the renamed inode and goes permanently silent —
// the log window would simply stop updating, with no error anywhere.
func TestTailerFollowsARotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	if err := os.WriteFile(path, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := startTailer(t, path, fastOpts())
	waitFor(t, c, func(l []string) bool { return len(l) >= 1 })

	// Exactly what lumberjack does: rename the live file aside, create a new one.
	if err := os.Rename(path, filepath.Join(dir, "app-old.log")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("after\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := waitFor(t, c, func(l []string) bool {
		return len(l) >= 2 && l[len(l)-1] == "after"
	})
	if got[0] != "before" {
		t.Errorf("lost the pre-rotation line: %v", got)
	}
}

// A line still being written must not be emitted half-formed.
func TestTailerWithholdsAPartialLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("complete\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := startTailer(t, path, fastOpts())
	waitFor(t, c, func(l []string) bool { return len(l) >= 1 })

	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("partial-no-newline")
	f.Close()

	time.Sleep(60 * time.Millisecond)
	for _, line := range c.snapshot() {
		if strings.Contains(line, "partial") {
			t.Fatalf("emitted an unterminated line: %q", line)
		}
	}

	f, _ = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("-done\n")
	f.Close()

	got := waitFor(t, c, func(l []string) bool { return len(l) >= 2 })
	if got[1] != "partial-no-newline-done" {
		t.Errorf("got %q, want the rejoined line", got[1])
	}
}

// A huge existing log must not be replayed in full into the window.
func TestTailerBoundsTheBacklogToWholeLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	var sb strings.Builder
	for i := range 5000 {
		sb.WriteString(strings.Repeat("x", 40))
		sb.WriteByte(byte('0' + i%10))
		sb.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	c := startTailer(t, path, logtail.Options{BacklogBytes: 4 << 10, Poll: 5 * time.Millisecond})
	got := waitFor(t, c, func(l []string) bool { return len(l) > 0 })

	if len(got) > 200 {
		t.Errorf("emitted %d backlog lines, want the tail only", len(got))
	}
	// The seek lands mid-line; the first emitted line must be whole, not a
	// fragment starting partway through one.
	if len(got[0]) != 41 {
		t.Errorf("first backlog line is a fragment: %q", got[0])
	}
}

// A missing file is normal at startup — the window can be opened before
// anything has been logged. The tailer must wait for it, not fail.
func TestTailerWaitsForAMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-yet.log")
	c := startTailer(t, path, fastOpts())

	time.Sleep(30 * time.Millisecond)
	if err := os.WriteFile(path, []byte("appeared\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := waitFor(t, c, func(l []string) bool { return len(l) >= 1 })
	if got[0] != "appeared" {
		t.Errorf("got %v, want [appeared]", got)
	}
}
