package vcs

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/require"
)

// Test 4d: prefix rules.
func TestNormalizeChangesPrefix(t *testing.T) {
	for _, in := range []string{"tasks/a", "tasks/a/", "/tasks/a", "Tasks/A", "/TASKS/a/"} {
		got, err := NormalizeChangesPrefix("kb", in)
		require.NoError(t, err, in)
		require.Equal(t, "kb/tasks/a", got, in)
	}
	got, err := NormalizeChangesPrefix("kb", "")
	require.NoError(t, err)
	require.Equal(t, "kb", got, "an empty prefix is the whole ontology root")

	got, err = NormalizeChangesPrefix("Facts", "x")
	require.NoError(t, err)
	require.Equal(t, "Facts/x", got, "the configured root is joined as-is, never a literal kb")

	got, err = NormalizeChangesPrefix("kb", "tasks/a.md")
	require.NoError(t, err)
	require.Equal(t, "kb/tasks/a.md", got, "no .md is added; a folder named like a file is taken literally")

	for _, bad := range []string{"../x", "tasks/../x", "tasks/.drafts", ".knomit/inbox", "tasks//a"} {
		_, err := NormalizeChangesPrefix("kb", bad)
		require.ErrorIs(t, err, ErrInvalidPrefix, bad)
	}
}

// The cursor records the repo and branch it was minted for. A cursor missing
// either (or both) was never minted and is refused.
func TestChangesCursor_ScopeRoundTrips(t *testing.T) {
	head := "0123456789abcdef0123456789abcdef01234567"
	scope := ChangesScope{Repo: "aaaaaaaaaaaa", Branch: "exp/x"}
	q, got, err := DecodeChangesCursor(EncodeChangesCursor(scope, "s", head, "tasks/a", "kb/tasks/a/t1.md"))
	require.NoError(t, err)
	require.Equal(t, scope, got)
	require.Equal(t, ChangesQuery{Since: "s", Head: head, Prefix: "tasks/a", After: "kb/tasks/a/t1.md"}, q)

	noScope := base64.RawURLEncoding.EncodeToString([]byte(`{"h":"` + head + `","a":"kb/x.md"}`))
	_, _, err = DecodeChangesCursor(noScope)
	require.ErrorIs(t, err, ErrInvalidChangesCursor)

	noRepo := base64.RawURLEncoding.EncodeToString([]byte(`{"b":"main","h":"` + head + `","a":"kb/x.md"}`))
	_, _, err = DecodeChangesCursor(noRepo)
	require.ErrorIs(t, err, ErrInvalidChangesCursor)

	half := base64.RawURLEncoding.EncodeToString([]byte(`{"r":"aaaaaaaaaaaa","h":"` + head + `","a":"kb/x.md"}`))
	_, _, err = DecodeChangesCursor(half)
	require.ErrorIs(t, err, ErrInvalidChangesCursor)
}

// since takes the bookmark form or a bare commit; a malformed repo half is
// ErrUnknownSince, never an empty page.
func TestParseChangesSince(t *testing.T) {
	head := "0123456789abcdef0123456789abcdef01234567"
	repo, commit, err := ParseChangesSince(ChangesBookmark("AAAAAAAAAAAA", head))
	require.NoError(t, err)
	require.Equal(t, "aaaaaaaaaaaa", repo)
	require.Equal(t, head, commit)

	repo, commit, err = ParseChangesSince(head)
	require.NoError(t, err)
	require.Empty(t, repo, "a bare commit names no repo")
	require.Equal(t, head, commit)

	for _, bad := range []string{"core:" + head, "aaaaaaaaaaaaa:" + head, ":" + head,
		"aaaaaaaaaaaa:", "aaaaaaaaaaaa:abc", "aaaaaaaaaaaa:" + head + "0", "aaaaaaaaaaaa:" + head[:39] + "z"} {
		_, _, err = ParseChangesSince(bad)
		require.ErrorIs(t, err, ErrUnknownSince, bad)
	}
}
