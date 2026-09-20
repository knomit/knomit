//go:build desktop && !windows

package main

// attachParentConsole does nothing off Windows. macOS and Linux link the
// desktop binary the ordinary way, so it keeps the stdout and stderr it was
// launched with and `knomit-desktop version` prints to the terminal without
// any help.
//
// Named _others rather than _unix so every non-Windows build has a definition,
// whatever GOOS it is — the same reason reveal_others.go and restart_others.go
// carry the suffix.
func attachParentConsole() {}
