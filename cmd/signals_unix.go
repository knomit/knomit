//go:build unix

package cmd

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/rs/zerolog/log"

	"knomit/internal/platform/crashdump"
)

// installGoroutineDumpSignal wires SIGUSR1 to dump every goroutine's stack to a
// file under dir without exiting the process, so a stuck-but-live server can be
// inspected with `kill -USR1 <pid>`. The returned func stops the handler.
func installGoroutineDumpSignal(dir string) func() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGUSR1)
	go func() {
		for range ch {
			if p, err := crashdump.DumpGoroutines(dir); err != nil {
				log.Warn().Err(err).Msg("goroutine dump failed")
			} else {
				log.Info().Str("path", p).Msg("goroutine dump written (SIGUSR1)")
			}
		}
	}()
	return func() {
		signal.Stop(ch)
		close(ch)
	}
}

// survivePipedStderrLoss keeps a supervised server alive when the pipe its
// stderr is connected to loses its reader: the desktop that started it died.
// By default the Go runtime ends a process that writes to a broken pipe on fd 1
// or 2 with SIGPIPE, so the very log line announcing "supervisor closed stdin"
// killed the server mid-way, before it could stop its listeners and remove
// server.json (observed: a SIGKILLed desktop left a stale server.json and an
// unclean-exit marker behind). With SIGPIPE delivered to a channel instead, a
// write to the dead pipe fails with EPIPE, the log line still reaches the log
// file, and the stdin-EOF shutdown runs to the end.
//
// Notify, not signal.Ignore: an IGNORED signal stays ignored across exec, which
// would change SIGPIPE for every recipe child (a shell pipeline that relies on
// it to stop); a caught one is reset to the default in the child.
func survivePipedStderrLoss() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGPIPE)
	go func() {
		for range ch {
		}
	}()
}

// installReloadSignal runs onReload, on its own goroutine and one call at a
// time, for every SIGHUP the process receives: `kill -HUP <pid>` makes a
// running server re-read its configuration (Listeners.Reload). Signals that
// arrive while a reload runs coalesce into at most one more.
//
// SIGHUP no longer ends the process: a server whose terminal closes keeps
// serving, as a daemon would. The returned func stops the handler and waits
// for a reload in flight; home is unused here (Windows keys its event by it).
func installReloadSignal(home string, onReload func()) (func(), error) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for {
			select {
			case <-ch:
				onReload()
			case <-done:
				return
			}
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
		<-finished
	}, nil
}
