package auth

import "testing"

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
