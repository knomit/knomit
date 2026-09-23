package auth

import (
	"errors"
	"net"
	"strings"
	"testing"
)

// Platform-neutral ListenLocal behaviour. The socket-shaped fixtures are in
// listen_unix_test.go and the pipe-shaped ones in listen_windows_test.go;
// this holds what must be true of both.

// An unconfigured listener is not an error: callers have no platform branch,
// so ListenLocal has to answer "nothing to open" without one either.
func TestListenLocal_EmptyPathIsNoop(t *testing.T) {
	ln, cleanup, err := ListenLocal("")
	if err != nil || ln != nil {
		t.Fatalf("empty path: %v %v", ln, err)
	}
	cleanup() // must not panic
}

// THE SECOND DOOR on the silent-lockout property (knomit#245 review).
//
// app.checkLocalListener refuses [auth].require = true when no listener is
// CONFIGURED. It cannot see whether one was actually BOUND, and the two
// differ: ListenLocal can return ErrSocketInUse, which both callers treat as
// benign — warn, serve TCP only, carry on. With require = true that produces
// exactly the server Defect A exists to prevent: up, healthy-looking, and
// answering every request 403.
//
// It is reachable without an attacker. On Windows the pipe namespace is flat
// and world-creatable, so ANY process holding the name produces
// ERROR_ACCESS_DENIED, which maps to ErrSocketInUse.
func TestRequireLocalListener(t *testing.T) {
	const path = "/tmp/k/knomit.sock"
	bound := fakeListener{}
	for _, tc := range []struct {
		name      string
		require   bool
		ln        net.Listener
		listenErr error
		wantErr   bool
	}{
		// The regression proper: nothing bound, and require says there must be.
		{"require, listener taken by another process", true, nil, ErrSocketInUse, true},
		{"require, listener failed for any other reason", true, nil, errors.New("boom"), true},
		// Positive controls. Without these the case above would pass for a
		// function that refused every boot.
		{"require, listener bound", true, bound, nil, false},
		{"no require, listener taken", false, nil, ErrSocketInUse, false},
		{"no require, listener bound", false, bound, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := RequireLocalListener(tc.require, tc.ln, path, tc.listenErr)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil {
				return
			}
			// The operator has to be able to act on it: name the setting that
			// caused the refusal and the path that could not be opened.
			// "[auth].require = true" and not "[auth].require", because the
			// remediation clause says "set [auth].require = false" and a
			// bare-name needle is satisfied by that alone.
			for _, want := range []string{"[auth].require = true", path} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error must name %q; got: %v", want, err)
				}
			}
			// The cause travels, so a log line says WHY there was no listener.
			if tc.listenErr != nil && !errors.Is(err, tc.listenErr) {
				t.Fatalf("the underlying listen failure must be wrapped; got: %v", err)
			}
		})
	}
}

// fakeListener stands in for a bound listener: RequireLocalListener only asks
// whether there IS one, so nothing here needs to work.
type fakeListener struct{ net.Listener }
