package app

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// AgentSlug is the one place a branch name becomes a category key, and the
// cases below are the ones sanitizeHostname does NOT already handle. That is
// the trap this test exists to pin: sanitizeHostname replaces only the
// characters git rejects in a ref name, so a dot, an underscore and an
// uppercase letter travel intact into the branch and would travel intact into
// the key if AgentSlug did not normalise them.
func TestAgentSlug(t *testing.T) {
	cases := []struct {
		name   string
		branch string
		want   string
	}{
		{
			// The shipping case: macOS hostnames are dotted, and
			// sanitizeHostname leaves the dot alone.
			name:   "dotted hostname",
			branch: "agent/mindev.local-8ef0cd32",
			want:   "mindev-local-8ef0cd32",
		},
		{
			// Neither uppercase nor underscore is invalid in a git ref, so
			// both reach AgentSlug unchanged.
			name:   "uppercase and underscore",
			branch: "agent/Build_Box-8ef0cd32",
			want:   "build-box-8ef0cd32",
		},
		{
			// A hostname sanitizeHostname already touched arrives carrying
			// hyphens; adjacent replacements must not produce a run.
			name:   "already sanitized, runs collapse",
			branch: "agent/my--host.:box-8ef0cd32",
			want:   "my-host-box-8ef0cd32",
		},
		{
			// Only the "agent/" prefix is stripped. A bare name is slugified
			// as-is rather than rejected — a slug is a key, not a validation.
			name:   "no agent prefix",
			branch: "mindev.local-8ef0cd32",
			want:   "mindev-local-8ef0cd32",
		},
		{
			// The prefix must be stripped BEFORE normalisation, or the
			// separator survives as a leading hyphen on every slug.
			name:   "slash never survives as a hyphen",
			branch: "agent/host-abc",
			want:   "host-abc",
		},
		{
			// TrimPrefix is case-sensitive, so the lowercasing has to happen
			// first or this keeps its prefix and slugs to "agent-host-abc".
			name:   "uppercase agent prefix is still a prefix",
			branch: "Agent/host-abc",
			want:   "host-abc",
		},
		{
			// A slash anywhere else is not a prefix and collapses like any
			// other invalid rune.
			name:   "inner slash collapses",
			branch: "exp/feat/signal-type",
			want:   "exp-feat-signal-type",
		},
		{
			name:   "leading and trailing junk is trimmed",
			branch: "agent/.host.",
			want:   "host",
		},
		{
			name:   "multi-byte runes collapse rather than survive",
			branch: "agent/café-8ef0cd32",
			want:   "caf-8ef0cd32",
		},
		{
			name:   "empty",
			branch: "",
			want:   "",
		},
		{
			// No [a-z0-9] rune at all. Documented to return "", which a caller
			// building inbox/<slug>/ must reject rather than treat as a path.
			name:   "all punctuation yields the empty slug",
			branch: "agent/...",
			want:   "",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, AgentSlug(c.branch))
		})
	}
}

// The slug of a real branch built by agentBranch must be stable and contain
// nothing outside [a-z0-9-] — asserted against the composed value rather than
// a literal, so this keeps holding if the branch format changes.
func TestAgentSlug_OfComposedAgentBranch(t *testing.T) {
	branch := agentBranch("8ef0cd32")
	slug := AgentSlug(branch)

	require.NotEmpty(t, slug)
	require.Regexp(t, `^[a-z0-9]+(-[a-z0-9]+)*$`, slug,
		"a slug is [a-z0-9-] with no leading, trailing or doubled hyphen")
	require.Equal(t, slug, AgentSlug(branch), "AgentSlug is pure")
	require.Equal(t, slug, AgentSlug(slug), "slugging a slug is a no-op")
}
