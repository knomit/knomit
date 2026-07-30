//go:build desktop

package main

import (
	"context"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// stopGrace bounds how long stop() waits for an in-flight boot to settle
// before giving up on tearing it down. Quit must not hang behind a boot that
// is itself stuck (an embedder downloading model files over a slow link, say).
const stopGrace = 10 * time.Second

// serverBoot runs the knomit server's boot off the main thread and hands its
// address to whoever needs it once it is up.
//
// It exists because booting the server takes seconds — the embedder loads ONNX
// weights, then every repo is opened and its commit log populated — and doing
// that before application.Run() meant the tray icon did not appear until it
// finished. The UI is built and shown first; this carries the boot alongside it.
//
// The zero value is not usable; call startServerBoot.
type serverBoot struct {
	done chan struct{}

	// Written by the boot goroutine before done is closed, and read only after
	// done is observed closed. That ordering is the synchronisation — no mutex.
	apiBase string
	stopFn  func()
	err     error

	once sync.Once
}

// startServerBoot runs boot on a goroutine and returns a handle to its result.
// boot returns the API base URL and a teardown for what it started; a non-nil
// error means nothing was started and the teardown is not called.
func startServerBoot(ctx context.Context, boot func(context.Context) (string, func(), error)) *serverBoot {
	b := &serverBoot{done: make(chan struct{})}
	go func() {
		defer close(b.done)
		b.apiBase, b.stopFn, b.err = boot(ctx)
	}()
	return b
}

// wait blocks until the server is up and returns its API base URL. It returns
// the boot error if the server failed to start, or ctx.Err() if ctx is done
// first — callers pass a request or timeout context so a wedged boot cannot
// pin a caller forever.
func (b *serverBoot) wait(ctx context.Context) (string, error) {
	select {
	case <-b.done:
		return b.apiBase, b.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// stop tears the server down. It is idempotent: Wails calls OnShutdown and
// run() also calls it after Run returns, and only the first call does the work.
//
// A boot still in flight is waited for, because tearing down half a boot is not
// possible — the teardown function does not exist until boot returns. The wait
// is bounded: the process is exiting either way, and a quit that hangs behind a
// stuck boot is worse than a lockfile left behind for the next launch's stale
// check to clear.
func (b *serverBoot) stop() {
	b.once.Do(func() {
		select {
		case <-b.done:
		case <-time.After(stopGrace):
			log.Warn().Dur("grace", stopGrace).
				Msg("shutdown: server boot did not settle in time; skipping teardown")
			return
		}
		if b.stopFn != nil {
			b.stopFn()
		}
	})
}
