package backup

import (
	"io"
	"strings"
	"testing"
	"time"

	"knomit/internal/backupproto"
)

// TestForwardAgentLogKeepsDrainingPastAnOversizedLine is a liveness test, not a
// logging test.
//
// Nothing but this loop reads the agent's stderr. If it stops early, the ~64 KiB
// pipe buffer fills and the agent BLOCKS on its next log write — forever, and
// without dying, so the supervisor never restarts it and every round trip
// against it burns its full budget before failing. A stuck logger is a stopped
// backup, reached through a code path nobody would think to look at.
//
// bufio.Scanner produces exactly that: it stops permanently on a token past its
// buffer. The assertion is therefore that writing CONTINUES to succeed after an
// oversized line — a reader that gave up leaves the writer blocked, and the
// deadline fires.
func TestForwardAgentLogKeepsDrainingPastAnOversizedLine(t *testing.T) {
	r, w := io.Pipe()
	done := make(chan struct{})
	go func() { forwardAgentLog(r); close(done) }()

	write := func(what string, s string) {
		t.Helper()
		written := make(chan error, 1)
		go func() { _, err := io.WriteString(w, s); written <- err }()
		select {
		case err := <-written:
			if err != nil {
				t.Fatalf("writing %s: %v", what, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("the agent blocked writing %s: its stderr is no longer being drained, "+
				"so it will wedge on its next log line and never die", what)
		}
	}

	write("a normal line", `{"level":"INFO","msg":"hello"}`+"\n")
	write("an oversized line", strings.Repeat("x", backupproto.MaxLineBytes+64)+"\n")
	write("a line after the oversized one", `{"level":"WARN","msg":"still here"}`+"\n")
	// Enough to fill a pipe buffer several times over, which is what actually
	// blocks a producer whose consumer has stopped.
	for i := 0; i < 8; i++ {
		write("bulk", strings.Repeat("y", 64*1024)+"\n")
	}

	w.Close()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("forwardAgentLog did not return at EOF")
	}
}

// TestForwardAgentLogSurvivesNonJSON: a panic trace or a runtime message is
// exactly what an operator most needs to see, and it is never JSON.
func TestForwardAgentLogSurvivesNonJSON(t *testing.T) {
	r, w := io.Pipe()
	done := make(chan struct{})
	go func() { forwardAgentLog(r); close(done) }()

	_, _ = io.WriteString(w, "panic: runtime error: invalid memory address\n")
	_, _ = io.WriteString(w, "\n")
	_, _ = io.WriteString(w, `{"level":"ERROR","msg":"after the panic","db":"core"}`+"\n")
	w.Close()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("forwardAgentLog wedged on a non-JSON line")
	}
}
