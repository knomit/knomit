package repos

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// F22: a trigger condition sees fact.context (the same map the validation
// rules see, through FactGlobal). `if: fact.context.verdict === 'disagree'`
// emits for the matching learn only (an if-false records no fire row), and a
// fact with no context reads fact.context as {} rather than throwing — which
// would record an if-error row.
//
// SABOTAGE: drop "context" from factToJS → every condition throws → three
// if-error rows, no emitted one → red.
func TestDispatch_IfSeesFactContext(t *testing.T) {
	t.Parallel()
	_, ri := newTriggerRepo(t, trig("disagree", "learn", "", "fact.context.verdict === 'disagree'"))
	writeCtx := func(path, ctxLine string) {
		r, err := testService(t, ri).Facts().WriteFact(context.Background(), trigAgent, path,
			"---\ntype: observation\nconfidence: 0.8\nsources: 1\n"+ctxLine+"---\n# "+path+"\n\nbody\n", "learn: "+path, "learn")
		require.NoError(t, err)
		waitTriggerHead(t, ri, r.CommitHash)
	}
	writeCtx("kb/tasks/a.md", "context: {task: t-17, verdict: disagree}\n")
	writeCtx("kb/tasks/b.md", "context: {task: t-17, verdict: agree}\n")
	writeCtx("kb/tasks/c.md", "")

	got := firesOf(t, ri, "disagree")
	require.Len(t, got, 1, "one fire for three learns: %+v", got)
	require.Equal(t, "kb/tasks/a.md", got[0].Path)
	require.Equal(t, store.TriggerOutcomeEmitted, got[0].Outcome, "no if-error: a fact without context reads {}")
}
