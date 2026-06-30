//go:build unix

package cmd

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/rs/zerolog/log"

	"knomit/internal/crashdump"
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
