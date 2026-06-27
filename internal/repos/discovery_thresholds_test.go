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

// TestDiscoveryConfidenceThreshold_HonorsExplicitZero is the regression guard
// for the silent re-defaulting bug. The accessor must return the stored value
// verbatim: an explicit 0 (the only way an operator can disable the confidence
// gate) must NOT be silently rewritten to 0.5. Before the fix, the guard
// `if ri.discoveryConfidenceThreshold <= 0 { return 0.5 }` made the disable
// knob unreachable.
func TestDiscoveryConfidenceThreshold_HonorsExplicitZero(t *testing.T) {
	cases := []struct {
		name   string
		stored float64
		want   float64
	}{
		{"explicit zero disables gate", 0, 0},
		{"configured default", 0.5, 0.5},
		{"explicit higher threshold", 0.8, 0.8},
		{"negative also disables", -0.1, -0.1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ri := &RepoInstance{discoveryConfidenceThreshold: tc.stored}
			if got := ri.DiscoveryConfidenceThreshold(); got != tc.want {
				t.Errorf("DiscoveryConfidenceThreshold() with stored %v = %v, want %v", tc.stored, got, tc.want)
			}
		})
	}
}

// TestNewTestInstanceWithDeps_ConfidenceThresholdDefault ensures test instances
// carry the production default of 0.5 now that the accessor no longer re-defaults
// 0. Without this, every test instance silently runs with the confidence gate
// disabled (accept everything).
func TestNewTestInstanceWithDeps_ConfidenceThresholdDefault(t *testing.T) {
	ri := NewTestInstanceWithDeps(TestInstanceConfig{Name: "t", AgentBranch: "agent/test"})
	if got := ri.DiscoveryConfidenceThreshold(); got != 0.5 {
		t.Errorf("test instance DiscoveryConfidenceThreshold() = %v, want 0.5 (mirror config.Defaults)", got)
	}
}
