//go:build windows

package knomitapi

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"knomit/internal/auth"
)

// knomit#265: a local listener owned by ANOTHER account is a failure, not a
// reason to fall back to TCP. The squatter is a real listener of this test's
// own, with auth told to refuse every owner — a pipe owned by another account
// cannot be created without privileges the suite does not have. Windows only
// because only the Windows DialLocal checks the owner; a unix socket's
// directory already gates who can create it.
func TestSocketPreferringClient_ForeignListenerIsNotATCPFallback(t *testing.T) {
	isolateHome(t)
	var tcpHits atomic.Int32
	tcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tcpHits.Add(1)
		_, _ = io.WriteString(w, "via-tcp")
	}))
	t.Cleanup(tcp.Close)
	sock := serveLocal(t, "via-socket")
	logbuf := captureLog(t)
	t.Cleanup(auth.RefuseEveryPipeOwnerForTest())

	c := NewHTTPClient(sock, false, 5*time.Second)
	for i := 0; i < 2; i++ {
		resp, err := c.Get(tcp.URL + "/anything")
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("request %d succeeded (body %q); a foreign listener must fail it", i, b)
		}
		if !errors.Is(err, auth.ErrForeignListener) {
			t.Fatalf("request %d: want auth.ErrForeignListener in the chain, got %v", i, err)
		}
		// The escape hatch is the BRIDGE's advice, added where it refuses.
		for _, want := range []string{"server argument", "KNOMIT_SERVER"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("request %d: the error must name %q as the way past the pipe; got %v", i, want, err)
			}
		}
		c.CloseIdleConnections() // the second request must dial afresh
	}
	if n := tcpHits.Load(); n != 0 {
		t.Fatalf("the TCP server received %d request(s); a foreign listener must not fall back to TCP", n)
	}

	out := logbuf.String()
	const line = "local listener is held by another account"
	if got := strings.Count(out, line); got != 1 {
		t.Fatalf("the refusal must be logged at WARN exactly once over two dials, got %d: %s", got, out)
	}
	if !strings.Contains(out, `"level":"warn"`) || !strings.Contains(out, jsonEscaped(sock)) {
		t.Fatalf("the refusal must be a WARN naming the listener path %q, got: %s", sock, out)
	}
	if strings.Contains(out, "local listener unreachable") {
		t.Fatalf("the unreachable-listener fallback line must not appear for a foreign one: %s", out)
	}
}
