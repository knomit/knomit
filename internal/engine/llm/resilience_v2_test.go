package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeChat scripts one behaviour per attempt.
type fakeChat struct {
	caps     Caps
	attempts atomic.Int32
	script   []func(ctx context.Context, sink func(Event)) (*Response, error)
}

func (f *fakeChat) Provider() string { return "fake" }
func (f *fakeChat) Model() string    { return "fake-model-1" }
func (f *fakeChat) Caps() Caps       { return f.caps }
func (f *fakeChat) Stream(ctx context.Context, _ Snapshot, _ StreamOptions, sink func(Event)) (*Response, error) {
	n := int(f.attempts.Add(1)) - 1
	if n >= len(f.script) {
		n = len(f.script) - 1
	}
	return f.script[n](ctx, sink)
}

var errOverloaded = errors.New("anthropic: HTTP 529: overloaded_error")

func say(text string) func(context.Context, func(Event)) (*Response, error) {
	return func(_ context.Context, sink func(Event)) (*Response, error) {
		if sink != nil { // the ChatModel contract allows a nil sink
			sink(Event{Kind: EventTextDelta, Text: text})
		}
		return &Response{Blocks: []Block{{Kind: BlockText, Text: text}}, StopReason: "end_turn"}, nil
	}
}

func fail(err error) func(context.Context, func(Event)) (*Response, error) {
	return func(context.Context, func(Event)) (*Response, error) { return nil, err }
}

func collect(texts *[]string) func(Event) {
	return func(e Event) {
		if e.Kind == EventTextDelta {
			*texts = append(*texts, e.Text)
		}
	}
}

func TestWrapChat_RetriesOverloadBeforeFirstEvent(t *testing.T) {
	f := &fakeChat{script: []func(context.Context, func(Event)) (*Response, error){fail(errOverloaded), say("ok")}}
	var texts []string
	resp, err := WrapChat(f, policyFor("ollama")).Stream(context.Background(), Snapshot{}, StreamOptions{}, collect(&texts))
	require.NoError(t, err)
	require.Equal(t, []string{"ok"}, texts)
	require.EqualValues(t, 2, f.attempts.Load())
	require.Equal(t, "ok", resp.Blocks[0].Text)
}

func TestWrapChat_NoRetryAfterFirstEvent(t *testing.T) {
	f := &fakeChat{script: []func(context.Context, func(Event)) (*Response, error){
		func(_ context.Context, sink func(Event)) (*Response, error) {
			sink(Event{Kind: EventTextDelta, Text: "par"})
			return nil, errOverloaded
		},
		say("never"),
	}}
	var texts []string
	_, err := WrapChat(f, policyFor("ollama")).Stream(context.Background(), Snapshot{}, StreamOptions{}, collect(&texts))
	require.ErrorIs(t, err, errOverloaded)
	require.EqualValues(t, 1, f.attempts.Load())
	require.Equal(t, []string{"par"}, texts)
}

func TestWrapChat_NonRetryableIsNotRetried(t *testing.T) {
	bad := errors.New("anthropic: HTTP 400: invalid_request_error")
	f := &fakeChat{script: []func(context.Context, func(Event)) (*Response, error){fail(bad), say("never")}}
	_, err := WrapChat(f, policyFor("ollama")).Stream(context.Background(), Snapshot{}, StreamOptions{}, nil)
	require.ErrorIs(t, err, bad)
	require.EqualValues(t, 1, f.attempts.Load())
}

// The attempt timeout bounds the time to the first event, not the stream.
func TestWrapChat_AttemptTimeoutIsTimeToFirstEvent(t *testing.T) {
	stall := func(ctx context.Context, _ func(Event)) (*Response, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	slowButTalking := func(ctx context.Context, sink func(Event)) (*Response, error) {
		sink(Event{Kind: EventTextDelta, Text: "a"})
		select { // outlives the attempt timeout, but has already spoken
		case <-time.After(120 * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		sink(Event{Kind: EventTextDelta, Text: "b"})
		return &Response{StopReason: "end_turn"}, nil
	}
	f := &fakeChat{script: []func(context.Context, func(Event)) (*Response, error){stall, slowButTalking}}
	var texts []string
	_, err := WrapChat(f, Policy{AttemptTimeout: 40 * time.Millisecond, MaxRetries: 1}).
		Stream(context.Background(), Snapshot{}, StreamOptions{}, collect(&texts))
	require.NoError(t, err)
	require.EqualValues(t, 2, f.attempts.Load()) // the stall was retried
	require.Equal(t, []string{"a", "b"}, texts)  // the talking stream was not cut off

	// With no retries left the stall surfaces as a deadline.
	f2 := &fakeChat{script: []func(context.Context, func(Event)) (*Response, error){stall}}
	_, err = WrapChat(f2, Policy{AttemptTimeout: 20 * time.Millisecond}).Stream(context.Background(), Snapshot{}, StreamOptions{}, nil)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.ErrorIs(t, err, errAttemptStalled)
}

func TestWrapChat_KeepsCaps(t *testing.T) {
	f := &fakeChat{caps: Caps{Tools: true, MaxBreakpoints: 3, ContextWindow: 4242, MaxOutput: 99}}
	w := WrapChat(f, policyFor("anthropic"))
	require.Equal(t, f.Caps(), w.Caps())
	require.Equal(t, "fake", w.Provider())
	require.Equal(t, "fake-model-1", w.Model())
	tr, c, err := NewTracingChat(w, filepath.Join(t.TempDir(), "t.jsonl"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	require.Equal(t, f.Caps(), tr.Caps())
}

func TestTracingChat_WritesOneLinePerRequest(t *testing.T) {
	f := &fakeChat{script: []func(context.Context, func(Event)) (*Response, error){say("first"), fail(errOverloaded)}}
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	tr, c, err := NewTracingChat(f, path)
	require.NoError(t, err)

	b := NewBuilder()
	require.NoError(t, b.Add(Item{ID: "instructions", Hook: HookRepo, Text: "INSTR"}))
	require.NoError(t, b.Add(Item{ID: "turn", Hook: HookTurn, Text: "q"}))
	_, err = tr.Stream(context.Background(), b.Snapshot(), StreamOptions{}, nil)
	require.NoError(t, err)
	_, err = tr.Stream(context.Background(), b.Snapshot(), StreamOptions{}, nil)
	require.ErrorIs(t, err, errOverloaded)
	require.NoError(t, c.Close())

	fh, err := os.Open(path)
	require.NoError(t, err)
	defer fh.Close()
	var lines []map[string]json.RawMessage
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var m map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(sc.Bytes(), &m))
		lines = append(lines, m)
	}
	require.Len(t, lines, 2)
	for i, l := range lines {
		var rec struct {
			Items [][]string `json:"items"`
		}
		require.NoError(t, json.Unmarshal(l["record"], &rec))
		require.Equal(t, []string{"instructions"}, rec.Items[3], "line %d", i)
		require.Equal(t, []string{"turn"}, rec.Items[7], "line %d", i)
		require.JSONEq(t, `"fake-model-1"`, string(l["model"]))
	}
	require.JSONEq(t, `1`, string(lines[0]["seq"]))
	require.JSONEq(t, `"end_turn"`, string(lines[0]["stop_reason"]))
	require.NotContains(t, lines[0], "error")
	require.JSONEq(t, `2`, string(lines[1]["seq"]))
	require.Contains(t, string(lines[1]["error"]), "overloaded")
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}
