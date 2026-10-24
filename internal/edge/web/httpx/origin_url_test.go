package httpx

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTrimOriginURL(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"leading spaces", "  https://h/r", "https://h/r"},
		{"trailing spaces", "https://h/r  ", "https://h/r"},
		{"both, with a tab", " \thttps://h/r \t", "https://h/r"},
		{"newline from a paste", "https://h/r\n", "https://h/r"},
		{"CRLF from a paste", "https://h/r\r\n", "https://h/r"},
		{"only whitespace is empty", " \t\n ", ""},
		{"clean is untouched", "https://h/r", "https://h/r"},
		// Everything below CHANGES which repository is named, so none of it is
		// this helper's business — the git route tolerates the suffix and the
		// slash on the serving side instead.
		{"inner spaces are not touched", "https://h/a b", "https://h/a b"},
		{"trailing slash is kept", "https://h/r/", "https://h/r/"},
		{"dot-git suffix is kept", "https://h/r.git", "https://h/r.git"},
		{"case is kept", "https://H/R", "https://H/R"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, TrimOriginURL(tc.in))
		})
	}
}
