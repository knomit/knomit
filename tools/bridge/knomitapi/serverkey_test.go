package knomitapi

import "testing"

func TestServerKey(t *testing.T) {
	for _, tc := range []struct{ repo, lens, want string }{
		{"myproj", "", "knomit-repo-myproj"},
		{"", "eng", "knomit-lens-eng"},
		{"myproj", "eng", "knomit-lens-eng"}, // lens wins
		{"knomit", "", "knomit-repo-knomit"}, // prefix applied unconditionally
		// The prefix is applied UNCONDITIONALLY on the repo axis, not skipped
		// when the name already looks prefixed. A one-sided prefix-skip (only
		// on the repo side) would turn this into "knomit-web" instead of
		// "knomit-repo-knomit-web" — an asymmetric variant of the collision
		// ServerKey exists to prevent, and the specific regression this row
		// catches. Do not drop it.
		{"knomit-web", "", "knomit-repo-knomit-web"},
		{"knomitten", "", "knomit-repo-knomitten"}, // a name merely containing "knomit" is not special
		{"lens-eng", "", "knomit-repo-lens-eng"},   // a repo named after the other axis still gets the repo prefix
	} {
		if got := ServerKey(tc.repo, tc.lens); got != tc.want {
			t.Errorf("ServerKey(%q,%q) = %q, want %q", tc.repo, tc.lens, got, tc.want)
		}
	}
}

// A repo and a lens sharing a name must never collide, and no repo name may
// produce a key equal to some lens key.
func TestServerKey_IsInjective(t *testing.T) {
	names := []string{
		"eng", "knomit", "lens-eng", "repo-eng", "a", "web", "knomit-web",
		"knomitten", "lens", "repo", "knomit-lens-eng",
	}
	seen := map[string]string{}
	for _, n := range names {
		for _, axis := range []string{"repo", "lens"} {
			var key string
			if axis == "repo" {
				key = ServerKey(n, "")
			} else {
				key = ServerKey("", n)
			}
			if prev, dup := seen[key]; dup {
				t.Errorf("key %q produced by both %s and %s/%s", key, prev, axis, n)
			}
			seen[key] = axis + "/" + n
		}

		// Lens must win even when a non-empty repo is ALSO passed. The flag
		// layer keeps --repo and --lens mutually exclusive, but that is a
		// caller-side rule; ServerKey itself must still resolve to the lens
		// key if it is ever called with both set, exercised here as part of
		// the injectivity property rather than assumed from the flag guard.
		if got, want := ServerKey("unrelated", n), ServerKey("", n); got != want {
			t.Errorf(`ServerKey("unrelated", %q) = %q, want lens key %q`, n, got, want)
		}
	}
}

// TestServerKey_RepoLensSameNameNoCollision names, directly, the exact
// collision the axis prefix exists to prevent: a repo and a lens sharing the
// same name must never derive the same .mcp.json key, or one entry would
// silently clobber the other. TestServerKey_IsInjective covers this
// generically across many names; this pins the single case call out in
// ServerKey's doc comment so a regression here fails with its own name
// instead of surfacing only as a map hit inside the broader property test.
func TestServerKey_RepoLensSameNameNoCollision(t *testing.T) {
	if repoKey, lensKey := ServerKey("eng", ""), ServerKey("eng", "eng"); repoKey == lensKey {
		t.Errorf("ServerKey(%q, %q) = %q == ServerKey(%q, %q) = %q; repo and lens must never collide",
			"eng", "", repoKey, "eng", "eng", lensKey)
	}
}
