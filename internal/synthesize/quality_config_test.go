package synthesize

import (
	"testing"

	"knomit/internal/repos"
)

// TestQualityConfigFromRepo verifies that QualityConfigFromRepo maps every
// Q-knob through NewTestInstanceWithDeps and its exported accessors.
// RepoInstance fields are unexported so this test uses the zero-value defaults
// (all 0) and asserts the round-trip: zero-value RepoInstance → zero-value
// QualityConfig. Accessor correctness for non-zero values is covered by
// TestDiscoveryQKnobAccessors in the repos package (which has same-package field
// access). Together they assert: accessors pass through → constructor reads accessors.
func TestQualityConfigFromRepo(t *testing.T) {
	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:        "test-repo",
		AgentBranch: "agent/test",
	})
	got := QualityConfigFromRepo(ri)
	// NewTestInstanceWithDeps zero-initialises the Q-knob fields, so all six
	// must come back as zero values.
	want := QualityConfig{}
	if got != want {
		t.Errorf("QualityConfigFromRepo() = %+v, want %+v", got, want)
	}
}
