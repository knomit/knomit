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
	for _, ty := range AllPragmaticTypes() {
		require.False(t, Epistemic.AllowsType(ty), "epistemic must reject pragmatic %q", ty)
	}
	require.False(t, Epistemic.AllowsType(""))
	require.False(t, Epistemic.AllowsType(Type("nope")))
}

func TestKind_AllowsType_Pragmatic(t *testing.T) {
	// Derived from AllPragmaticTypes rather than hard-coded: this test read
	// as "pragmatic allows its types" while actually naming two of them, so
	// adding a third left it passing and silent about the new one.
	for _, ty := range AllPragmaticTypes() {
		require.True(t, Pragmatic.AllowsType(ty), "pragmatic must allow %q", ty)
	}
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
		{Pragmatic, Signal},
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
	require.Contains(t, err.Error(), "invalid type")
}

func TestValidateKindAndType_DefaultingHappensBeforeTypeCheck(t *testing.T) {
	// Empty kind normalizes to Epistemic, then Policy fails the
	// epistemic-allows-type check (not the kind check).
	_, err := validateKindAndType("", Policy)
	require.Error(t, err)
	require.Contains(t, err.Error(), "epistemic")
}

// TestKindAllowsType_Signal pins both directions of the new leaf type in one
// place, because the two halves fail for different reasons and only together
// say "signal is pragmatic". A signal allowed under epistemic would defeat the
// point of the type: kind is what keeps it out of synthesis
// (synthesize.reviewStrategy.AcceptSeed tests Kind, not Type), so an epistemic
// signal would seed the review pipeline and be rewritten on commit.
func TestKindAllowsType_Signal(t *testing.T) {
	require.True(t, Pragmatic.AllowsType(Signal), "signal is a pragmatic leaf type")
	require.False(t, Epistemic.AllowsType(Signal), "signal must never be reachable under epistemic")

	require.True(t, PragmaticTypes[Signal], "the authoritative set must carry signal")
	require.Contains(t, AllPragmaticTypes(), Signal, "the ordered slice feeds the tool schema enum")

	// Validation agrees with the switch, through the single entry point both
	// ParseFact and SerializeFact use.
	k, err := validateKindAndType(Pragmatic, Signal)
	require.NoError(t, err)
	require.Equal(t, Pragmatic, k)

	_, err = validateKindAndType(Epistemic, Signal)
	require.Error(t, err)
	require.Contains(t, err.Error(), "epistemic")
}
