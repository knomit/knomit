package web

import (
	"errors"
	"strings"
	"testing"

	"knomit/internal/federate"
	"knomit/internal/repos"
)

// A panicking mount becomes that mount's error, not the process's death — even
// when the mount has NO RepoInstance to name.
//
// The nil instance is the point. RepoInstance.Name() reads a field, so it
// panics on a nil receiver, and the recover handler used to build its message
// from exactly that call: a mount that panicked BECAUSE its instance was gone
// would panic the handler too, and the second panic escapes the goroutine and
// takes the server down. That is the archive/shutdown-race family the recover
// was written for, so the recover failing on it defeats the whole mechanism.
//
// A nil RepoInstance cannot be produced through the HTTP surface, which is why
// this constructs one directly rather than through a router: the hazard is real
// but only reachable from inside.
func TestFanOutMounts_PanicWithNoRepoInstanceDoesNotEscape(t *testing.T) {
	targets := []federate.Target{
		{RT: repos.ReadTarget{RI: nil, Branch: "agent/gone"}},
	}

	f := fanOutMounts(targets, func(int, federate.Target) (string, error) {
		panic("mount store vanished")
	})

	if f == nil {
		t.Fatal("a panicking mount produced no failure — §9.1 says it must fail the request")
	}
	if f.Mount != 0 {
		t.Errorf("mount: got %d, want 0", f.Mount)
	}
	if !strings.Contains(f.Err.Error(), "mount store vanished") {
		t.Errorf("error %q does not carry the panic value", f.Err)
	}
	// The mount is still identified by whatever identity survived.
	if !strings.Contains(f.Err.Error(), "agent/gone") {
		t.Errorf("error %q does not name the mount", f.Err)
	}
}

// The ordinary case: a mount with an instance is named by repo and branch.
func TestFanOutMounts_PanicNamesTheMount(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha")
	targets := []federate.Target{
		{RT: repos.ReadTarget{RI: m.Get("alpha"), Branch: "machine/test"}},
	}

	f := fanOutMounts(targets, func(int, federate.Target) (string, error) {
		panic("boom")
	})

	if f == nil {
		t.Fatal("a panicking mount produced no failure")
	}
	if !strings.Contains(f.Err.Error(), "alpha@machine/test") {
		t.Errorf("error %q does not name repo@branch", f.Err)
	}
}

// All mounts succeeding reports no failure — the helper's own happy path,
// asserted so the tests above cannot pass by always returning something.
func TestFanOutMounts_AllSucceedingReportsNoFailure(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	targets := []federate.Target{
		{RT: repos.ReadTarget{RI: m.Get("alpha"), Branch: "machine/test"}},
		{RT: repos.ReadTarget{RI: m.Get("beta"), Branch: "machine/test"}},
	}

	seen := make([]int, len(targets))
	if f := fanOutMounts(targets, func(i int, _ federate.Target) (string, error) {
		seen[i] = i + 1
		return "", nil
	}); f != nil {
		t.Fatalf("unexpected failure: %v", f.Err)
	}
	for i, v := range seen {
		if v != i+1 {
			t.Errorf("mount %d never ran (seen=%v)", i, seen)
		}
	}
}

// A panic in ONE mount does not suppress a lower-indexed mount's ordinary
// error: the two failure kinds share one slot array and one ordering rule.
func TestFanOutMounts_PanicDoesNotOutrankALowerIndexedError(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	targets := []federate.Target{
		{RT: repos.ReadTarget{RI: m.Get("alpha"), Branch: "machine/test"}},
		{RT: repos.ReadTarget{RI: m.Get("beta"), Branch: "machine/test"}},
	}

	f := fanOutMounts(targets, func(i int, _ federate.Target) (string, error) {
		if i == 0 {
			return "Failed", errors.New("alpha said no")
		}
		panic("beta exploded")
	})

	if f == nil {
		t.Fatal("no failure reported")
	}
	if f.Mount != 0 || !strings.Contains(f.Err.Error(), "alpha said no") {
		t.Errorf("got mount %d / %v, want mount 0's error — binding order decides, "+
			"not the kind of failure or which arrived first", f.Mount, f.Err)
	}
}
