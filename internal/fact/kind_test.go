package fact

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestKind_Validate(t *testing.T) {
	require.NoError(t, Epistemic.Validate())
	require.NoError(t, Pragmatic.Validate())
	require.Error(t, Kind("").Validate())
	require.Error(t, Kind("bogus").Validate())
}

func TestKind_AllowsType_Epistemic(t *testing.T) {
	for _, ty := range AllEpistemicTypes() {
		require.True(t, Epistemic.AllowsType(ty), "epistemic must allow %q", ty)
	}
	require.False(t, Epistemic.AllowsType(Policy))
	require.False(t, Epistemic.AllowsType(Heuristic))
	require.False(t, Epistemic.AllowsType(""))
	require.False(t, Epistemic.AllowsType(Type("nope")))
}

func TestKind_AllowsType_Pragmatic(t *testing.T) {
	require.True(t, Pragmatic.AllowsType(Policy))
	require.True(t, Pragmatic.AllowsType(Heuristic))
	require.False(t, Pragmatic.AllowsType(Observation))
	require.False(t, Pragmatic.AllowsType(Hypothesis))
	require.False(t, Pragmatic.AllowsType(""))
}

func TestDefaultKind(t *testing.T) {
	require.Equal(t, Epistemic, DefaultKind)
}
