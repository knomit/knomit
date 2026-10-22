package repos

import "testing"

// TestDiscoveryQKnobAccessors verifies that each Q-knob accessor returns the
// value stored in the corresponding unexported field — no re-defaulting, no
// transformation. The six knobs are: CohFloor, MaxMembers, QualityFloor, WCoh,
// WGap, WSpec.
//
// The test lives in the repos package (same-package access to unexported fields)
// to assert the six-field mapping without importing synthesize (which imports
// repos — import cycle). QualityConfigFromRepo is a trivial field-copy whose
// correctness follows from these accessor assertions.
func TestDiscoveryQKnobAccessors(t *testing.T) {
	t.Run("DiscoveryCohFloor", func(t *testing.T) {
		ri := &RepoInstance{discoveryCohFloor: 0.7}
		if got := ri.DiscoveryCohFloor(); got != 0.7 {
			t.Errorf("DiscoveryCohFloor() = %v, want 0.7", got)
		}
	})

	t.Run("DiscoveryMaxMembers", func(t *testing.T) {
		ri := &RepoInstance{discoveryMaxMembers: 8}
		if got := ri.DiscoveryMaxMembers(); got != 8 {
			t.Errorf("DiscoveryMaxMembers() = %v, want 8", got)
		}
	})

	t.Run("DiscoveryQualityFloor", func(t *testing.T) {
		ri := &RepoInstance{discoveryQualityFloor: 0.3}
		if got := ri.DiscoveryQualityFloor(); got != 0.3 {
			t.Errorf("DiscoveryQualityFloor() = %v, want 0.3", got)
		}
	})

	t.Run("DiscoveryWCoh", func(t *testing.T) {
		ri := &RepoInstance{discoveryWCoh: 2.5}
		if got := ri.DiscoveryWCoh(); got != 2.5 {
			t.Errorf("DiscoveryWCoh() = %v, want 2.5", got)
		}
	})

	t.Run("DiscoveryWGap", func(t *testing.T) {
		ri := &RepoInstance{discoveryWGap: 1.5}
		if got := ri.DiscoveryWGap(); got != 1.5 {
			t.Errorf("DiscoveryWGap() = %v, want 1.5", got)
		}
	})

	t.Run("DiscoveryWSpec", func(t *testing.T) {
		ri := &RepoInstance{discoveryWSpec: 0.8}
		if got := ri.DiscoveryWSpec(); got != 0.8 {
			t.Errorf("DiscoveryWSpec() = %v, want 0.8", got)
		}
	})
}
