package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// #349: the agent's `trace` argument, handler half. Every refusal is driven
// through a real handler and asserts the branch head did not move — a trace
// validated after the write would still refuse the call, but too late.

var (
	traceCause40 = strings.Repeat("a", 40)
	traceCause64 = strings.Repeat("b", 64)
	traceRun     = "run-" + strings.Repeat("c", 32)
)

func traceRepo(t *testing.T) (*repos.RepoInstance, context.Context) {
	t.Helper()
	ri := newLearnTestRepo(t, fact.CodeOntology())
	return ri, repos.WithRepoInstance(context.Background(), ri)
}

// callHandler runs one tool handler and returns its text and isError.
func callHandler(t *testing.T, h toolHandler, ctx context.Context, args map[string]any) (string, bool) {
	t.Helper()
	var req mcpgo.CallToolRequest
	req.Params.Arguments = args
	res, err := h(ctx, req)
	require.NoError(t, err)
	return resultText(t, res), res.IsError
}

var traceFactSeq int

// learnWithTrace writes one fact; trace is omitted when nil.
func learnWithTrace(moment string, trace any) map[string]any {
	traceFactSeq++
	args := map[string]any{
		"moment_name": moment,
		"facts": []any{map[string]any{"topic": "architecture", "category": "trace/test",
			"title": fmt.Sprintf("Traced fact number %d about zebras", traceFactSeq), "body": "body",
			"entities": []any{fmt.Sprintf("ent-%d", traceFactSeq)}}},
	}
	if trace != nil {
		args["trace"] = trace
	}
	return args
}

// learnedCommit decodes a learn result's single commit.
func learnedCommit(t *testing.T, text string) (file, hash string) {
	t.Helper()
	var out struct {
		Commits []struct {
			File string `json:"file"`
			Hash string `json:"hash"`
		} `json:"commits"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &out), text)
	require.Len(t, out.Commits, 1, text)
	return out.Commits[0].File, out.Commits[0].Hash
}

// T4 + T5: every rule refuses the WHOLE call, names the entry, and writes
// nothing. Sabotage: check the Knomit- prefix case-sensitively (the lowercase
// rows go green); drop any one rule (its row goes green); validate the trace
// after the write (head moves).
func TestTrace_RefusedNamesTheEntryAndWritesNothing(t *testing.T) {
	ri, ctx := traceRepo(t)
	own := func(n int) map[string]any {
		m := map[string]any{}
		for i := 0; i < n; i++ {
			m[fmt.Sprintf("Key-%d", i)] = "v"
		}
		return m
	}
	cases := []struct {
		name  string
		trace any
		want  string
	}{
		// T4: reserved names, any case.
		{"Knomit-Trigger", map[string]any{"Knomit-Trigger": "wake"}, "Knomit-Trigger is reserved"},
		{"knomit-trigger lowercase", map[string]any{"knomit-trigger": "wake"}, "knomit-trigger is reserved"},
		{"KNOMIT-TRIGGER uppercase", map[string]any{"KNOMIT-TRIGGER": "wake"}, "KNOMIT-TRIGGER is reserved"},
		{"invented Knomit-Foo", map[string]any{"Knomit-Foo": "x"}, "Knomit-Foo is reserved"},
		{"invented knomit-foo lowercase", map[string]any{"knomit-foo": "x"}, "knomit-foo is reserved"},
		{"Knomit-Merge is knomit's too", map[string]any{"Knomit-Merge": "x"}, "Knomit-Merge is reserved"},
		{"knomit-trace lowercase, bad value", map[string]any{"knomit-trace": "two\nlines"}, "trace.knomit-trace: value must be one line"},
		// T5: own keys.
		{"key 33 chars", map[string]any{strings.Repeat("k", 33): "v"}, "longer than 32 characters"},
		{"key with a space", map[string]any{"Has Space": "v"}, "letters, digits and hyphens"},
		{"key with underscore", map[string]any{"under_score": "v"}, "letters, digits and hyphens"},
		{"key with a leading hyphen", map[string]any{"-lead": "v"}, "letters, digits and hyphens"},
		{"key with a colon", map[string]any{"Ti:cket": "v"}, "letters, digits and hyphens"},
		{"empty key", map[string]any{"": "v"}, "letters, digits and hyphens"},
		{"9 own entries", own(9), "9 entries of your own; at most 8"},
		{"case-duplicate own keys", map[string]any{"Ticket": "a", "ticket": "b"}, "are the same key"},
		{"case-duplicate Knomit-Trace", map[string]any{"Knomit-Trace": "a", "knomit-trace": "b"}, "are the same key"},
		// T5: values.
		{"value 129 bytes", map[string]any{"Ticket": strings.Repeat("v", 129)}, "trace.Ticket: value is longer than 128 bytes"},
		{"value 128 bytes of 2-byte runes plus one", map[string]any{"Ticket": strings.Repeat("é", 64) + "x"}, "longer than 128 bytes"},
		{"value LF", map[string]any{"Ticket": "a\nb"}, "one line: U+000A"},
		{"value CR", map[string]any{"Ticket": "a\rb"}, "one line: U+000D"},
		{"value TAB", map[string]any{"Ticket": "a\tb"}, "one line: U+0009"},
		{"value DEL", map[string]any{"Ticket": "a\x7fb"}, "one line: U+007F"},
		{"value NEL", map[string]any{"Ticket": "a\u0085b"}, "one line: U+0085"},
		{"value LINE SEPARATOR", map[string]any{"Ticket": "a b"}, "one line: U+2028"},
		{"value PARAGRAPH SEPARATOR", map[string]any{"Ticket": "a b"}, "one line: U+2029"},
		{"value leading space", map[string]any{"Ticket": " a"}, "leading or trailing whitespace"},
		{"value trailing space", map[string]any{"Ticket": "a "}, "leading or trailing whitespace"},
		{"value empty", map[string]any{"Ticket": ""}, "value is empty"},
		{"value not a string", map[string]any{"Ticket": 5}, "value must be a string"},
		{"trace not an object", "Knomit-Trace: x", "trace: must be an object"},
		// T5: the three Knomit- forms.
		{"Knomit-Trace 257 bytes", map[string]any{"Knomit-Trace": strings.Repeat("t", 257)}, "longer than 256 bytes"},
		{"Knomit-Trace padded", map[string]any{"Knomit-Trace": "t "}, "leading or trailing whitespace"},
		{"Knomit-Trace empty", map[string]any{"Knomit-Trace": ""}, "value is empty"},
		{"Knomit-Cause upper-case", map[string]any{"Knomit-Cause": strings.ToUpper(traceCause40)}, "lowercase hex"},
		{"Knomit-Cause 39 chars", map[string]any{"Knomit-Cause": traceCause40[:39]}, "lowercase hex"},
		{"Knomit-Cause 41 chars", map[string]any{"Knomit-Cause": traceCause40 + "a"}, "lowercase hex"},
		{"Knomit-Cause 63 chars", map[string]any{"Knomit-Cause": traceCause64[:63]}, "lowercase hex"},
		{"Knomit-Cause not hex", map[string]any{"Knomit-Cause": strings.Repeat("g", 40)}, "lowercase hex"},
		{"Knomit-Run 31 hex", map[string]any{"Knomit-Run": traceRun[:len(traceRun)-1]}, "run- followed by 32"},
		{"Knomit-Run 33 hex", map[string]any{"Knomit-Run": traceRun + "c"}, "run- followed by 32"},
		{"Knomit-Run upper-case", map[string]any{"Knomit-Run": "run-" + strings.Repeat("C", 32)}, "run- followed by 32"},
		{"Knomit-Run other prefix", map[string]any{"Knomit-Run": "job-" + strings.Repeat("c", 32)}, "run- followed by 32"},
		// One bad entry among good ones refuses the whole call.
		{"good entries plus one bad", map[string]any{"Knomit-Trace": "t", "Knomit-Run": traceRun, "Ticket": "ok", "Knomit-Trigger": "wake"}, "Knomit-Trigger is reserved"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			head := headOf(t, ri, "agent/test")
			text, isErr := callHandler(t, LearnHandler(), ctx, learnWithTrace("m", tc.trace))
			require.True(t, isErr, "must be refused, got: %s", text)
			require.Contains(t, text, tc.want)
			require.Equal(t, head, headOf(t, ri, "agent/test"), "a refused call writes nothing")
		})
	}

	// update and retract refuse before writing too.
	text, isErr := callHandler(t, LearnHandler(), ctx, learnWithTrace("m", nil))
	require.False(t, isErr, text)
	file, _ := learnedCommit(t, text)
	head := headOf(t, ri, "agent/test")
	text, isErr = callHandler(t, UpdateHandler(), ctx, map[string]any{"file": file, "moment_name": "m",
		"updates": map[string]any{"confidence": 0.9}, "trace": map[string]any{"knomit-trigger": "wake"}})
	require.True(t, isErr, text)
	require.Contains(t, text, "knomit-trigger is reserved")
	text, isErr = callHandler(t, RetractHandler(), ctx, map[string]any{"file": file, "moment_name": "m",
		"trace": map[string]any{"Ticket": strings.Repeat("v", 129)}})
	require.True(t, isErr, text)
	require.Contains(t, text, "longer than 128 bytes")
	require.Equal(t, head, headOf(t, ri, "agent/test"), "a refused update/retract writes nothing")
}

// T5, the accepted side of every bound: a 32-character key, a 128-byte value,
// exactly 8 own entries, a 256-byte Knomit-Trace, a 64-hex Knomit-Cause, a
// lowercase knomit- spelling (written canonically), git's own well-known
// trailers. Sabotage: tighten any bound by one (key < 32, value < 128,
// entries < 8) → this goes red.
func TestTrace_BoundsAccepted(t *testing.T) {
	ri, ctx := traceRepo(t)
	key32 := "K" + strings.Repeat("k", 31)
	val128 := strings.Repeat("é", 64) // 128 bytes
	trace := map[string]any{
		"knomit-trace":  strings.Repeat("t", 256),
		"KNOMIT-CAUSE":  traceCause64,
		"Knomit-Run":    traceRun,
		key32:           val128,
		"Signed-off-by": "A Person <a@example.com>",
		"Ticket":        "ABC-12: with a colon and spaces",
	}
	for i := 0; i < 5; i++ {
		trace[fmt.Sprintf("Key-%d", i)] = "v"
	}
	require.Len(t, trace, 3+8, "fixture: exactly 8 own entries")

	text, isErr := callHandler(t, LearnHandler(), ctx, learnWithTrace("bounds", trace))
	require.False(t, isErr, text)
	_, hash := learnedCommit(t, text)
	want := "learn: bounds\n\nKnomit-Trace: " + strings.Repeat("t", 256) + "\nKnomit-Cause: " + traceCause64 +
		"\nKnomit-Run: " + traceRun +
		"\nKey-0: v\nKey-1: v\nKey-2: v\nKey-3: v\nKey-4: v\n" + key32 + ": " + val128 +
		"\nSigned-off-by: A Person <a@example.com>\nTicket: ABC-12: with a colon and spaces\n"
	require.Equal(t, want, messageOf(t, ri, hash))
	require.Equal(t, traceRun, store.TrailerValue(messageOf(t, ri, hash), store.TrailerRun))

	// An empty object is a trace with no entries: nothing is stamped.
	text, isErr = callHandler(t, LearnHandler(), ctx, learnWithTrace("empty", map[string]any{}))
	require.False(t, isErr, text)
	_, hash = learnedCommit(t, text)
	require.Equal(t, "learn: empty", messageOf(t, ri, hash))
}

// T7: the moment_name back door (probe P2) is closed on learn, update and
// retract — a control character or a Unicode line separator is refused and
// nothing is written; a one-line name still works. Sabotage: remove the
// control-character check (the forged paragraph lands and TrailerValue reads
// Knomit-Trigger: wake).
func TestTrace_MomentNameMustBeOneLine(t *testing.T) {
	ri, ctx := traceRepo(t)
	text, isErr := callHandler(t, LearnHandler(), ctx, learnWithTrace("one line: fine — ünïcode ok", nil))
	require.False(t, isErr, text)
	file, _ := learnedCommit(t, text)

	head := headOf(t, ri, "agent/test")
	for _, moment := range []string{
		"x\n\nKnomit-Trigger: wake\nKnomit-Trace: forged", // P2
		"x\r\rKnomit-Trace: f", "a\tb", "a\u0085b", "a b", "a b",
	} {
		text, isErr := callHandler(t, LearnHandler(), ctx, learnWithTrace(moment, nil))
		require.True(t, isErr, "learn %q: %s", moment, text)
		require.Contains(t, text, "moment_name must be one line")
		text, isErr = callHandler(t, UpdateHandler(), ctx, map[string]any{"file": file, "moment_name": moment,
			"updates": map[string]any{"confidence": 0.9}})
		require.True(t, isErr, "update %q: %s", moment, text)
		require.Contains(t, text, "moment_name must be one line")
		text, isErr = callHandler(t, RetractHandler(), ctx, map[string]any{"file": file, "moment_name": moment})
		require.True(t, isErr, "retract %q: %s", moment, text)
		require.Contains(t, text, "moment_name must be one line")
	}
	require.Equal(t, head, headOf(t, ri, "agent/test"), "nothing was written")
}

// The five write tools and knomit_experiment declare `trace` in their served schema, as an object
// of string values. review and hypothesize refuse undeclared arguments, so
// without this a trace there is "unknown argument". Sabotage: drop traceArg()
// from one tool.
func TestTrace_DeclaredOnTheFiveWriteTools(t *testing.T) {
	for _, tool := range []mcpgo.Tool{learnTool(), updateTool(), retractTool(), reviewTool(), hypothesizeTool(), experimentTool()} {
		prop, ok := tool.InputSchema.Properties["trace"].(map[string]any)
		require.True(t, ok, "%s has no trace argument", tool.Name)
		require.Equal(t, "object", prop["type"], tool.Name)
		require.Equal(t, map[string]any{"type": "string"}, prop["additionalProperties"], tool.Name)
		require.NotContains(t, tool.InputSchema.Required, "trace", tool.Name)
	}
}

// T8, hypothesize half: a knomit_hypothesize answer that writes (a discover
// proposal) carries the call's trace on its commit; the same answer without a
// trace carries none. Sabotage: drop applyTrace from HypothesizeHandler, or
// apply it to a ctx the engine does not receive.
func TestTrace_HypothesizeAnswerStampsItsWrite(t *testing.T) {
	ctx, svc := newHypothesizeHandlerCtx(t)
	for _, p := range []string{"kb/seedone.md", "kb/seedtwo.md"} {
		f := fact.NewFact(p)
		f.Title, f.Body, f.Type = "Seed "+p, "body of "+p, fact.Observation
		f.Confidence, f.Sources, f.Domain = 0.8, 1, []string{"auth"}
		body, err := fact.SerializeFact(f)
		require.NoError(t, err)
		_, err = svc.Facts().WriteFact(context.Background(), "agent/test", p, body, "seed", "")
		require.NoError(t, err)
	}
	// One planned session holding one forward-discover item over the two
	// seeds (the shape the engine plans at high effort).
	session := func() string {
		sess, err := svc.Pipeline().CreatePipelineSession(context.Background(), "hypothesize", "agent/test", "")
		require.NoError(t, err)
		_, err = svc.Pipeline().MarkPipelineSessionPlanned(context.Background(), sess.ID)
		require.NoError(t, err)
		require.NoError(t, svc.Pipeline().InsertPipelineWorkItem(context.Background(), store.PipelineWorkItem{
			SessionID: sess.ID, StepType: "discover", ClusterKey: "discover-bwd-0", Priority: -100,
			FactsJSON: `{"direction":"forward","bridge":{"token":"auth","kind":"entity","members":[{"path":"kb/seedone.md"},{"path":"kb/seedtwo.md"}]}}`,
		}))
		return sess.ID
	}
	proposal := func(title string) string {
		return `{"proposals":[{"path":"kb/x/p.md","title":"` + title + `","body":"B","type":"synthesis","domain":["auth"],"confidence":0.9,"entities":[],"refs":["kb/seedone.md","kb/seedtwo.md"]}]}`
	}
	headMsg := func() (string, string) {
		h, err := svc.Branches().HeadCommit(context.Background(), "agent/test")
		require.NoError(t, err)
		info, err := svc.Triggers().CommitInfo(context.Background(), plumbing.NewHash(h))
		require.NoError(t, err)
		return h, info.Message
	}

	before, _ := headMsg()
	text, isErr := callHandler(t, HypothesizeHandler(), ctx, map[string]any{"session_id": session(),
		"response": proposal("Traced proposal"), "trace": map[string]any{"Knomit-Trace": "task-7", "Knomit-Run": traceRun}})
	require.False(t, isErr, text)
	h, msg := headMsg()
	require.NotEqual(t, before, h, "fixture: the answer must write a commit, or this test proves nothing")
	require.True(t, strings.HasSuffix(msg, "\n\nKnomit-Trace: task-7\nKnomit-Run: "+traceRun+"\n"), "%q", msg)

	text, isErr = callHandler(t, HypothesizeHandler(), ctx, map[string]any{"session_id": session(),
		"response": proposal("Untraced proposal")})
	require.False(t, isErr, text)
	h2, msg := headMsg()
	require.NotEqual(t, h, h2, "fixture: the second answer writes too")
	require.NotContains(t, msg, "\n\n", "no trace passed: no paragraph of any kind (D-mint): %q", msg)
}
