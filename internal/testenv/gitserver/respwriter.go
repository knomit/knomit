package gitserver

import (
	"net"
	"net/http"
	"time"
)

// faultWriter wraps a ResponseWriter to throttle and/or truncate the body.
//
// Truncation strategy: when the byte budget is exhausted we hijack the
// underlying TCP connection and close it immediately. This produces a hard
// connection drop that git (and go-git) reliably sees as a broken/incomplete
// packfile, regardless of git version. The alternative — returning
// http.ErrBodyNotAllowed — is silently swallowed by net/http's chunked encoder
// and never reaches the client as an error, so it is not used here.
type faultWriter struct {
	http.ResponseWriter
	truncateAfter int // 0 = unlimited
	written       int
	perWrite      int // 0 = no throttle
	delay         time.Duration
}

func (w *faultWriter) Write(b []byte) (int, error) {
	// Truncate: if the budget is already spent, close the socket hard.
	if w.truncateAfter > 0 && w.written >= w.truncateAfter {
		w.closeConn()
		// Return a non-nil error so the CGI handler's write loop exits.
		return 0, net.ErrClosed
	}

	// Clip the slice to fit within the remaining budget.
	if w.truncateAfter > 0 && w.written+len(b) > w.truncateAfter {
		b = b[:w.truncateAfter-w.written]
	}

	var (
		total int
		err   error
	)

	if w.perWrite > 0 {
		for len(b) > 0 {
			chunk := w.perWrite
			if chunk > len(b) {
				chunk = len(b)
			}
			var m int
			m, err = w.ResponseWriter.Write(b[:chunk])
			total += m
			w.written += m
			if f, ok := w.ResponseWriter.(http.Flusher); ok {
				f.Flush()
			}
			if err != nil {
				return total, err
			}
			time.Sleep(w.delay)
			b = b[chunk:]
		}
	} else {
		total, err = w.ResponseWriter.Write(b)
		w.written += total
	}

	// After writing the clipped slice, if we've hit the budget, close now so
	// the client sees an abrupt EOF rather than a clean response end.
	if w.truncateAfter > 0 && w.written >= w.truncateAfter {
		w.closeConn()
	}

	return total, err
}

func (w *faultWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// closeConn hijacks the underlying TCP connection and closes it, producing a
// hard connection reset visible to the remote client.
func (w *faultWriter) closeConn() {
	hj, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		return
	}
	conn.Close()
}
