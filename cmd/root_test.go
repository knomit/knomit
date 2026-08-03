package cmd

import (
	"sort"
	"testing"
)

// TestRootRegistersExactlyTheShippedCommands pins the CLI surface, and exists
// mainly to keep `reset` from coming back by reflex.
//
// reset deleted a repo's .db file directly. That predated the control.db
// registry, and once the registry became authoritative the command stopped
// meaning what its name said: the row survived the delete, so the next boot
// either re-cloned the repo straight back from its recorded origin or logged it
// forever as registered-but-unrebuildable. With backup on it did not even clear
// the replica. Removing a repo goes through the registry instead — DELETE
// /api/v1/repos/{repo} to archive, DELETE /api/v1/archived/{id} to purge.
//
// An exact-set assertion rather than a "reset is absent" one, so a command added
// without a decision behind it is also visible here.
func TestRootRegistersExactlyTheShippedCommands(t *testing.T) {
	var got []string
	for _, c := range RootCmd().Commands() {
		got = append(got, c.Name())
	}
	sort.Strings(got)

	want := []string{"serve", "verify", "version", "warm-models"}
	sort.Strings(want)

	if len(got) != len(want) {
		t.Fatalf("registered commands = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("registered commands = %v, want %v", got, want)
		}
	}
}
