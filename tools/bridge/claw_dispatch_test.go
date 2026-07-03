package main

import "testing"

// The claw subcommand must be reachable without a server for `-h`-style misuse.
func TestClawUnknownSubcommandErrors(t *testing.T) {
	// claw.Run is exercised via its own package tests; here we assert the
	// dispatch constant exists by importing the symbol indirectly.
	// Smoke: unknown subcommand returns an error (no panic).
	if err := clawRunForTest([]string{"bogus"}); err == nil {
		t.Fatal("expected error for unknown claw subcommand")
	}
}
