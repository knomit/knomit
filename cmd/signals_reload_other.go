//go:build !unix && !windows

package cmd

// installReloadSignal: no reload trigger exists on this platform; a config
// change takes a restart.
func installReloadSignal(home string, onReload func()) (func(), error) {
	return func() {}, nil
}
