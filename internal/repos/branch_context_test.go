package repos

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBranchContext_RoundTrip(t *testing.T) {
	ctx := WithBranch(context.Background(), "agent/laptop")
	b, ok := BranchFromContextOpt(ctx)
	require.True(t, ok)
	require.Equal(t, "agent/laptop", b)
}

func TestBranchContext_AbsentReturnsFalse(t *testing.T) {
	b, ok := BranchFromContextOpt(context.Background())
	require.False(t, ok)
	require.Equal(t, "", b)
}
