//go:build !unix

package cmd

// installGoroutineDumpSignal is a no-op on platforms without SIGUSR1. The
// runtime diagnostics port's pprof goroutine profile remains available.
func installGoroutineDumpSignal(string) func() { return func() {} }
