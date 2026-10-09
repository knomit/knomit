package repos

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// ---- F08 PR A, M4 (ruling R9): inline `js:` trigger scripts, dispatcher
// side. The code runs exactly like a file script: same sandbox, host API,
// trailers, loop guard and rate cap.

// jsTrig renders a `do: script` entry with inline code (a YAML
// double-quoted scalar).
func jsTrig(name, on, match, js string) string {
	return fmt.Sprintf("      - name: %s\n        on: %s\n        do: script\n        match: %q\n        js: %q\n", name, on, match, js)
}

// T-A12 (dispatcher half) InlineJS: an inline trigger runs (`ran`), and the
// commit its knomit.learn makes carries Knomit-Trigger; a trigger whose `js`
// does not compile is `invalid` on the endpoint with the compile error and
// never fires — no per-fire script-error. Sabotage: compile lazily per fire →
// the bad one is active and logs script-error rows → red.
func TestTriggers_InlineJS(t *testing.T) {
	t.Parallel()
	_, ri, tools := newScriptRepo(t, 0,
		jsTrig("inline", "learn", "tasks/in/**", `knomit.learn({topic: "tasks", category: "out", title: "Inline"});`),
		jsTrig("broken", "learn", "tasks/in/**", `if (`))
	settle(t, ri)
	base := settle(t, ri)
	writeOn(t, ri, trigAgent, "kb/tasks/in/a.md")
	settle(t, ri)

	require.Equal(t, []string{store.TriggerOutcomeRan}, outcomesOf(firesOf(t, ri, "inline")))
	require.Len(t, tools.recorded(), 1)
	commits := commitsAfter(t, ri, base)
	require.Len(t, commits, 2, "the write and the inline script's learn")
	require.Equal(t, "inline", trailersOf(t, ri, commits[1]).Trigger)

	require.Empty(t, firesOf(t, ri, "broken"), "an uncompilable js never fires")
	rep, err := ri.TriggerReport(context.Background(), 0)
	require.NoError(t, err)
	byName := map[string]TriggerView{}
	for _, v := range rep.Triggers {
		byName[v.Name] = v
	}
	require.Equal(t, fact.TriggerActive, byName["inline"].State)
	require.Contains(t, byName["inline"].JS, "knomit.learn(")
	require.Empty(t, byName["inline"].Script)
	require.Contains(t, []string{fact.TriggerInvalid, "frozen"}, byName["broken"].State)
	require.Contains(t, byName["broken"].Error, "js does not compile")
}

// T-A13 InlineJSCacheBySource: an ontology edit ELSEWHERE (a new blob) keeps
// the inline program — no compile, and the same code runs; editing the `js`
// compiles once and the EDITED code is what runs on the next fire. The effect
// is asserted, not only a counter, so a cache keyed by trigger name (which
// would keep running the old program) is red too. Sabotage: key by ontology
// blob → the unrelated edit compiles → red; key by trigger name → v1 runs
// after the edit → red.
func TestTriggers_InlineJSCacheBySource(t *testing.T) {
	v := func(n int) string {
		return jsTrig("ver", "learn", "tasks/in/**", fmt.Sprintf(`knomit.emit({v: %d, marker: "cache-by-source"});`, n))
	}
	_, ri, _ := newScriptRepo(t, 0, v(1))
	sink := subscribePayloads(t, ri, "ver")
	settle(t, ri)
	write(t, ri, "kb/tasks/in/a.md")
	settle(t, ri)
	require.Equal(t, float64(1), sink.wait(t, 1)[0]["v"])

	compiles := fact.InlineJSCompilesForTest()
	setOntology(t, ri, triggerOntology("description: edited elsewhere\n", v(1)))
	write(t, ri, "kb/tasks/in/b.md")
	settle(t, ri)
	ps := sink.wait(t, 2)
	require.Equal(t, float64(1), ps[1]["v"], "the same code still runs")
	require.Equal(t, compiles, fact.InlineJSCompilesForTest(), "an edit elsewhere compiles nothing")

	setOntology(t, ri, triggerOntology("description: edited elsewhere\n", v(2)))
	write(t, ri, "kb/tasks/in/c.md")
	settle(t, ri)
	ps = sink.wait(t, 3)
	require.Equal(t, float64(2), ps[2]["v"], "the EDITED code runs on the next fire")
	require.Equal(t, compiles+1, fact.InlineJSCompilesForTest(), "a changed js compiles once")
	require.Equal(t, []string{store.TriggerOutcomeRan, store.TriggerOutcomeRan, store.TriggerOutcomeRan}, outcomesOf(firesOf(t, ri, "ver")))
}
