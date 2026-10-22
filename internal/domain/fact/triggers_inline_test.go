package fact

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// ---- F08 PR A, M4 (ruling R9): inline `js:` trigger scripts, parse side.

func jsOntology(entry string) string {
	return "id: t\nname: T\ntopics:\n  tasks:\n    description: tasks\n    triggers:\n" + entry
}

// stateOf compiles the ontology and returns the state of trigger name.
func jsStateOf(t *testing.T, yaml, name string) TriggerState {
	t.Helper()
	o, err := ParseOntology([]byte(yaml))
	require.NoError(t, err, "a trigger problem is never fatal to the ontology")
	set := CompileTriggers(o, validationIdentity, "blob")
	for _, st := range set.States {
		if st.Name == name {
			return st
		}
	}
	t.Fatalf("trigger %s not declared", name)
	return TriggerState{}
}

// T-A12 (parse half) InlineJSValidity: `js` compiles with the trigger and the
// trigger is active with its program; js with script, js on another `do`,
// do: script with neither, and code that does not compile are each INVALID
// with their own message — never fatal, and never deferred to a fire.
// Sabotage: accept both keys (red: active); compile lazily (red: a bad js is
// active).
func TestTriggers_InlineJSValidity(t *testing.T) {
	good := jsStateOf(t, jsOntology("      - name: exp\n        on: due\n        do: script\n        js: \"knomit.retract(change.path)\"\n"), "exp")
	require.Equal(t, TriggerActive, good.State, good.Error)
	require.Equal(t, "knomit.retract(change.path)", good.JS)

	o, err := ParseOntology([]byte(jsOntology("      - name: exp\n        on: due\n        do: script\n        js: \"knomit.retract(change.path)\"\n")))
	require.NoError(t, err)
	set := CompileTriggers(o, validationIdentity, "blob")
	require.Len(t, set.Active, 1)
	require.NotNil(t, set.Active[0].JSProgram(), "compiled with the trigger")
	require.Regexp(t, `^js:[0-9a-f]{16}$`, set.Active[0].JSKey)

	for _, tc := range []struct{ name, entry, want string }{
		{"both", "      - name: x\n        on: learn\n        do: script\n        script: s\n        js: \"1\"\n", "js and script are mutually exclusive"},
		{"emit", "      - name: x\n        on: learn\n        do: emit\n        js: \"1\"\n", "js is only for do: script"},
		{"neither", "      - name: x\n        on: learn\n        do: script\n", "do: script needs script (a file name) or js (inline code)"},
		{"syntax", "      - name: x\n        on: learn\n        do: script\n        js: \"if (\"\n", "js does not compile"},
		{"type", "      - name: x\n        on: learn\n        do: script\n        js: 5\n", "js must be a string"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := jsStateOf(t, jsOntology(tc.entry), "x")
			require.Equal(t, TriggerInvalid, st.State)
			require.Contains(t, st.Error, tc.want)
		})
	}
}

// InlineJSBlockScalarRoundTrips: a `js: |` block scalar decodes verbatim and
// Serialize writes the same block back.
func TestTriggers_InlineJSBlockScalarRoundTrips(t *testing.T) {
	yaml := jsOntology("      - name: exp\n        on: due\n        do: script\n        js: |\n          if (fact) {\n            knomit.retract(change.path);\n          }\n")
	o, err := ParseOntology([]byte(yaml))
	require.NoError(t, err)
	st := jsStateOf(t, yaml, "exp")
	require.Equal(t, TriggerActive, st.State, st.Error)
	require.Equal(t, "if (fact) {\n  knomit.retract(change.path);\n}\n", st.JS)
	out, err := o.Serialize()
	require.NoError(t, err)
	require.Contains(t, string(out), "js: |\n          if (fact) {\n            knomit.retract(change.path);\n          }\n")
}

// InlineJSCachedBySource: recompiling the SAME trigger source (a new
// ontology blob, as an edit elsewhere in the file makes) compiles nothing;
// a changed source compiles once. Sabotage: no cache → +1 on the recompile.
func TestTriggers_InlineJSCachedBySource(t *testing.T) {
	o1, err := ParseOntology([]byte(jsOntology("      - name: cached\n        on: learn\n        do: script\n        js: \"knomit.emit({v: 'cache-src-1'})\"\n")))
	require.NoError(t, err)
	CompileTriggers(o1, validationIdentity, "b1")
	n := InlineJSCompilesForTest()
	CompileTriggers(o1, validationIdentity, "b2")
	require.Equal(t, n, InlineJSCompilesForTest(), "same source, new blob: no compile")
	o2, err := ParseOntology([]byte(jsOntology("      - name: cached\n        on: learn\n        do: script\n        js: \"knomit.emit({v: 'cache-src-2'})\"\n")))
	require.NoError(t, err)
	CompileTriggers(o2, validationIdentity, "b3")
	require.Equal(t, n+1, InlineJSCompilesForTest(), "a changed source compiles once")
}
