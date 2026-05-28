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

func TestEvaluateRule_PassFail(t *testing.T) {
	rules, err := compileRules("p", []Validation{
		{Name: "designer", Message: "missing designer", Rule: "fact.entities.includes('designer')"},
	})
	require.NoError(t, err)

	pass := Fact{Entities: []string{"designer"}}
	ok, err := evaluateRule(rules[0], pass)
	require.NoError(t, err)
	require.True(t, ok)

	fail := Fact{Entities: []string{"agent"}}
	ok, err = evaluateRule(rules[0], fail)
	require.NoError(t, err)
	require.False(t, ok)
}

func TestEvaluateRule_SandboxNoGlobals(t *testing.T) {
	// process, require, globalThis, etc. should be undefined inside the rule.
	rules, err := compileRules("p", []Validation{
		{Name: "no-process", Message: "x", Rule: "typeof process === 'undefined'"},
		{Name: "no-globalThis-keys", Message: "x", Rule: "typeof globalThis === 'undefined' || Object.keys(globalThis).length === 0"},
	})
	require.NoError(t, err)

	f := Fact{Entities: []string{"designer"}}
	for _, r := range rules {
		ok, err := evaluateRule(r, f)
		require.NoError(t, err, r.Name)
		require.True(t, ok, "sandbox check failed: %s", r.Name)
	}
}
