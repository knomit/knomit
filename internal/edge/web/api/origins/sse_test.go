package origins

import (
	"errors"
	"strings"
	"testing"
)

// beginSSE backs four origin-session flows (test, preview, apply, commit) and
// is the one sender in this package that does not turn a failed write into a
// return — those procedures are not abortable half-way. What must hold instead
// is that a dead client costs nothing: the first failure latches, and every
// later send is a no-op rather than another blocked write.
func TestBeginSSE_LatchesDeadAndStopsWriting(t *testing.T) {
	rec := newStreamRecorder()
	send, ok := beginSSE(rec)
	if !ok {
		t.Fatal("beginSSE refused a recorder that has Flush and SetWriteDeadline")
	}

	send(map[string]string{"step": "first"})
	if !strings.Contains(rec.body(), "first") {
		t.Fatalf("first event not written: %q", rec.body())
	}
	rec.mu.Lock()
	deadlines, before := rec.deadlines, rec.ResponseRecorder.Body.Len()
	rec.mu.Unlock()
	if deadlines == 0 {
		t.Fatal("beginSSE set no write deadline")
	}

	rec.failWrites(errors.New("connection reset by peer"))
	send(map[string]string{"step": "fails"})

	// Everything after the failure must be a no-op: no bytes, and no further
	// deadline calls, which is what proves it is short-circuiting rather than
	// attempting each write and failing.
	rec.failWrites(nil) // let writes succeed again; a latched stream still must not write
	send(map[string]string{"step": "after"})

	rec.mu.Lock()
	after, deadlinesAfter := rec.ResponseRecorder.Body.Len(), rec.deadlines
	rec.mu.Unlock()
	if after != before {
		t.Errorf("stream wrote %d more bytes after a failed write; it should be latched dead", after-before)
	}
	if deadlinesAfter != deadlines+1 {
		t.Errorf("deadline calls went %d → %d; after latching there should be none", deadlines, deadlinesAfter)
	}
}
