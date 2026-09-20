//go:build desktop

package main

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
)

// bootPhase names the stage bootKnomit is in. It is what the boot screen shows
// while there is no API to ask anything else of — see GET /boot/status in
// configInjectingHandler.
//
// These strings are a WIRE FORMAT: web/src/boot.ts maps each one to the
// sentence the user reads, and an unknown value there falls back to a generic
// "Starting…". Renaming one without changing that map silently degrades the
// boot screen rather than breaking it, so change both together.
type bootPhase string

const (
	phaseStarting          bootPhase = "starting"
	phaseInstallingTools   bootPhase = "installing-tools"
	phaseDownloadingModels bootPhase = "downloading-models"
	phaseStartingEngine    bootPhase = "starting-engine"
	phaseStartingServer    bootPhase = "starting-server"
	phaseReady             bootPhase = "ready"
	phaseFailed            bootPhase = "failed"
)

// bootStatus is the JSON GET /boot/status answers with.
//
// APIBase is carried here rather than making the client re-fetch /config.js
// once the boot finishes: the client already has to poll this endpoint, and one
// field costs nothing next to a second script fetch whose only job is to set the
// same string.
type bootStatus struct {
	Ready   bool   `json:"ready"`
	Phase   string `json:"phase"`
	APIBase string `json:"api_base,omitempty"`
	Error   string `json:"error,omitempty"`
}

// stopGrace bounds how long stop() waits for an in-flight boot to settle
// before giving up on tearing it down. Quit must not hang behind a boot that
// is itself stuck (an embedder downloading model files over a slow link, say).
//
// A var rather than a const so tests can shorten it. That is not a cosmetic
// concession: the branch it guards — stop() returning WITHOUT running the
// teardown — is the one NativeService.releaseInstance's explicit
// lockfile.Remove exists to cover, and at ten seconds it was unreachable from
// any test, which left the safety argument in native.go resting on unexercised
// code. Only tests write it, and only under t.Cleanup.
var stopGrace = 10 * time.Second

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

	// phase, unlike the three fields above, is read WHILE the boot is still
	// running — that is the whole point of it — so it cannot lean on the
	// close(done) ordering and needs its own atomic. Holds a bootPhase.
	phase atomic.Value

	once sync.Once
}

// startServerBoot runs boot on a goroutine and returns a handle to its result.
// boot returns the API base URL and a teardown for what it started; a non-nil
// error means nothing was started and the teardown is not called.
//
// boot is handed a setPhase callback rather than the *serverBoot itself, so the
// only thing it can do to this handle is narrate its own progress.
func startServerBoot(ctx context.Context, boot func(context.Context, func(bootPhase)) (string, func(), error)) *serverBoot {
	b := &serverBoot{done: make(chan struct{})}
	b.phase.Store(phaseStarting)
	go func() {
		defer close(b.done)
		b.apiBase, b.stopFn, b.err = boot(ctx, b.setPhase)
	}()
	return b
}

// setPhase records which stage the boot has reached.
func (b *serverBoot) setPhase(p bootPhase) {
	b.phase.Store(p)
	log.Info().Str("phase", string(p)).Msg("desktop boot phase")
}

// status reports the boot's progress WITHOUT blocking, for GET /boot/status.
//
// The non-blocking select is what preserves the struct's synchronisation
// argument: apiBase and err are read only on the branch where done is already
// closed, which is exactly the condition their happens-before rests on. Reading
// them in the default branch would be a data race even though the values look
// settled.
func (b *serverBoot) status() bootStatus {
	select {
	case <-b.done:
		if b.err != nil {
			return bootStatus{Phase: string(phaseFailed), Error: b.err.Error()}
		}
		return bootStatus{Ready: true, Phase: string(phaseReady), APIBase: b.apiBase}
	default:
		p, _ := b.phase.Load().(bootPhase)
		return bootStatus{Phase: string(p)}
	}
}

// wait blocks until the server is up and returns its API base URL. It returns
// the boot error if the server failed to start, or ctx.Err() if ctx is done
// first — callers pass a request or timeout context so a wedged boot cannot
// pin a caller forever.
//
// Its one caller is run()'s badge/fatal-dialog goroutine, which genuinely wants
// to sleep until the boot settles. Anything serving an HTTP REQUEST wants
// status() instead: blocking a request on this is what used to hold /config.js,
// and through it the whole document, for the length of a first-launch download.
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
