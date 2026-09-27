package fact

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCheckFences(t *testing.T) {
	ok := []string{
		"",
		"no fences here",
		"```go\ncode\n```",
		"text\n  ```\n  indented in a list\n  ```\nmore",
		"inline ``` mid-line is not a fence line",
		"```\na\n```\n\n```bash\nb\n```",
	}
	for _, body := range ok {
		require.NoError(t, CheckFences(body), "body %q", body)
	}

	err := CheckFences("intro\n```go\ncode\n```\nmore\n```\ndangling")
	require.Error(t, err)
	require.Contains(t, err.Error(), "line 6", "the error must point at the unmatched fence line")
}
