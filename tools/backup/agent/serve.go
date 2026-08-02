package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"knomit/internal/backup/proto"
)

// Serve reads newline-delimited JSON requests from in and writes one response
// line per request to out, until in reaches EOF.
//
// Every request is handled in its own goroutine, so a slow restore never queues
// a cheap status behind it — responses carry the request id, and the client
// correlates. Ordering is therefore NOT preserved, by design.
//
// EOF is the shutdown signal, and the important one: when knomit dies — for any
// reason, including SIGKILL, where no handler of ours can run — the write end
// of this pipe closes and the read here returns EOF. The agent then closes its
// stores (a final replica sync per database) and exits, so a killed parent can
// never leave an orphan replicating on its behalf.
//
// Serve waits for in-flight handlers before closing, so a request accepted just
// before EOF still completes.
func (a *Agent) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	br := bufio.NewReader(in)
	w := &lineWriter{w: out}

	var wg sync.WaitGroup
	var readErr error
	for {
		line, err := proto.ReadLine(br, proto.MaxLineBytes)
		if errors.Is(err, proto.ErrLineTooLong) {
			// The oversized line has been drained through its newline, so the
			// stream is resynchronised: report and keep serving. Wedging the
			// channel on one bad line would silently end replication.
			w.write(&proto.Response{
				OK:    false,
				Code:  proto.CodeBadRequest,
				Error: fmt.Sprintf("request line exceeds the %d byte maximum", proto.MaxLineBytes),
			})
			continue
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				readErr = err
			}
			break
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}

		var req proto.Request
		if err := json.Unmarshal(line, &req); err != nil {
			// PeekID so a waiter still gets its answer where the id survived
			// the malformed payload; 0 when it did not, which the client logs.
			w.write(&proto.Response{
				ID:    proto.PeekID(line),
				OK:    false,
				Code:  proto.CodeBadRequest,
				Error: fmt.Sprintf("malformed request: %v", err),
			})
			continue
		}

		wg.Add(1)
		go func(req proto.Request) {
			defer wg.Done()
			w.write(a.dispatch(ctx, req))
		}(req)
	}

	wg.Wait()
	closeErr := a.Close(context.Background())
	if readErr != nil {
		return readErr
	}
	return closeErr
}

// lineWriter serialises response lines onto one stream. Handlers run
// concurrently, and two interleaved partial writes would produce a line neither
// side can parse.
type lineWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lineWriter) write(resp *proto.Response) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// A failed write means the client is gone; there is nowhere to report it,
	// and the read loop will see EOF momentarily.
	_ = proto.WriteLine(l.w, resp)
}

// dispatch runs one request and builds its response. It never panics out into
// the serve loop: a handler crash would otherwise take down replication for
// every database, so it is converted into a failed response for that request
// alone.
func (a *Agent) dispatch(ctx context.Context, req proto.Request) (resp *proto.Response) {
	defer func() {
		if r := recover(); r != nil {
			a.logger.Error("panic while handling a request", "method", req.Method, "panic", fmt.Sprint(r))
			resp = &proto.Response{
				ID:    req.ID,
				OK:    false,
				Code:  proto.CodeInternal,
				Error: fmt.Sprintf("panic handling %s: %v", req.Method, r),
			}
		}
	}()

	result, err := a.handle(ctx, req)
	if err != nil {
		a.logger.Warn("request failed", "method", req.Method, "id", req.ID, "err", err.Error())
		return &proto.Response{ID: req.ID, OK: false, Code: codeOf(err), Error: err.Error()}
	}
	var raw json.RawMessage
	if result != nil {
		b, merr := json.Marshal(result)
		if merr != nil {
			return &proto.Response{
				ID: req.ID, OK: false, Code: proto.CodeInternal,
				Error: fmt.Sprintf("encoding the %s result: %v", req.Method, merr),
			}
		}
		raw = b
	}
	return &proto.Response{ID: req.ID, OK: true, Result: raw}
}

// handle routes one request to its method.
func (a *Agent) handle(ctx context.Context, req proto.Request) (any, error) {
	switch req.Method {
	case proto.MethodOpen:
		var p proto.OpenParams
		if err := decode(req.Params, &p); err != nil {
			return nil, err
		}
		return nil, a.Open(ctx, p.Config)

	case proto.MethodTrack:
		var p proto.TrackParams
		if err := decode(req.Params, &p); err != nil {
			return nil, err
		}
		return nil, a.Track(ctx, p)

	case proto.MethodUntrack:
		var p proto.UntrackParams
		if err := decode(req.Params, &p); err != nil {
			return nil, err
		}
		return nil, a.Untrack(p.Name)

	case proto.MethodStatus:
		dbs, err := a.Status(ctx)
		if err != nil {
			return nil, err
		}
		return proto.StatusResult{Databases: dbs}, nil

	case proto.MethodRestore:
		var p proto.RestoreParams
		if err := decode(req.Params, &p); err != nil {
			return nil, err
		}
		restored, err := a.Restore(ctx, p)
		if err != nil {
			return nil, err
		}
		return proto.RestoreResult{Restored: restored}, nil

	case proto.MethodResetLocalState:
		var p proto.ResetLocalStateParams
		if err := decode(req.Params, &p); err != nil {
			return nil, err
		}
		return nil, a.ResetLocalState(ctx, p.Path)

	case proto.MethodDeleteReplica:
		var p proto.DeleteReplicaParams
		if err := decode(req.Params, &p); err != nil {
			return nil, err
		}
		return nil, a.DeleteReplica(ctx, p.Rel)

	case proto.MethodClose:
		return nil, a.Close(ctx)

	default:
		return nil, withCode(proto.CodeUnknownMethod,
			fmt.Errorf("unknown method %q", req.Method))
	}
}

// decode unmarshals a request's params, reporting a malformed payload as a
// bad request rather than an internal failure.
func decode(raw json.RawMessage, into any) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return withCode(proto.CodeBadRequest, fmt.Errorf("decoding params: %w", err))
	}
	return nil
}
