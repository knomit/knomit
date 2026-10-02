//go:build !windows

package main

// The local-listener spelling of a server address on this platform, for the
// usage text (internal/serveraddr parses it).
const (
	localServerForm    = "unix:///absolute/path/knomit.sock"
	localServerExample = "unix:///Users/me/.knomit/knomit.sock"
)
