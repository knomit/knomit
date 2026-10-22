package fact

import (
	"context"
	"strings"
	"testing"
	"time"
)

// RunIsActive (F07 PR 5): `do: run` with a kebab-case recipe compiles as an
// ACTIVE trigger carrying its recipe name. Sabotage: leave run out of
// activeTriggerDo (red: unsupported).
func TestTriggers_RunIsActive(t *testing.T) {
	o := mustParseTriggerDoc(t, ontologyWithTriggers("  tasks:\n    description: t\n    triggers:\n      - {name: work, on: learn, do: run, recipe: claude-session}\n"))
	set := CompileTriggers(o, testIdentity, "b")
	if st := stateOf(t, set, "work"); st.State != TriggerActive || st.Recipe != "claude-session" {
		t.Fatalf("want active with its recipe, got %+v", st)
	}
	if ct := activeNamed(set, "work"); ct == nil || ct.Recipe != "claude-session" || ct.Do != TriggerDoRun {
		t.Fatalf("want an active run trigger with Recipe set, got %+v", ct)
	}
}

// RecipeNameValidated [H7] (T13, the ontology half): the recipe name becomes
// a path, so it must be kebab-case — `../x`, `a/b` and `X` are invalid (a
// problem, with a warning), `ok-name` is active. Sabotage: check only that
// the name is non-empty (red).
func TestTriggers_RecipeNameValidated(t *testing.T) {
	for _, bad := range []string{"../x", "a/b", "X", "x.js", ".."} {
		o := mustParseTriggerDoc(t, ontologyWithTriggers("  tasks:\n    description: t\n    triggers:\n      - {name: work, on: learn, do: run, recipe: \""+bad+"\"}\n"))
		st := stateOf(t, CompileTriggers(o, testIdentity, "b"), "work")
		if st.State != TriggerInvalid || !strings.Contains(st.Error, "kebab-case") {
			t.Errorf("recipe %q: want invalid (kebab-case), got %+v", bad, st)
		}
	}
	if ValidRecipeName("../x") || !ValidRecipeName("ok-name") {
		t.Errorf("ValidRecipeName disagrees with the rule")
	}
}

// RecipeHeader (D8): the first-line header sets concurrent, jitter and the
// timeout; absent fields take the defaults; an unknown field, a bad value or
// malformed JSON is an error; a header anywhere but the first line is
// ignored. Sabotage: accept unknown fields (red).
func TestRecipe_Header(t *testing.T) {
	lim, err := ParseRecipeHeader("// knomit: {\"concurrent\": 3, \"jitter_ms\": 250, \"timeout_ms\": 5000}\nx;")
	if err != nil || lim.Concurrent != 3 || lim.Jitter != 250*time.Millisecond || lim.Timeout != 5*time.Second {
		t.Fatalf("got %+v, %v", lim, err)
	}
	lim, err = ParseRecipeHeader("x;\n// knomit: {\"concurrent\": 9}\n")
	if err != nil || lim.Concurrent != RecipeDefaultConcurrent || lim.Timeout != RecipeDefaultTimeout || lim.Jitter != 0 {
		t.Fatalf("defaults: got %+v, %v", lim, err)
	}
	for _, bad := range []string{`{"bogus": 1}`, `{"concurrent": 0}`, `{"jitter_ms": -1}`, `{"timeout_ms": 0}`, `{concurrent: 1}`} {
		if _, err := ParseRecipeHeader("// knomit: " + bad + "\nx;"); err == nil {
			t.Errorf("header %s: want an error", bad)
		}
	}
}

// RunRecipe returns the completion value as the result: a known status with
// its message and id; anything else is an invalid result. Sabotage: ignore
// the completion value (red).
func TestRecipe_RunReturnsCompletionValue(t *testing.T) {
	run := func(src string) (RecipeResult, error) {
		prog, _, err := CompileRecipe("r", src)
		if err != nil {
			t.Fatalf("compile %q: %v", src, err)
		}
		return RunRecipe(context.Background(), prog, "r", map[string]any{"payload": map[string]any{"k": 1}}, ScriptHost{}, time.Now())
	}
	res, err := run(`({status: "delivered", message: "k=" + payload.k, id: 7});`)
	if err != nil || res != (RecipeResult{Status: "delivered", Message: "k=1", ID: "7"}) {
		t.Fatalf("got %+v, %v", res, err)
	}
	for _, src := range []string{`({status: "maybe"});`, `42;`, `undefined;`, `({});`} {
		if _, err := run(src); err == nil || !strings.Contains(err.Error(), "invalid result") {
			t.Errorf("%s: want invalid result, got %v", src, err)
		}
	}
}
