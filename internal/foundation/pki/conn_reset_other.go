//go:build !windows

package pki

// isConnReset is false off Windows: there a peer's RST does not discard data
// already received, so the refusal alert that precedes the reset is still
// read (knomit#543 is a Windows-client race). Keeping it false leaves every
// other platform's behaviour exactly as before: no redial.
func isConnReset(error) bool { return false }
