package web

import (
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Shared SSE test double. Every stream handler in this package is exercised
// through it, so the deadline-and-checked-error contract in sse.go is asserted
// the same way everywhere rather than once per handler's own recorder.

// streamRecorder is the flusher recorder from handlers_events_test.go with a
// lock and a signal, because this test reads the body from one goroutine
// while the handler writes it from another — a plain ResponseRecorder races.
type streamRecorder struct {
	*httptest.ResponseRecorder
	mu      sync.Mutex
	flushed chan struct{}
	// writeErr, when set, fails every Write — a client whose socket is gone.
	writeErr error
	// gate, when non-nil, holds Write until unblock closes it — a client that
	// has stopped reading. Used to make a handler fall behind on purpose.
	gate chan struct{}
	// deadlines counts SetWriteDeadline calls. httptest.ResponseRecorder does
	// not implement it, so without the method below the handler's deadline
	// path would report ErrNotSupported in every test and never be exercised.
	deadlines int
}

// SetWriteDeadline makes the recorder satisfy what http.ResponseController
// looks for, so the handler's deadline path runs here as it does in a server.
func (s *streamRecorder) SetWriteDeadline(time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deadlines++
	return nil
}

// failWrites makes every subsequent Write fail, as a write to a closed socket
// does.
func (s *streamRecorder) failWrites(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeErr = err
}

func newStreamRecorder() *streamRecorder {
	return &streamRecorder{ResponseRecorder: httptest.NewRecorder(), flushed: make(chan struct{}, 64)}
}

func (s *streamRecorder) Write(b []byte) (int, error) {
	// Read the gate and RELEASE the lock before waiting on it: body() takes
	// the same lock, and a test that blocked writes still has to be able to
	// read what was written before the block.
	s.mu.Lock()
	gate := s.gate
	s.mu.Unlock()
	if gate != nil {
		<-gate
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeErr != nil {
		return 0, s.writeErr
	}
	return s.ResponseRecorder.Write(b)
}

// block makes every subsequent Write wait, simulating a client that has
// stopped reading its socket.
func (s *streamRecorder) block() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gate = make(chan struct{})
}

// unblock releases a block, letting the handler catch up.
func (s *streamRecorder) unblock() {
	s.mu.Lock()
	gate := s.gate
	s.gate = nil
	s.mu.Unlock()
	if gate != nil {
		close(gate)
	}
}

func (s *streamRecorder) Flush() {
	select {
	case s.flushed <- struct{}{}:
	default:
	}
}

func (s *streamRecorder) body() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ResponseRecorder.Body.String()
}

// waitFor polls the recorded body until cond holds, so the test never sleeps
// a fixed interval waiting for a frame that is already there.
func (s *streamRecorder) waitFor(t *testing.T, what string, cond func(string) bool) string {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		if b := s.body(); cond(b) {
			return b
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s; body so far:\n%s", what, s.body())
		case <-s.flushed:
		case <-time.After(10 * time.Millisecond):
		}
	}
}
