// Package crashdump captures forensic artifacts when the process dies, so a
// central deployment can be investigated post-mortem without any third-party
// tooling. It is pure stdlib.
package crashdump

import (
	"bytes"
	"sync"
)

// RingWriter is an io.Writer that retains the last N newline-delimited records
// written to it. It is wired into the zerolog MultiLevelWriter so that, at
// crash time, the crash report can include the most recent log lines that led
// up to the failure. Safe for concurrent use.
type RingWriter struct {
	mu    sync.Mutex
	max   int
	lines []string
}

// NewRingWriter returns a RingWriter retaining at most max records. A max <= 0
// disables retention (Lines always returns empty) while still satisfying the
// io.Writer contract.
func NewRingWriter(max int) *RingWriter {
	return &RingWriter{max: max}
}

// Write splits p on newlines, appends each non-empty record, and drops the
// oldest records beyond the retention limit. It always reports len(p) consumed
// so it never triggers a short-write error in a MultiLevelWriter.
func (w *RingWriter) Write(p []byte) (int, error) {
	if w.max > 0 {
		w.mu.Lock()
		for rec := range bytes.SplitSeq(p, []byte{'\n'}) {
			if len(rec) == 0 {
				continue
			}
			w.lines = append(w.lines, string(rec))
		}
		if over := len(w.lines) - w.max; over > 0 {
			w.lines = w.lines[over:]
		}
		w.mu.Unlock()
	}
	return len(p), nil
}

// Lines returns a copy of the retained records, oldest first.
func (w *RingWriter) Lines() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]string, len(w.lines))
	copy(out, w.lines)
	return out
}
