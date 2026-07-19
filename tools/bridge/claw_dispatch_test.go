package main

import "testing"

// The claw subcommand must be reachable without a server for `-h`-style misuse.
func TestClawUnknownSubcommandErrors(t *testing.T) {
	// claw.Run is exercised via its own package tests; here we just smoke-test
	// dispatch by calling clawRunForTest and asserting an unknown subcommand
	// returns an error (no panic), without needing a live server.
	if err := clawRunForTest([]string{"bogus"}); err == nil {
		t.Fatal("expected error for unknown claw subcommand")
	}
}
