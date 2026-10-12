//go:build !unix

package cmd

// installGoroutineDumpSignal is a no-op on platforms without SIGUSR1. The
// runtime diagnostics port's pprof goroutine profile remains available.
func installGoroutineDumpSignal(string) func() { return func() {} }

// survivePipedStderrLoss: there is no SIGPIPE here; a write to a pipe whose
// reader is gone fails with an error and nothing ends the process.
func survivePipedStderrLoss() {}
