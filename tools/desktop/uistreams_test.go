//go:build desktop

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeStream is a Wails StreamConn in memory: subscribe is the page's first
// frame, frames collects what the adapter sends.
type fakeStream struct {
	ctx    context.Context
	cancel context.CancelFunc
	sub    string
	frames chan sseFrame
	closed chan struct{}
	once   sync.Once
}

func newFakeStream(sub string) *fakeStream {
	ctx, cancel := context.WithCancel(context.Background())
	return &fakeStream{ctx: ctx, cancel: cancel, sub: sub, frames: make(chan sseFrame, 256), closed: make(chan struct{})}
}

func (f *fakeStream) Context() context.Context { return f.ctx }
func (f *fakeStream) Send(b []byte) error {
	if f.ctx.Err() != nil {
		return errors.New("closed")
	}
	var fr sseFrame
	if err := json.Unmarshal(b, &fr); err != nil {
		return err
	}
	f.frames <- fr
	return nil
}
func (f *fakeStream) ReceiveJSON(v any) error {
	if f.sub == "" {
		<-f.ctx.Done()
		return errors.New("closed")
	}
	return json.Unmarshal([]byte(f.sub), v)
}
func (f *fakeStream) Close() error {
	f.once.Do(func() { close(f.closed); f.cancel() })
	return nil
}

func (f *fakeStream) next(t *testing.T) sseFrame {
	t.Helper()
	select {
	case fr := <-f.frames:
		return fr
	case <-time.After(3 * time.Second):
		t.Fatal("no frame within 3s")
		return sseFrame{}
	}
}

// run starts serveSSEStream and returns a channel that closes when it returns.
func (f *fakeStream) run(open sseOpener) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		serveSSEStream(f, open, sseBackoff{initial: 5 * time.Millisecond, max: 20 * time.Millisecond})
	}()
	return done
}

func okBody(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}
}

func statusResp(code int) *http.Response {
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(""))}
}

func TestValidSSEPath(t *testing.T) {
	good := []string{
		"/api/v1/repo-events",
		"/api/v1/logs/events",
		"/api/v1/repos/a/branches/main/events?since=3",
		"/api/v1/sessions/events",
	}
	bad := []string{
		"", "/", "/api/v2/x", "/api/v1", "api/v1/x",
		"/api/v1/../v2/x", "/api/v1/a/../../b", "/api/v1/%2e%2e/x",
		"//evil.example/api/v1/x", "http://evil.example/api/v1/x", "https://localhost/api/v1/x",
		"/api/v1/x\r\nHost: evil", `/api/v1/a\b`, "/api/v1/x\x00",
	}
	for _, p := range good {
		if !validSSEPath(p) {
			t.Errorf("validSSEPath(%q) = false, want true", p)
		}
	}
	for _, p := range bad {
		if validSSEPath(p) {
			t.Errorf("validSSEPath(%q) = true, want false", p)
		}
	}
}

func TestServeSSEStream_RefusesABadPathWithoutDialing(t *testing.T) {
	f := newFakeStream(`{"path":"/api/v1/../admin"}`)
	opened := 0
	done := f.run(func(context.Context, string, string) (*http.Response, error) {
		opened++
		return nil, errors.New("must not dial")
	})
	fr := f.next(t)
	if fr.T != "error" || !fr.Closed || fr.Status != 400 {
		t.Fatalf("frame = %+v, want a closed 400 error", fr)
	}
	<-done
	if opened != 0 {
		t.Fatalf("the adapter dialed %d times for a refused path", opened)
	}
	select {
	case <-f.closed:
	default:
		t.Fatal("the stream was not closed")
	}
}

func TestServeSSEStream_MapsFramesIncludingMultilineDataIdAndRetry(t *testing.T) {
	var gotPath, gotLast string
	body := ": keepalive\n\n" +
		"retry: 7\n" +
		"event: ready\ndata: {}\n\n" +
		"id: 41\nevent: index\ndata: line one\ndata: line two\n\n" +
		"data: plain\n\n" +
		"event: nodata\n\n" // no data line: not dispatched
	f := newFakeStream(`{"path":"/api/v1/repo-events?x=1"}`)
	opens := 0
	done := f.run(func(ctx context.Context, path, last string) (*http.Response, error) {
		opens++
		if opens > 1 { // block after the first body ends, so only one pass is observed
			<-ctx.Done()
			return nil, ctx.Err()
		}
		gotPath, gotLast = path, last
		return okBody(body), nil
	})
	want := []sseFrame{
		{T: "open"},
		{T: "event", Event: "ready", Data: "{}"},
		{T: "event", Event: "index", Data: "line one\nline two", ID: "41"},
		{T: "event", Data: "plain", ID: "41"},
	}
	for i, w := range want {
		if got := f.next(t); got != w {
			t.Fatalf("frame %d = %+v, want %+v", i, got, w)
		}
	}
	// The body ending is an upstream drop: an error frame, not silence.
	if got := f.next(t); got.T != "error" || got.Closed {
		t.Fatalf("frame after the body = %+v, want a retrying error", got)
	}
	if gotPath != "/api/v1/repo-events?x=1" || gotLast != "" {
		t.Fatalf("first request = %q Last-Event-ID %q", gotPath, gotLast)
	}
	f.cancel()
	<-done
}

func TestServeSSEStream_ReconnectsWithLastEventID(t *testing.T) {
	f := newFakeStream(`{"path":"/api/v1/repo-events"}`)
	var mu sync.Mutex
	var lasts []string
	done := f.run(func(ctx context.Context, path, last string) (*http.Response, error) {
		mu.Lock()
		lasts = append(lasts, last)
		n := len(lasts)
		mu.Unlock()
		switch n {
		case 1:
			return okBody("id: 5\nevent: index\ndata: a\n\n"), nil
		case 2:
			return nil, errors.New("connection refused") // server down
		case 3:
			return okBody("event: ready\ndata: {}\n\n"), nil
		}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	seq := []string{"open", "event", "error", "error", "open", "event"}
	var got []string
	for range seq {
		got = append(got, f.next(t).T)
	}
	if strings.Join(got, ",") != strings.Join(seq, ",") {
		t.Fatalf("frame types = %v, want %v", got, seq)
	}
	f.cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(lasts) < 3 || lasts[0] != "" || lasts[1] != "5" || lasts[2] != "5" {
		t.Fatalf("Last-Event-ID per attempt = %q, want [\"\" 5 5 ...]", lasts)
	}
}

func TestServeSSEStream_ResumeIDFromThePageIsSent(t *testing.T) {
	f := newFakeStream(`{"path":"/api/v1/repo-events","last_event_id":"9"}`)
	got := make(chan string, 1)
	done := f.run(func(ctx context.Context, path, last string) (*http.Response, error) {
		got <- last
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if v := <-got; v != "9" {
		t.Fatalf("Last-Event-ID = %q, want 9", v)
	}
	f.cancel()
	<-done
}

func TestServeSSEStream_ServerRefusalSurfacesTheStatusAndStops(t *testing.T) {
	for _, code := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError} {
		f := newFakeStream(`{"path":"/api/v1/logs/events"}`)
		opens := 0
		done := f.run(func(context.Context, string, string) (*http.Response, error) { opens++; return statusResp(code), nil })
		fr := f.next(t)
		if fr.T != "error" || !fr.Closed || fr.Status != code {
			t.Fatalf("%d: frame = %+v", code, fr)
		}
		<-done
		if opens != 1 {
			t.Fatalf("%d: %d attempts, want exactly 1 (EventSource does not retry a refusal)", code, opens)
		}
		select {
		case <-f.closed:
		default:
			t.Fatalf("%d: stream not closed", code)
		}
	}
}

func TestServeSSEStream_GatewayStatusIsRetried(t *testing.T) {
	f := newFakeStream(`{"path":"/api/v1/repo-events"}`)
	opens := 0
	done := f.run(func(ctx context.Context, _, _ string) (*http.Response, error) {
		opens++
		if opens == 1 {
			return statusResp(http.StatusBadGateway), nil
		}
		return okBody("event: ready\ndata: {}\n\n"), nil
	})
	if fr := f.next(t); fr.T != "error" || fr.Closed {
		t.Fatalf("first frame = %+v, want a retrying error", fr)
	}
	if fr := f.next(t); fr.T != "open" {
		t.Fatalf("second frame = %+v, want open", fr)
	}
	f.cancel()
	<-done
}

func TestServeSSEStream_ClosingTheStreamCancelsTheUpstream(t *testing.T) {
	f := newFakeStream(`{"path":"/api/v1/repo-events"}`)
	pr, pw := io.Pipe()
	upstreamCtx := make(chan context.Context, 1)
	done := f.run(func(ctx context.Context, _, _ string) (*http.Response, error) {
		upstreamCtx <- ctx
		go func() { <-ctx.Done(); pw.CloseWithError(ctx.Err()) }() // what net/http does on a cancelled request
		return &http.Response{StatusCode: 200, Body: pr}, nil
	})
	if fr := f.next(t); fr.T != "open" {
		t.Fatalf("frame = %+v", fr)
	}
	ctx := <-upstreamCtx
	f.cancel() // the page called close(): Wails cancels StreamConn.Context()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the handler did not return after the stream context ended")
	}
	if ctx.Err() == nil {
		t.Fatal("the upstream request context was not cancelled")
	}
}

func TestServeSSEStream_NoSubscribeFrameClosesTheStream(t *testing.T) {
	f := newFakeStream("") // the page never sends the path
	done := f.run(func(context.Context, string, string) (*http.Response, error) {
		t.Error("dialed without a subscribe frame")
		return nil, errors.New("no")
	})
	f.cancel() // the page went away
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the handler hung waiting for the subscribe frame")
	}
}

// The whole path against a real local listener: the opener dials the unix
// socket through auth.DialLocal and the adapter forwards what the server sends.
func TestServeSSEStream_EndToEndOverTheLocalSocket(t *testing.T) {
	gotAccept := make(chan string, 1)
	up := startLocalUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccept <- r.Header.Get("Accept") + "|" + r.Host
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: ready\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	f := newFakeStream(`{"path":"/api/v1/repo-events"}`)
	done := make(chan struct{})
	go func() { defer close(done); serveSSEStream(f, newSSEOpener(up), defaultSSEBackoff) }()
	if fr := f.next(t); fr.T != "open" {
		t.Fatalf("frame = %+v", fr)
	}
	if fr := f.next(t); fr.T != "event" || fr.Event != "ready" {
		t.Fatalf("frame = %+v", fr)
	}
	if v := <-gotAccept; v != "text/event-stream|localhost" {
		t.Fatalf("upstream saw %q", v)
	}
	f.cancel()
	<-done
}
