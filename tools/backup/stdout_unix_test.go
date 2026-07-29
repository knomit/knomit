//go:build unix

package main

import (
	"os"
	"syscall"
	"testing"
	"time"
)

// TestClaimProtocolStreamPutsStderrOnFD1 pins the guard that reassigning
// os.Stdout cannot provide.
//
// Reassigning os.Stdout covers Go code that resolves it at CALL time. It cannot
// cover a package-level variable that captured the real file descriptor 1
// before main ran — litestream.LogWriter is exactly such a variable, dormant in
// v0.5.15 and one upgrade away from writing to the protocol pipe. So the fix is
// at the descriptor level, and so is this test: after claimProtocolStream, a
// write to the raw NUMBER 1 must land in the log, and only the returned handle
// must reach the wire.
//
// The test rewires this process's own fds 1 and 2 and restores them, because
// that is the only way to observe a descriptor-level property.
func TestClaimProtocolStreamPutsStderrOnFD1(t *testing.T) {
	savedOut, err := syscall.Dup(1)
	if err != nil {
		t.Skipf("cannot duplicate fd 1: %v", err)
	}
	savedErr, err := syscall.Dup(2)
	if err != nil {
		syscall.Close(savedOut)
		t.Skipf("cannot duplicate fd 2: %v", err)
	}
	goOut, goErr := os.Stdout, os.Stderr
	t.Cleanup(func() {
		_ = dupOnto(savedOut, 1)
		_ = dupOnto(savedErr, 2)
		syscall.Close(savedOut)
		syscall.Close(savedErr)
		os.Stdout, os.Stderr = goOut, goErr
	})

	// Stand in for what knomit hands the child: fd 1 is the protocol pipe,
	// fd 2 is the log.
	protoR, protoW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	logR, logW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := dupOnto(int(protoW.Fd()), 1); err != nil {
		t.Fatal(err)
	}
	if err := dupOnto(int(logW.Fd()), 2); err != nil {
		t.Fatal(err)
	}
	os.Stdout = os.NewFile(1, "/dev/stdout")
	os.Stderr = os.NewFile(2, "/dev/stderr")

	protocol, err := claimProtocolStream()
	if err != nil {
		t.Fatalf("claimProtocolStream: %v", err)
	}

	// A write to the bare descriptor 1 — what a variable captured before main
	// would do — must reach the LOG.
	if _, err := syscall.Write(1, []byte("stray\n")); err != nil {
		t.Fatalf("write to fd 1: %v", err)
	}
	// The returned handle must reach the WIRE.
	if _, err := protocol.Write([]byte("wire\n")); err != nil {
		t.Fatalf("write to the protocol handle: %v", err)
	}

	if got := readN(t, logR, len("stray\n")); got != "stray\n" {
		t.Errorf("the log received %q, want the stray fd-1 write", got)
	}
	if got := readN(t, protoR, len("wire\n")); got != "wire\n" {
		t.Errorf("the protocol stream received %q, want only the protocol write", got)
	}
}

// readN reads exactly n bytes, so neither pipe has to be closed first.
//
// The deadline matters: when this test FAILS, the bytes went to the other pipe
// and this one is empty forever. Without it a broken redirect hangs the package
// until the test binary's global timeout instead of reporting in a second.
func readN(t *testing.T, f *os.File, n int) string {
	t.Helper()
	if err := f.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	buf := make([]byte, n)
	read := 0
	for read < n {
		got, err := f.Read(buf[read:])
		if err != nil {
			t.Fatalf("read (%q so far): %v", buf[:read], err)
		}
		read += got
	}
	return string(buf)
}
