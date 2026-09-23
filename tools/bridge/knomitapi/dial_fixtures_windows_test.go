//go:build windows

package knomitapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"net"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"

	"knomit/internal/auth"
)

// The Windows half of the local-transport fixtures. Its unix twin is
// dial_fixtures_unix_test.go, and the suite in dial_test.go is written
// against these names only, so the same eleven tests run on both platforms
// instead of skipping here (knomit#245; they were skipped in 53a0c7b9).

// localListenerName is what this platform calls "a data root's listener" in a
// log line, for test failure messages.
const localListenerName = "named pipe"

// homeForLocalListener is a data root whose resolved listener path this
// platform can actually open. On Windows the pipe name is a hash of the root,
// so the root's own length does not matter and t.TempDir() is fine.
func homeForLocalListener(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// localListenerPath is a listener path for one test's exclusive use. The pipe
// namespace is flat and machine-wide, so uniqueness has to be manufactured:
// t.TempDir() is unique per test and per run, and hashing it keeps the name
// inside what the namespace accepts.
func localListenerPath(t *testing.T) string {
	t.Helper()
	sum := sha256.Sum256([]byte(t.TempDir()))
	return auth.PipePrefix + "knomit-kd-" + hex.EncodeToString(sum[:8])
}

// listenLocal opens a listener a test can serve on.
func listenLocal(t *testing.T, path string) net.Listener {
	t.Helper()
	l, cleanup, err := auth.ListenLocal(path)
	if err != nil {
		t.Fatalf("ListenLocal(%q): %v", path, err)
	}
	t.Cleanup(cleanup)
	return l
}

// unreachableLocalListener is a listener path that EXISTS and cannot be
// dialled — the anomaly the WARN exists for.
//
// It is not a literal translation of the unix fixture, and cannot be: a
// stale socket INODE has no Windows counterpart, because the kernel reaps a
// pipe with the process that created it. The equivalent condition — a
// listener is there, and this bridge cannot use it — is a pipe whose ACL
// excludes us, which is what a knomit running as another user would leave in
// the namespace.
func unreachableLocalListener(t *testing.T) string {
	t.Helper()
	path := localListenerPath(t)
	// SYSTEM only: no ACE matches this process, and a DACL with no matching
	// ACE denies.
	l, err := winio.ListenPipe(path, &winio.PipeConfig{SecurityDescriptor: "D:P(A;;GA;;;SY)"})
	if err != nil {
		t.Fatalf("listen on a SYSTEM-only pipe: %v", err)
	}
	t.Cleanup(func() { l.Close() })

	// POSITIVE CONTROL on the fixture itself. The phase 1 review found two
	// socket fixtures that passed while proving nothing, so this one proves
	// what it claims: the pipe is there, dialling it FAILS, and the failure
	// is NOT "not found" — which would send the client down the quiet Debug
	// branch and make any warn assertion meaningless.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	probe, err := auth.DialLocal(ctx, path, 5*time.Second)
	if err == nil {
		probe.Close()
		t.Fatal("the fixture pipe was dialable, so it is not unreachable and proves nothing")
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the fixture pipe reports as ABSENT (%v), which is the quiet case, not the anomaly this fixture is for", err)
	}
	t.Logf("unreachable-pipe dial error, recorded verbatim: %v", err)
	return path
}

// absentLocalListenerPath is a well-formed listener path with nothing at it:
// the ordinary "no server running" case, which must fall back QUIETLY.
func absentLocalListenerPath(t *testing.T) string {
	t.Helper()
	path := localListenerPath(t)

	// POSITIVE CONTROL: this must be the fs.ErrNotExist case, or the test
	// asserting "no warning" would be asserting it about the wrong branch.
	// Windows reports ERROR_FILE_NOT_FOUND inside an *os.PathError, and
	// syscall.Errno.Is maps it onto fs.ErrNotExist -- verified here rather
	// than assumed, because the whole quiet/loud split keys on it.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := auth.DialLocal(ctx, path, 5*time.Second)
	if err == nil {
		conn.Close()
		t.Fatal("something is listening at the supposedly absent path")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a missing pipe must classify as fs.ErrNotExist or the quiet branch never runs; got %#v", err)
	}
	return path
}

// notAListenerPath exists but is not a local listener. On unix that is a
// regular file where a socket should be; here it is any path outside the pipe
// namespace, which auth.DialLocal refuses rather than opening as a file —
// CreateFile would otherwise happily hand back whatever is there.
func notAListenerPath(t *testing.T) string {
	t.Helper()
	return t.TempDir() + `\knomit.sock`
}
