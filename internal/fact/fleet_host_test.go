package fact

import "testing"

// The master marker is the agent id on one line and nothing else.
// SABOTAGE: accept a second line -> red.
func TestFleetMaster_OneAgentIDOnOneLine(t *testing.T) {
	if got := RenderFleetMaster("mindev.local-6c927ab0"); got != "mindev.local-6c927ab0\n" {
		t.Fatalf("render: %q", got)
	}
	if id, err := ParseFleetMaster([]byte("mindev.local-6c927ab0\n")); err != nil || id != "mindev.local-6c927ab0" {
		t.Fatalf("parse: %q %v", id, err)
	}
	for _, bad := range []string{"", "\n", "a\nb\n", "a b\n"} {
		if _, err := ParseFleetMaster([]byte(bad)); err == nil {
			t.Fatalf("%q parsed", bad)
		}
	}
}
