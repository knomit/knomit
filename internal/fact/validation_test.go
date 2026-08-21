package fact

import (
	"reflect"
	"strings"
	"testing"
	"time"

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

// TestEvaluateRule_TimesOutOnRunaway guards the goja execution-timeout: a
// rule that never returns (infinite loop in a hand-edited ontology) must be
// interrupted and surfaced as an error rather than hanging the caller.
func TestEvaluateRule_TimesOutOnRunaway(t *testing.T) {
	rules, err := compileRules("p", []Validation{
		{Name: "runaway", Message: "x", Rule: "while (true) {}"},
	})
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		_, e := evaluateRule(rules[0], Fact{Entities: []string{"designer"}})
		done <- e
	}()
	select {
	case e := <-done:
		require.Error(t, e, "runaway rule must be interrupted, not pass")
	case <-time.After(5 * time.Second):
		t.Fatal("evaluateRule did not return — interrupt timer is not working")
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

// TestValidateFact_MixedCaseTopicStillFiresRules guards against a regression
// where ValidateFact lowercased topic segments for the ontology NODE lookup
// but not for the rules-CACHE lookup. A mixed-case topic path (e.g.
// "Principles/Mission") would resolve the node yet miss the lowercase-keyed
// cache, silently skipping every validation rule. Rules must fire regardless
// of the casing of the supplied topic path.
func TestValidateFact_MixedCaseTopicStillFiresRules(t *testing.T) {
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

	bad := Fact{Entities: []string{"agent"}}
	for _, topicPath := range []string{"Principles/mission", "PRINCIPLES/MISSION", "principles/Mission"} {
		err := ValidateFact(o, topicPath, bad)
		require.Error(t, err, "rule should fire for mixed-case topic %q", topicPath)
		var vErr *ValidationError
		require.ErrorAs(t, err, &vErr)
		require.Equal(t, "must-have-designer", vErr.RuleName)
	}
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

	for i := 0; i < 5; i++ {
		err := ValidateFact(o, "principles/m", Fact{Entities: []string{"designer"}})
		require.NoError(t, err)
	}
	require.Equal(t, 1, o.cache.compileCalls)
}

func TestParseOntology_BadRuleJSFailsLoudly(t *testing.T) {
	const y = `
id: t
name: T
topics:
  principles:
    description: x
    validations:
      - name: bad
        message: m
        rule: "this is not js {{"
`
	_, err := ParseOntology([]byte(y))
	require.Error(t, err)
	require.Contains(t, err.Error(), "bad")
}

// factToJSOmitted names the exported Fact fields deliberately withheld from
// the rule sandbox, each with the reason it is withheld. It is the opt-out
// list for TestFactToJS_ExposesEveryFactField: adding a field to Fact without
// exposing it to rules is a red test unless the omission is recorded here.
//
// The default is EXPOSURE. A field belongs here only when a rule written over
// it could not be trusted — not merely because no rule needs it today.
var factToJSOmitted = map[string]string{
	"RefWarnings": "derived on read, never stored, and unusable from a rule in both directions: " +
		"structurally always empty on the knomit_learn path (the fact is built in memory and " +
		"SerializeFact refuses to write a malformed ref anyway), and stale on the knomit_update " +
		"path (ParseFact computes it from the on-disk refs, which the handler replaces wholesale " +
		"before ValidateFact runs) — so a rule would judge refs that are not being written",
	"MotifWarnings": "derived on read, never stored, and unusable from a rule for the same reason " +
		"RefWarnings is: structurally always empty on the knomit_learn path (the fact is built in " +
		"memory and SerializeFact refuses to write a motif that would earn a warning), and stale on " +
		"the knomit_update path (ParseFact computes it from the on-disk motifs, which the handler may " +
		"replace wholesale before ValidateFact runs) — so a rule would judge motifs that are not " +
		"being written. The STRIPPED motif list is exposed instead; see resolvedMotifs.",
}

// TestFactToJS_ExposesEveryFactField is the regression guard for the omission
// this list exists to prevent: sources, origin and evidence_weight were added
// to Fact and factToJS was never updated, so rules could not see them and
// nothing failed. Reflection over the struct turns the next such addition into
// a red test instead of a silent gap.
func TestFactToJS_ExposesEveryFactField(t *testing.T) {
	js := factToJS(Fact{})
	typ := reflect.TypeFor[Fact]()

	for i := range typ.NumField() {
		f := typ.Field(i)
		if !f.IsExported() {
			continue
		}
		key, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		require.NotEmpty(t, key, "Fact.%s has no json tag; the rule-sandbox key cannot be derived", f.Name)

		if reason, omitted := factToJSOmitted[f.Name]; omitted {
			require.NotEmpty(t, reason, "Fact.%s is omitted from factToJS without a recorded reason", f.Name)
			require.NotContains(t, js, key,
				"Fact.%s is listed in factToJSOmitted but factToJS exposes %q — remove one or the other", f.Name, key)
			continue
		}
		require.Contains(t, js, key,
			"Fact.%s (json %q) is invisible to ontology rules. Expose it in factToJS, "+
				"or record why it must stay hidden in factToJSOmitted.", f.Name, key)
	}

	// path is unexported on Fact (read via f.Path()), so the loop above cannot
	// reach it. Assert it separately rather than leave the sandbox's most-used
	// key untested.
	require.Contains(t, js, "path")
}

func TestEvaluateRule_SeesSources(t *testing.T) {
	rules, err := compileRules("p", []Validation{
		{Name: "corroborated", Message: "needs two sources", Rule: "fact.sources >= 2"},
	})
	require.NoError(t, err)

	ok, err := evaluateRule(rules[0], Fact{Sources: 2})
	require.NoError(t, err)
	require.True(t, ok)

	ok, err = evaluateRule(rules[0], Fact{Sources: 1})
	require.NoError(t, err)
	require.False(t, ok)
}

func TestEvaluateRule_SeesEvidenceWeight(t *testing.T) {
	rules, err := compileRules("p", []Validation{
		{Name: "weighted", Message: "needs weight", Rule: "fact.evidence_weight > 0.5"},
	})
	require.NoError(t, err)

	ok, err := evaluateRule(rules[0], Fact{EvidenceWeight: 0.9})
	require.NoError(t, err)
	require.True(t, ok)

	ok, err = evaluateRule(rules[0], Fact{EvidenceWeight: 0.1})
	require.NoError(t, err)
	require.False(t, ok)
}

// TestEvaluateRule_SeesResolvedOrigin pins the decision that rules see the
// RESOLVED origin, never the raw field. knomit_learn deliberately leaves
// Fact.Origin empty when the caller omitted it, so SerializeFact/ParseFact can
// apply the default — but that happens AFTER ValidateFact runs. knomit_update,
// by contrast, hands ValidateFact a fact ParseFact already resolved. Exposing
// the raw field would therefore make one rule disagree between the two write
// paths: `fact.origin === 'authored'` would reject nearly every learn write
// while passing the equivalent update.
func TestEvaluateRule_SeesResolvedOrigin(t *testing.T) {
	rules, err := compileRules("p", []Validation{
		{Name: "authored", Message: "x", Rule: "fact.origin === 'authored'"},
		{Name: "distilled", Message: "x", Rule: "fact.origin === 'distilled'"},
		{Name: "discovered", Message: "x", Rule: "fact.origin === 'discovered'"},
	})
	require.NoError(t, err)
	byName := map[string]compiledRule{}
	for _, r := range rules {
		byName[r.Name] = r
	}

	cases := []struct {
		name  string
		fact  Fact
		rule  string
		match bool
	}{
		// Unset origin resolves through defaultOriginForType, exactly as the
		// on-disk round trip will resolve it.
		{"unset observation is authored", Fact{Type: Observation}, "authored", true},
		{"unset synthesis is distilled", Fact{Type: Synthesis}, "distilled", true},
		{"unset synthesis is not authored", Fact{Type: Synthesis}, "authored", false},
		// An explicit origin passes through untouched.
		{"explicit discovered survives", Fact{Type: Synthesis, Origin: Discovered}, "discovered", true},
		{"explicit authored survives", Fact{Type: Observation, Origin: Authored}, "authored", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, err := evaluateRule(byName[tc.rule], tc.fact)
			require.NoError(t, err)
			require.Equal(t, tc.match, ok)
		})
	}
}

// TestEvaluateRule_ResolvedOriginAgreesAcrossWritePaths states the property the
// resolution exists to preserve, rather than the mechanism: the same fact must
// judge identically whether it reaches ValidateFact straight from knomit_learn
// (origin unset) or via ParseFact on the knomit_update path (origin resolved).
// The rules must DISCRIMINATE the defaults: a rule like `fact.origin !==
// 'discovered'` is true both for an unresolved (undefined) origin and for every
// resolved one, so it agrees across the paths whether or not resolution happens
// and would pass with the fix removed. Asserting the whole match vector — and
// that exactly one origin matches — is what fails when the raw field leaks
// through (undefined matches none) or when the wrong default is substituted.
func TestEvaluateRule_ResolvedOriginAgreesAcrossWritePaths(t *testing.T) {
	origins := []Origin{Authored, Distilled, Discovered}
	rules, err := compileRules("p", []Validation{
		{Name: "authored", Message: "x", Rule: "fact.origin === 'authored'"},
		{Name: "distilled", Message: "x", Rule: "fact.origin === 'distilled'"},
		{Name: "discovered", Message: "x", Rule: "fact.origin === 'discovered'"},
	})
	require.NoError(t, err)

	// What ParseFact resolves an elided origin to, stated literally rather than
	// through defaultOriginForType — otherwise a wrong default would make both
	// sides agree on the same wrong answer.
	parsedOrigin := map[Type]Origin{
		Observation: Authored,
		Synthesis:   Distilled,
		Hypothesis:  Authored,
	}

	for typ, want := range parsedOrigin {
		asLearnBuilt := Fact{Type: typ}           // origin left unset
		asParsed := Fact{Type: typ, Origin: want} // origin resolved by ParseFact

		for i, o := range origins {
			learnOK, err := evaluateRule(rules[i], asLearnBuilt)
			require.NoError(t, err)
			parsedOK, err := evaluateRule(rules[i], asParsed)
			require.NoError(t, err)

			require.Equal(t, parsedOK, learnOK,
				"type %q judges origin %q differently on the learn and update paths — origin is not being resolved", typ, o)
			require.Equal(t, o == want, learnOK,
				"type %q with origin unset should match only %q, but rule %q returned %v", typ, want, o, learnOK)
		}
	}
}
