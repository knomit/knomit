package mcp

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// profileFor is the per-session profile resolution used by AfterInitialize:
// nil manager, nil settings, empty repo ID, and absent rows all read "code".
func TestProfileFor_DefaultsToCode(t *testing.T) {
	require.Equal(t, "code", profileFor(nil, nil))
}
