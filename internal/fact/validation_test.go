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

func TestValidateFact_RulesAtTopicFire(t *testing.T) {
	const y = `
id: t
name: T
topics:
  principles:
    description: x
    validations:
      - name: must-have-designer
        message: missing designer
        rule: "fact.entities.includes('designer')"
`
	o, err := ParseOntology([]byte(y))
	require.NoError(t, err)

	good := Fact{Entities: []string{"designer"}}
	require.NoError(t, ValidateFact(o, "principles/mission", good))

	bad := Fact{Entities: []string{"agent"}}
	err = ValidateFact(o, "principles/mission", bad)
	require.Error(t, err)
	var vErr *ValidationError
	require.ErrorAs(t, err, &vErr)
	require.Equal(t, "must-have-designer", vErr.RuleName)
	require.Equal(t, "missing designer", vErr.Message)
}

func TestValidateFact_NoRulesForTopic(t *testing.T) {
	const y = `
id: t
name: T
topics:
  invariants:
    description: x
`
	o, err := ParseOntology([]byte(y))
	require.NoError(t, err)
	require.NoError(t, ValidateFact(o, "invariants/store", Fact{}))
}

func TestValidateFact_RootRulesFire(t *testing.T) {
	const y = `
id: t
name: T
validations:
  - name: kind-set
    message: "kind is required"
    rule: "fact.kind !== ''"
topics:
  invariants:
    description: x
`
	o, err := ParseOntology([]byte(y))
	require.NoError(t, err)

	err = ValidateFact(o, "invariants/store", Fact{Kind: ""})
	require.Error(t, err)

	require.NoError(t, ValidateFact(o, "invariants/store", Fact{Kind: Epistemic}))
}

func TestValidateFact_RulesCompiledOncePerOntology(t *testing.T) {
	const y = `
id: t
name: T
topics:
  principles:
    description: x
    validations:
      - name: r
        message: m
        rule: "fact.entities.length > 0"
`
	o, err := ParseOntology([]byte(y))
	require.NoError(t, err)

	cache := o.rulesCache()
	for i := 0; i < 5; i++ {
		err := ValidateFact(o, "principles/m", Fact{Entities: []string{"designer"}})
		require.NoError(t, err)
	}
	require.Equal(t, 1, cache.compileCalls)
}
