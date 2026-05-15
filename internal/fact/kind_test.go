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

func TestValidateKindAndType_DefaultsMissingKind(t *testing.T) {
	k, err := validateKindAndType("", Observation)
	require.NoError(t, err)
	require.Equal(t, Epistemic, k)
}

func TestValidateKindAndType_AcceptsValidPairs(t *testing.T) {
	cases := []struct {
		kind Kind
		typ  Type
	}{
		{Epistemic, Observation},
		{Epistemic, Hypothesis},
		{Pragmatic, Policy},
		{Pragmatic, Heuristic},
	}
	for _, c := range cases {
		t.Run(string(c.kind)+"/"+string(c.typ), func(t *testing.T) {
			k, err := validateKindAndType(c.kind, c.typ)
			require.NoError(t, err)
			require.Equal(t, c.kind, k)
		})
	}
}

func TestValidateKindAndType_RejectsUnknownKind(t *testing.T) {
	_, err := validateKindAndType(Kind("bogus"), Observation)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid kind")
}

func TestValidateKindAndType_RejectsCrossKindMismatch(t *testing.T) {
	_, err := validateKindAndType(Pragmatic, Observation)
	require.Error(t, err)
	require.Contains(t, err.Error(), "type")
	require.Contains(t, err.Error(), "pragmatic")

	_, err = validateKindAndType(Epistemic, Policy)
	require.Error(t, err)
	require.Contains(t, err.Error(), "type")
	require.Contains(t, err.Error(), "epistemic")
}

func TestValidateKindAndType_RejectsEmptyType(t *testing.T) {
	_, err := validateKindAndType(Epistemic, "")
	require.Error(t, err)
}
