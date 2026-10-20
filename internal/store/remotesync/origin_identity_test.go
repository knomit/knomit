package remotesync

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// What counts as the same origin, and that no credential survives.
// SABOTAGE: compare the raw URL / no path cleaning / scp not mapped /
// credentials kept in an unparseable URL -> red.
func TestOriginIdentity(t *testing.T) {
	same := [][2]string{
		{"https://forge.example/team/kb.git", "https://user:new-token@forge.example/team/kb.git"},
		{"https://forge.example/team/kb.git", "HTTPS://Forge.Example:443/team/kb.git/"},
		{"http://forge.example/kb", "http://forge.example:80/kb"},
		{"/srv/kb.git", "/srv/kb.git/"},
		{"https://forge.example/team/kb", "https://forge.example/team/kb.git"},
		{"https://forge.example/team/kb", "https://forge.example/team/./kb"},
		{"https://forge.example/team/kb", "https://forge.example/team/x/../kb/"},
		{"https://forge.example/team/kb", "https://forge.example//team/kb"},
		{"git@forge.example:team/kb.git", "ssh://git@forge.example/team/kb"},
		{"git@forge.example:team/kb", "ssh://forge.example:22/team/kb.git"},
		{"/srv/kb", "file:///srv/kb"},
		{"/srv/./kb", "file:///srv/kb.git"},
	}
	for _, p := range same {
		require.Equal(t, originIdentity(p[0]), originIdentity(p[1]), "%q vs %q", p[0], p[1])
	}
	// Still a change (accepted as known).
	differ := [][2]string{
		{"https://forge.example/team/kb.git", "https://forge.example/team/fork.git"},
		{"https://forge.example/kb", "https://other.example/kb"},
		{"https://forge.example:8443/kb", "https://forge.example/kb"},
		{"https://forge.example/kb", "ssh://forge.example/kb"},
		{"https://forge.example/Team/kb", "https://forge.example/team/kb"},
		{"http://forge.example/kb", "https://forge.example/kb"},
		{"git@forge.example:team/kb", "git@other.example:team/kb"},
		{"/srv/kb", "/srv/kb2"},
	}
	for _, p := range differ {
		require.NotEqual(t, originIdentity(p[0]), originIdentity(p[1]), "%q vs %q", p[0], p[1])
	}
	// No credential survives, parseable or not.
	for _, raw := range []string{
		"https://user:s3cret@forge.example/kb",
		"https://user:se%zzs3cret@forge.example/kb",
		"https://s3cret@forge.example:99999x/kb",
		"user:s3cret@forge.example:team/kb",
		"https://user:s3cret/x@forge.example/kb",
		"https://user:s3cret#x@forge.example/kb",
		"https://user:s3cret?x@forge.example/kb",
		"https://user:p@s3cret@forge.example/kb",
	} {
		require.NotContains(t, originIdentity(raw), "s3cret", raw)
	}
}
