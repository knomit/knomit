package web

import "knomit/internal/platform/hostguard"

// loopbackHostOK is hostguard.LoopbackHostOK: the #281 Host rule moved below
// internal/web so the runtime diagnostics port (internal/platform/diag) can
// apply the same rule (#288). See there for what it admits and why.
func loopbackHostOK(host string, listed []string) bool {
	return hostguard.LoopbackHostOK(host, listed)
}
