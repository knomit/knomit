//go:build windows

package main

// The local-listener spelling of a server address on this platform, for the
// usage text (internal/serveraddr parses it).
const (
	localServerForm    = "npipe:////./pipe/<name>"
	localServerExample = "npipe:////./pipe/knomit-0123456789abcdef"
)
