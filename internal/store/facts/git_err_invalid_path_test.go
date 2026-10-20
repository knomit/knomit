package facts

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The sentinel is recovered from go-git's internal package by unwrapping a
// probe. If an upgrade stopped wrapping it, gitReadablePath would accept
// everything; this pins it.
func TestGitErrInvalidPath_Recovered(t *testing.T) {
	require.NotNil(t, gitErrInvalidPath)
	require.Equal(t, "invalid path", gitErrInvalidPath.Error())
}
