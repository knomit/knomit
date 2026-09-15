package logging

import (
	"context"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTap_RetainsLastNLines(t *testing.T) {
	tap := NewTap(context.Background(), 3)
	for _, s := range []string{"one\n", "two\n", "three\n", "four\n"} {
		n, err := tap.Write([]byte(s))
		if err != nil || n != len(s) {
			t.Fatalf("Write(%q) = %d, %v; want %d, nil", s, n, err, len(s))
		}
	}
	got := tap.Lines()
	want := []string{"two", "three", "four"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("Lines() = %v, want %v", got, want)
	}
}

// One Write can carry several records, and blank ones are not lines.
func TestTap_SplitsOnNewlineAndSkipsEmpty(t *testing.T) {
	tap := NewTap(context.Background(), 10)
	if _, err := tap.Write([]byte("a\n\nb\n")); err != nil {
		t.Fatal(err)
	}
	if got := tap.Lines(); strings.Join(got, "|") != "a|b" {
		t.Errorf("Lines() = %v, want [a b]", got)
	}
}

// Subscribe hands back the backlog and the live channel together. Taking them
// in two steps would drop a line published between the two, or replay one
// twice — and the client cannot tell either from a gap in the log.
func TestTap_SubscribeSnapshotAndLiveAreAtomic(t *testing.T) {
	tap := NewTap(context.Background(), 10)
	if _, err := tap.Write([]byte("before\n")); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub, snapshot := tap.Subscribe(ctx)
	if strings.Join(snapshot, "|") != "before" {
		t.Fatalf("snapshot = %v, want [before]", snapshot)
	}

	if _, err := tap.Write([]byte("after\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case line := <-sub.Lines():
		if line != "after" {
			t.Errorf("live line = %q, want %q", line, "after")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no live line delivered")
	}
	// The backlog is NOT replayed onto the live channel as well.
	select {
	case line := <-sub.Lines():
		t.Fatalf("unexpected second line %q — the snapshot was replayed live too", line)
	case <-time.After(100 * time.Millisecond):
	}
}

// THE memory-safety property. goob's per-subscriber pipe buffers into an
// unbounded slice, so a browser that stops reading a debug-level stream would
// grow the server's heap without limit. Delivery is capped instead, and what
// the cap costs is COUNTED — a log viewer that silently discards history is
// lying by omission.
func TestTap_SlowSubscriberDropsAreBoundedAndCounted(t *testing.T) {
	tap := NewTap(context.Background(), 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub, _ := tap.Subscribe(ctx) // deliberately never read from

	const sent = subscriberBuffer + 500
	for i := 0; i < sent; i++ {
		if _, err := tap.Write([]byte("line\n")); err != nil {
			t.Fatal(err)
		}
	}

	dropped := sub.Dropped()
	if dropped == 0 {
		t.Fatal("a subscriber that never reads dropped nothing — delivery is unbounded")
	}
	// Everything sent is either sitting in the bounded buffer or counted as
	// dropped. Nothing vanishes unaccounted for.
	if buffered := len(sub.Lines()); uint64(buffered)+dropped != uint64(sent) {
		t.Errorf("buffered %d + dropped %d != %d sent", buffered, dropped, sent)
	}
	if cap(sub.Lines()) > subscriberBuffer {
		t.Errorf("subscriber buffer grew to %d, want <= %d", cap(sub.Lines()), subscriberBuffer)
	}
	// The retained ring is bounded independently of the subscriber.
	if n := len(tap.Lines()); n > 4 {
		t.Errorf("ring holds %d lines, want <= 4", n)
	}
}

// A publisher must never be held up by a subscriber, dropping or not: this
// writer sits in the logging chain, so a blocked Write blocks whatever was
// logging.
func TestTap_WriteNeverBlocksOnASubscriber(t *testing.T) {
	tap := NewTap(context.Background(), 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tap.Subscribe(ctx) // never read

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < subscriberBuffer*3; i++ {
			tap.Write([]byte("x\n"))
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Write blocked on a subscriber that stopped reading")
	}
}

// A browser closing its tab must not leave the tap fanning out to a dead
// channel for the life of the process.
func TestTap_SubscriberGoesAwayWithItsContext(t *testing.T) {
	tap := NewTap(context.Background(), 2)
	ctx, cancel := context.WithCancel(context.Background())
	tap.Subscribe(ctx)
	if n := tap.Subscribers(); n != 1 {
		t.Fatalf("Subscribers() = %d, want 1", n)
	}
	cancel()
	deadline := time.Now().Add(2 * time.Second)
	for tap.Subscribers() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("Subscribers() = %d after cancel, want 0", tap.Subscribers())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestTap_ZeroMaxRetainsNothingButStillWrites(t *testing.T) {
	tap := NewTap(context.Background(), 0)
	n, err := tap.Write([]byte("dropped\n"))
	if err != nil || n != len("dropped\n") {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if got := tap.Lines(); len(got) != 0 {
		t.Errorf("Lines() = %v, want empty", got)
	}
}

func TestTap_ConcurrentWritersAndSubscribers(t *testing.T) {
	tap := NewTap(context.Background(), 50)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				tap.Write([]byte("c\n"))
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sub, _ := tap.Subscribe(ctx)
			for j := 0; j < 50; j++ {
				select {
				case <-sub.Lines():
				case <-time.After(time.Second):
					return
				}
			}
		}()
	}
	wg.Wait() // -race is what makes this test worth having
}

// THE FORMAT CONTRACT, pinned through the REAL writer chain rather than by
// writing a hand-made line into the tap.
//
// The viewer parses `<stamp> <LVL> <message>` with a whitespace-free stamp
// (LogView.tsx parseConsole). BuildWriter's `ring` argument receives zerolog's
// raw JSON, NOT console output, so a tap wired in as a bare io.Writer would
// deliver records the console branch of the parser cannot read. Tap.Writer()
// is what makes the bytes the viewer's format.
func TestTapWriter_EmitsTheFileFormatThroughTheRealChain(t *testing.T) {
	tap := NewTap(context.Background(), 10)
	lg, _, err := Build(Options{Format: "console", Level: "info"}, os.Stderr, os.Stderr, nil, tap.Writer())
	if err != nil {
		t.Fatal(err)
	}
	lg.Info().Str("repo", "core").Msg("hello")

	lines := tap.Lines()
	if len(lines) != 1 {
		t.Fatalf("tap holds %d lines, want 1: %v", len(lines), lines)
	}
	// <RFC3339> INF hello repo=core — the stamp must contain no space, or the
	// level shifts out of the second token and the viewer's level filter
	// silently stops filtering.
	re := regexp.MustCompile(`^(\S+) INF hello repo=core$`)
	m := re.FindStringSubmatch(lines[0])
	if m == nil {
		t.Fatalf("tap line %q does not match `<stamp> INF hello repo=core`", lines[0])
	}
	if _, err := time.Parse(time.RFC3339, m[1]); err != nil {
		t.Errorf("stamp %q is not RFC3339: %v", m[1], err)
	}
}

// The tap must not colourise: ANSI escapes would reach the browser as garbage
// in the middle of the level token.
func TestTapWriter_NoANSIEscapes(t *testing.T) {
	tap := NewTap(context.Background(), 4)
	lg, _, err := Build(Options{Format: "console", Level: "info"}, os.Stderr, os.Stderr, nil, tap.Writer())
	if err != nil {
		t.Fatal(err)
	}
	lg.Error().Msg("boom")
	for _, l := range tap.Lines() {
		if strings.Contains(l, "\x1b[") {
			t.Errorf("tap line carries ANSI escapes: %q", l)
		}
	}
}
