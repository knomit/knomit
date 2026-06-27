package repos

import "testing"

// TestDiscoveryBlastRadiusThreshold_HonorsExplicitZero is the regression guard
// for the "0 disables the gate" config contract. The accessor must return the
// stored value verbatim: an explicit 0 (the only value an operator can set to
// turn the backward blast-radius gate OFF) must NOT be silently rewritten to 1.
// Before the fix, `if ri.discoveryBlastRadiusThreshold == 0 { return 1 }` made
// the documented disable knob unreachable.
func TestDiscoveryBlastRadiusThreshold_HonorsExplicitZero(t *testing.T) {
	cases := []struct {
		name   string
		stored int
		want   int
	}{
		{"explicit zero disables", 0, 0},
		{"configured default", 1, 1},
		{"explicit higher threshold", 5, 5},
		{"negative also disables", -1, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ri := &RepoInstance{discoveryBlastRadiusThreshold: tc.stored}
			if got := ri.DiscoveryBlastRadiusThreshold(); got != tc.want {
				t.Errorf("DiscoveryBlastRadiusThreshold() with stored %d = %d, want %d", tc.stored, got, tc.want)
			}
		})
	}
}

// TestNewTestInstanceWithDeps_BlastRadiusDefault ensures test instances (which
// bypass config.Load/Defaults) still carry the production default of 1 now that
// the accessor no longer re-defaults 0. Otherwise every test instance would
// silently run with the backward blast gate disabled.
func TestNewTestInstanceWithDeps_BlastRadiusDefault(t *testing.T) {
	ri := NewTestInstanceWithDeps(TestInstanceConfig{Name: "t", AgentBranch: "agent/test"})
	if got := ri.DiscoveryBlastRadiusThreshold(); got != 1 {
		t.Errorf("test instance DiscoveryBlastRadiusThreshold() = %d, want 1 (mirror config.Defaults)", got)
	}
}
