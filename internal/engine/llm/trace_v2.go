package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// tracingChat writes one JSONL line per Stream call: the per-hook record of
// the request, the response blocks, the usage and any error. It is a debugging
// aid (the v2 twin of TracingAdapter); the durable per-chat record is the
// chat layer's session log.
type tracingChat struct {
	inner ChatModel
	mu    sync.Mutex
	file  *os.File
	seq   int
}

// traceLine is one request in the trace file.
type traceLine struct {
	Seq        int           `json:"seq"`
	Time       string        `json:"ts"`
	Provider   string        `json:"provider"`
	Model      string        `json:"model"`
	Record     RequestRecord `json:"record"`
	Snapshot   Snapshot      `json:"snapshot"`
	Blocks     []Block       `json:"blocks,omitempty"`
	StopReason string        `json:"stop_reason,omitempty"`
	Usage      *Usage        `json:"usage,omitempty"`
	Error      string        `json:"error,omitempty"`
	ElapsedMS  int64         `json:"elapsed_ms"`
}

// NewTracingChat wraps inner and appends a line per request to path (0600).
// Close the returned Closer to release the file.
func NewTracingChat(inner ChatModel, path string) (ChatModel, io.Closer, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("llm trace: open %s: %w", path, err)
	}
	t := &tracingChat{inner: inner, file: f}
	return t, f, nil
}

func (t *tracingChat) Provider() string { return t.inner.Provider() }
func (t *tracingChat) Model() string    { return t.inner.Model() }
func (t *tracingChat) Caps() Caps       { return t.inner.Caps() }

func (t *tracingChat) Stream(ctx context.Context, s Snapshot, o StreamOptions, sink func(Event)) (*Response, error) {
	start := time.Now()
	resp, err := t.inner.Stream(ctx, s, o, sink)
	line := traceLine{
		Time:      start.UTC().Format(time.RFC3339Nano),
		Provider:  t.inner.Provider(),
		Model:     t.inner.Model(),
		Record:    s.Record(),
		Snapshot:  s,
		ElapsedMS: time.Since(start).Milliseconds(),
	}
	if err != nil {
		line.Error = err.Error()
	}
	if resp != nil {
		line.Blocks, line.StopReason, line.Usage = resp.Blocks, resp.StopReason, &resp.Usage
	}
	t.write(line)
	return resp, err
}

// write appends line; a trace that cannot be written never fails the request.
func (t *tracingChat) write(line traceLine) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seq++
	line.Seq = t.seq
	b, err := json.Marshal(line)
	if err != nil {
		b, _ = json.Marshal(traceLine{Seq: line.Seq, Time: line.Time, Provider: line.Provider, Model: line.Model, Record: line.Record, Error: "trace: " + err.Error()})
	}
	_, _ = t.file.Write(append(b, '\n'))
}
