package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"knomit/internal/backup/proto"
)

// serveHarness runs Serve over in-memory pipes and lets a test drive the
// protocol by hand, the way an operator could with a terminal.
type serveHarness struct {
	agent *Agent
	in    *io.PipeWriter
	out   *bufio.Reader
	done  chan error
	mu    sync.Mutex
}

func newServeHarness(t *testing.T) *serveHarness {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	a := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := &serveHarness{agent: a, in: inW, out: bufio.NewReader(outR), done: make(chan error, 1)}
	go func() { h.done <- a.Serve(context.Background(), inR, outW); outW.Close() }()
	t.Cleanup(func() {
		inW.Close()
		select {
		case <-h.done:
		case <-time.After(10 * time.Second):
			t.Error("Serve did not return after its input closed")
		}
	})
	return h
}

// send writes one raw line.
func (h *serveHarness) send(t *testing.T, line string) {
	t.Helper()
	if err := h.write(line); err != nil {
		t.Fatalf("write %q: %v", line, err)
	}
}

// write is send without a *testing.T, for the oversized-line case: an io.Pipe
// blocks until the reader drains, so that write has to happen off the test
// goroutine — where t.Fatalf is not allowed.
func (h *serveHarness) write(line string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.in, line+"\n")
	return err
}

// recv reads one response line.
func (h *serveHarness) recv(t *testing.T) proto.Response {
	t.Helper()
	line, err := proto.ReadLine(h.out, proto.MaxLineBytes)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	var resp proto.Response
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("decode response %q: %v", line, err)
	}
	return resp
}

// TestMalformedLineIsAnsweredAndTheChannelSurvives: a request that will not
// parse must cost one response, not the connection. A serve loop that returned
// on a decode error would end replication for the process's lifetime, and the
// only symptom would be that knomit never hears from the agent again.
func TestMalformedLineIsAnsweredAndTheChannelSurvives(t *testing.T) {
	h := newServeHarness(t)

	h.send(t, `{"id":7,"method":`) // truncated JSON
	resp := h.recv(t)
	if resp.ID != 7 {
		t.Errorf("id = %d, want 7 recovered from the malformed line so the waiter is answered", resp.ID)
	}
	if resp.OK || resp.Code != proto.CodeBadRequest {
		t.Errorf("resp = %+v, want a bad_request refusal", resp)
	}

	h.send(t, `not json at all`)
	resp = h.recv(t)
	if resp.OK || resp.Code != proto.CodeBadRequest {
		t.Errorf("resp = %+v, want a bad_request refusal", resp)
	}

	// The channel still works.
	h.send(t, `{"id":9,"method":"status"}`)
	resp = h.recv(t)
	if resp.ID != 9 || resp.OK {
		t.Fatalf("resp = %+v, want id 9 refused with not_open (the agent is not open)", resp)
	}
	if resp.Code != proto.CodeNotOpen {
		t.Errorf("code = %q, want %q", resp.Code, proto.CodeNotOpen)
	}
}

// TestOversizedRequestLineDoesNotWedgeTheChannel: the reader must resynchronise
// on the next newline rather than stop. bufio.Scanner — the obvious choice —
// STOPS on an over-long token, so this is the test that rules it out.
func TestOversizedRequestLineDoesNotWedgeTheChannel(t *testing.T) {
	h := newServeHarness(t)

	go func() { _ = h.write(strings.Repeat("x", proto.MaxLineBytes+64)) }()
	resp := h.recv(t)
	if resp.OK || resp.Code != proto.CodeBadRequest {
		t.Fatalf("resp = %+v, want a bad_request refusal for the oversized line", resp)
	}

	h.send(t, `{"id":3,"method":"status"}`)
	resp = h.recv(t)
	if resp.ID != 3 {
		t.Fatalf("resp = %+v, want id 3: the oversized line wedged the channel", resp)
	}
}

// TestUnknownMethodIsRefusedNotFatal keeps a version skew (a knomit newer than
// its agent) from taking replication down: the unknown call fails, everything
// else keeps working.
func TestUnknownMethodIsRefusedNotFatal(t *testing.T) {
	h := newServeHarness(t)

	h.send(t, `{"id":1,"method":"teleport"}`)
	resp := h.recv(t)
	if resp.OK || resp.Code != proto.CodeUnknownMethod {
		t.Fatalf("resp = %+v, want an unknown_method refusal", resp)
	}
	if !strings.Contains(resp.Error, "teleport") {
		t.Errorf("error %q does not name the method", resp.Error)
	}

	h.send(t, `{"id":2,"method":"status"}`)
	if resp := h.recv(t); resp.ID != 2 {
		t.Fatalf("resp = %+v, want id 2", resp)
	}
}

// TestResponsesAreCorrelatedNotOrdered pins the concurrency contract. Several
// requests go out before any response is read, and the ids must all come back —
// in whatever order the handlers finished. An implementation that answered in
// arrival order would satisfy a weaker test; this one only checks the set,
// which is exactly what the client relies on.
func TestResponsesAreCorrelatedNotOrdered(t *testing.T) {
	h := newServeHarness(t)

	const n = 25
	for i := 1; i <= n; i++ {
		h.send(t, `{"id":`+itoa(i)+`,"method":"status"}`)
	}
	seen := map[uint64]bool{}
	for i := 0; i < n; i++ {
		resp := h.recv(t)
		if seen[resp.ID] {
			t.Fatalf("id %d answered twice", resp.ID)
		}
		seen[resp.ID] = true
	}
	for i := uint64(1); i <= n; i++ {
		if !seen[i] {
			t.Errorf("id %d never answered", i)
		}
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// TestEOFClosesTheAgent: knomit's death closes the write end of this pipe, and
// that must be enough to shut replication down. It is the ONLY mechanism that
// works when knomit is SIGKILLed, and an agent that ignored it would outlive
// its parent and keep writing to the replica prefix the next knomit will claim.
func TestEOFClosesTheAgent(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go io.Copy(io.Discard, outR)

	a := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	done := make(chan error, 1)
	go func() { done <- a.Serve(context.Background(), inR, outW) }()

	inW.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve after EOF = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return when its input reached EOF: a SIGKILLed knomit would leave this agent running")
	}
	a.mu.RLock()
	closed := a.closed
	a.mu.RUnlock()
	if !closed {
		t.Error("the agent did not mark itself closed on EOF")
	}
}
