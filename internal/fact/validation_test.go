package fact

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCompileRules_ValidJS(t *testing.T) {
	rules := []Validation{
		{Name: "ok", Message: "ok", Rule: "fact.entities.includes('designer')"},
	}
	cs, err := compileRules("principles", rules)
	require.NoError(t, err)
	require.Len(t, cs, 1)
	require.Equal(t, "ok", cs[0].Name)
}

func TestCompileRules_BadJS(t *testing.T) {
	rules := []Validation{
		{Name: "bad", Message: "x", Rule: "this is not js {{"},
	}
	_, err := compileRules("principles", rules)
	require.Error(t, err)
	require.Contains(t, err.Error(), "bad")
}
