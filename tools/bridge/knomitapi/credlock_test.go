package knomitapi

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// N5 (3a review): waiting for the cross-process refresh lock honours ctx. A
// holder that is stopped (SIGSTOP) or stuck in its 30 s refresh must not
// stall every other kb past its own deadline. This test runs on every OS:
// flock on darwin/linux and LockFileEx on Windows both conflict between two
// open handles of the same file, even in one process — so the Windows
// implementation is exercised by the windows CI job, not by this box.
func TestCredentialsLock_WaitHonoursContext(t *testing.T) {
	useHome(t)
	u, _ := url.Parse("http://127.0.0.1:19280")
	p, err := CredentialsPath(u)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	// Another kb holds the lock.
	holder, err := os.OpenFile(p+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := lockFile(context.Background(), holder); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	ran := false
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- withCredentialsLock(ctx, u, func() error { ran = true; return nil }) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) || ran {
			t.Fatalf("err = %v ran = %v; want the ctx deadline and fn not run", err, ran)
		}
		if waited := time.Since(start); waited > 2*time.Second {
			t.Fatalf("returned after %s; the deadline was 200ms", waited)
		}
	case <-time.After(5 * time.Second):
		_ = unlockFile(holder) // let the stuck waiter finish
		<-done
		t.Fatal("the lock wait ignored ctx: still blocked 5 s after a 200 ms deadline")
	}

	// And once the holder lets go, a waiter gets the lock.
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(100 * time.Millisecond)
		_ = unlockFile(holder)
	}()
	// The kernel lock is no Go happens-before edge: join the releaser before
	// the deferred Close touches the same file.
	defer func() { <-released }()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if err := withCredentialsLock(ctx2, u, func() error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("after release: err = %v ran = %v", err, ran)
	}
}
