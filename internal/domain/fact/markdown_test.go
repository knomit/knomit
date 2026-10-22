package fact

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCheckFenceEdit(t *testing.T) {
	balanced := []string{
		"",
		"no fences here",
		"```go\ncode\n```",
		"text\n  ```\n  indented in a list\n  ```\nmore",
		"inline ``` mid-line is not a fence line",
		"```\na\n```\n\n```bash\nb\n```",
	}
	for _, body := range balanced {
		require.NoError(t, CheckFenceEdit("", body), "body %q", body)
	}

	err := CheckFenceEdit("intro", "intro\n```go\ncode\n```\nmore\n```\ndangling")
	require.Error(t, err)
	require.Contains(t, err.Error(), "line 6", "the error must point at the last fence line")

	// Valid CommonMark with an odd count: a ```` fence quoting a ``` line.
	odd := "````md\n```\n````"
	require.NoError(t, CheckFenceEdit(odd, odd+"\n\nmore prose"),
		"a body that was already odd must stay editable")
	require.NoError(t, CheckFenceEdit(odd, "rewritten without fences"),
		"odd to even is a repair, never refused")
}
