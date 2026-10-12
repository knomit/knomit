//go:build desktop

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/wailsapp/wails/v3/pkg/application"
)

// sseStreamName is the ONE Wails Stream the UI opens. A Wails stream name is
// matched exactly (no path, query or headers), so the page names the API path in
// the first frame and this handler validates it.
//
// Why a Stream and not the asset-handler proxy: the Windows response writer
// buffers a response until the handler returns, so an SSE body delivers nothing
// there. A Stream is a held poll plus a POST, and every poll response completes,
// which is exactly what that writer needs. macOS and Linux carry the same wire,
// so there is one transport, not one per platform.
//
// Limits Wails imposes (v3.0.0-beta.28, pkg/application/stream*.go), which this
// adapter inherits rather than re-enforces: 256 open streams per page, 16 stream
// sessions per window, 4096 open streams per app; per window 8 MiB / 256 frames
// queued toward the page (Send blocks when full, so a stalled page throttles the
// upstream read instead of growing memory); one frame at most 64 MiB; a poll
// answer at most 1 MiB, so a burst larger than that arrives in several polls
// (Windows pays one HTTP round trip per batch). The UI holds six kinds of
// stream, a handful at a time.
const sseStreamName = "knomit-sse"

// subscribeTimeout is how long a freshly opened stream may stay silent before
// its first frame (the subscribe request) arrives.
const subscribeTimeout = 10 * time.Second

// sseSubscribe is the page's first frame.
type sseSubscribe struct {
	Path        string `json:"path"`
	LastEventID string `json:"last_event_id,omitempty"`
}

// sseFrame is one desktop-to-page frame; web/src/eventStream.ts DesktopFrame is
// its mirror. Change both together.
//
//	open   the upstream answered 200: EventSource's open
//	event  one dispatched SSE event
//	error  the upstream leg failed. Closed false: the desktop is retrying
//	       (EventSource's CONNECTING-with-error). Closed true: the desktop gave
//	       up; Status carries the HTTP status when the server answered one.
type sseFrame struct {
	T      string `json:"t"`
	Event  string `json:"event,omitempty"`
	Data   string `json:"data,omitempty"`
	ID     string `json:"id,omitempty"`
	Closed bool   `json:"closed,omitempty"`
	Status int    `json:"status,omitempty"`
}

// streamConn is the part of *application.StreamConn the adapter uses, so tests
// can drive it without a Wails window.
type streamConn interface {
	Context() context.Context
	Send(data []byte) error
	ReceiveJSON(v any) error
	Close() error
}

// sseOpener performs the upstream GET for one subscription attempt.
type sseOpener func(ctx context.Context, path, lastEventID string) (*http.Response, error)

// sseBackoff is the reconnect schedule. The server's `retry:` field overrides
// the current delay, as it does for EventSource.
type sseBackoff struct{ initial, max time.Duration }

var defaultSSEBackoff = sseBackoff{initial: 500 * time.Millisecond, max: 5 * time.Second}

// registerSSEStream installs the "knomit-sse" handler on the app.
func registerSSEStream(wapp *application.App, up *uiUpstream) {
	open := newSSEOpener(up)
	wapp.HandleStream(sseStreamName, func(c *application.StreamConn) {
		serveSSEStream(c, open, defaultSSEBackoff)
	})
}

// newSSEOpener returns the upstream GET for a subscription: one non-pooled
// connection per attempt over the local socket / pipe, so cancelling the
// stream's context closes exactly that connection.
func newSSEOpener(up *uiUpstream) sseOpener {
	client := &http.Client{Transport: newUpstreamTransport(up, false)}
	return func(ctx context.Context, path, lastEventID string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost"+path, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "text/event-stream")
		if lastEventID != "" {
			req.Header.Set("Last-Event-ID", lastEventID)
		}
		return client.Do(req)
	}
}

// validSSEPath accepts only an API read: an absolute path under /api/v1/ with no
// scheme, host, dot-dot segment, backslash or control character. The page names
// a path and nothing else, so it cannot steer the dial at another host or verb.
func validSSEPath(p string) bool {
	if !strings.HasPrefix(p, "/api/v1/") || strings.HasPrefix(p, "//") || strings.Contains(p, `\`) {
		return false
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	u, err := url.Parse(p)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || !strings.HasPrefix(u.Path, "/api/v1/") {
		return false
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if seg == ".." {
			return false
		}
	}
	return !strings.Contains(u.RawPath, "..")
}

// serveSSEStream is one Wails stream connection: read the subscribe frame, then
// hold one upstream SSE subscription, forwarding events, until the page closes
// the stream (c.Context() done) or the server refuses it.
func serveSSEStream(c streamConn, open sseOpener, bo sseBackoff) {
	ctx := c.Context()
	sub, ok := readSubscribe(c)
	if !ok {
		_ = c.Close()
		return
	}
	if !validSSEPath(sub.Path) {
		log.Warn().Str("path", sub.Path).Msg("desktop sse stream: refused path")
		_ = sendFrame(c, sseFrame{T: "error", Closed: true, Status: http.StatusBadRequest})
		_ = c.Close()
		return
	}
	lastID := sub.LastEventID
	delay := bo.initial
	log.Debug().Str("path", sub.Path).Msg("desktop sse stream: opened")
	defer func() { log.Debug().Str("path", sub.Path).Err(ctx.Err()).Msg("desktop sse stream: ended") }()
	for ctx.Err() == nil {
		resp, err := open(ctx, sub.Path, lastID)
		if err == nil && resp.StatusCode == http.StatusOK {
			delay = bo.initial
			if sendFrame(c, sseFrame{T: "open"}) != nil {
				resp.Body.Close()
				return
			}
			err = readSSE(c, resp.Body, &lastID, &delay)
			resp.Body.Close()
		} else if err == nil {
			status := resp.StatusCode
			resp.Body.Close()
			// EventSource fails the connection on any non-200 and does not
			// retry. A gateway-class answer is the exception: it means the
			// server was not there to answer (restarting), which the
			// reconnect loop exists for.
			if status != http.StatusBadGateway && status != http.StatusServiceUnavailable && status != http.StatusGatewayTimeout {
				_ = sendFrame(c, sseFrame{T: "error", Closed: true, Status: status})
				_ = c.Close()
				return
			}
			err = errors.New("upstream status " + strconv.Itoa(status))
		}
		if ctx.Err() != nil {
			return
		}
		log.Debug().Err(err).Str("path", sub.Path).Dur("retry_in", delay).Msg("desktop sse stream: upstream dropped")
		if sendFrame(c, sseFrame{T: "error"}) != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		if delay < bo.max {
			delay *= 2
			if delay > bo.max {
				delay = bo.max
			}
		}
	}
}

// readSubscribe reads the page's first frame, bounded by subscribeTimeout and
// the stream's own lifetime.
func readSubscribe(c streamConn) (sseSubscribe, bool) {
	type result struct {
		sub sseSubscribe
		err error
	}
	ch := make(chan result, 1)
	go func() {
		var s sseSubscribe
		err := c.ReceiveJSON(&s)
		ch <- result{s, err}
	}()
	t := time.NewTimer(subscribeTimeout)
	defer t.Stop()
	select {
	case r := <-ch:
		return r.sub, r.err == nil
	case <-t.C:
		log.Warn().Msg("desktop sse stream: no subscribe frame; closing")
		return sseSubscribe{}, false
	case <-c.Context().Done():
		return sseSubscribe{}, false
	}
}

func sendFrame(c streamConn, f sseFrame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return c.Send(b)
}

// readSSE parses text/event-stream (event:, multi-line data:, id:, retry:,
// comments) and forwards each dispatched event, carrying the last id seen. It
// returns when the body ends or a send fails. lastID is updated even for events
// that carry no data, as in the EventSource spec.
func readSSE(c streamConn, body io.Reader, lastID *string, delay *time.Duration) error {
	br := bufio.NewReaderSize(body, 64<<10)
	var ev string
	var data []string
	hasData := false
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if hasData {
				if err := sendFrame(c, sseFrame{T: "event", Event: ev, Data: strings.Join(data, "\n"), ID: *lastID}); err != nil {
					return err
				}
			}
			ev, data, hasData = "", nil, false
		case strings.HasPrefix(line, ":"):
			// comment / keepalive
		default:
			name, val, _ := strings.Cut(line, ":")
			val = strings.TrimPrefix(val, " ")
			switch name {
			case "event":
				ev = val
			case "data":
				data = append(data, val)
				hasData = true
			case "id":
				if !strings.Contains(val, "\x00") {
					*lastID = val
				}
			case "retry":
				if ms, err := strconv.Atoi(val); err == nil && ms >= 0 {
					*delay = time.Duration(ms) * time.Millisecond
				}
			}
		}
	}
}
