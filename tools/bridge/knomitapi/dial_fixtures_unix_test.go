//go:build !windows

package knomitapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"knomit/internal/auth"
)

// The unix half of the local-transport fixtures. Its Windows twin is
// dial_fixtures_windows_test.go, and the suite in dial_test.go is written
// against these names only, so the same eleven tests run on both platforms.

// localListenerName is what this platform calls "a data root's listener" in a
// log line, for test failure messages.
const localListenerName = "unix socket"

// homeForLocalListener is a data root whose resolved listener path this
// platform can actually open. Under /tmp and not t.TempDir(): macOS caps
// sun_path at 104 bytes and t.TempDir() overruns it, which the phase 1 review
// found passing a fixture that proved nothing.
func homeForLocalListener(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "kh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// localListenerPath is a listener path for one test's exclusive use.
func localListenerPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(homeForLocalListener(t), "knomit.sock")
}

// listenLocal opens a listener a test can serve on.
func listenLocal(t *testing.T, path string) net.Listener {
	t.Helper()
	l, err := auth.ListenLocal(path)
	if err != nil {
		t.Fatalf("ListenLocal(%q): %v", path, err)
	}
	return l
}

// unreachableLocalListener is a listener path that EXISTS and cannot be
// dialled — the anomaly the WARN exists for. Here that is a socket INODE with
// nothing accepting on it, which is what an ungraceful server exit leaves
// behind: cmd/serve.go only unlinks the socket on a graceful one.
func unreachableLocalListener(t *testing.T) string {
	t.Helper()
	path := localListenerPath(t)
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	// Go unlinks a unix socket on Close by default, which is precisely what a
	// CRASHING server does not do. Turning that off reproduces the real
	// leftover: the inode survives, so os.Stat still reports a socket and a
	// dial gets ECONNREFUSED.
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	if st, serr := os.Stat(path); serr != nil || st.Mode()&os.ModeSocket == 0 {
		t.Fatalf("expected a stale SOCKET to remain at %s (err=%v)", path, serr)
	}

	// POSITIVE CONTROL on the fixture itself: the socket is there, dialling
	// it FAILS, and the failure is NOT "not found" — which would send the
	// client down the quiet Debug branch and make a warn assertion vacuous.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	probe, err := auth.DialLocal(ctx, path, 5*time.Second)
	if err == nil {
		probe.Close()
		t.Fatal("the fixture socket was dialable, so it is not unreachable and proves nothing")
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the fixture socket reports as ABSENT (%v), which is the quiet case, not the anomaly this fixture is for", err)
	}
	return path
}

// absentLocalListenerPath is a well-formed listener path with nothing at it:
// the ordinary "no server running" case, which must fall back QUIETLY.
func absentLocalListenerPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(homeForLocalListener(t), "absent.sock")

	// POSITIVE CONTROL: this must be the fs.ErrNotExist case, or the test
	// asserting "no warning" would be asserting it about the wrong branch. A
	// path too long for sun_path fails with EINVAL instead, which is a
	// misconfiguration and SHOULD warn — see
	// TestNewHTTPClient_OverlongSocketPathStillWarns.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := auth.DialLocal(ctx, path, 5*time.Second)
	if err == nil {
		conn.Close()
		t.Fatal("something is listening at the supposedly absent path")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a missing socket must classify as fs.ErrNotExist or the quiet branch never runs; got %#v", err)
	}
	return path
}

// notAListenerPath exists but is not a local listener: a stale regular file
// where a socket should be.
func notAListenerPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(homeForLocalListener(t), "knomit.sock")
	if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A path too long for sun_path fails with EINVAL, not ENOENT. That is a
// misconfiguration and not the ordinary "no server running" case, so it must
// still WARN — this pins that the quiet path is keyed on ENOENT specifically
// and not on "any failure to reach the socket".
//
// THERE IS NO WINDOWS COUNTERPART, deliberately: pipe names have no sun_path
// equivalent to overrun. The Windows analogue of "an unusable local path must
// warn rather than go quiet" is a path outside the pipe namespace, which
// TestNewHTTPClient_NonListenerPathFallsBackToTCP covers on both platforms.
func TestNewHTTPClient_OverlongSocketPathStillWarns(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the sun_path cap that produces EINVAL here is a darwin limit")
	}
	isolateHome(t)
	tcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "via-tcp")
	}))
	defer tcp.Close()

	var logbuf bytes.Buffer
	restore := log.Logger
	log.Logger = zerolog.New(&logbuf).Level(zerolog.DebugLevel)
	t.Cleanup(func() { log.Logger = restore })

	overlong := filepath.Join(t.TempDir(), strings.Repeat("x", 120)+".sock")
	c := NewHTTPClient(overlong, false, 2*time.Second)
	if got := get(t, c, tcp.URL+"/anything"); got != "via-tcp" {
		t.Fatalf("it must still fall back; body=%q", got)
	}
	if !strings.Contains(logbuf.String(), "unreachable") {
		t.Fatalf("an unusable socket path is an anomaly and must warn, got: %s", logbuf.String())
	}
}
