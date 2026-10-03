package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// contextOntologyYAML declares context on `verdicts` (required task+verdict)
// and a looser block on `notes`; `plain` declares none. learn_dedup is off on
// verdicts so near-identical verdicts land as separate facts.
const contextOntologyYAML = `id: t
name: T
topics:
  verdicts:
    description: verdicts
    attributes:
      learn_dedup: off
    context:
      task:    {type: string, required: true, pattern: "^t-[0-9]+$"}
      verdict: {type: enum, values: [agree, disagree, unsure], required: true}
      score:   {type: number, min: 0, max: 1}
      at:      {type: time}
  notes:
    description: notes
    context:
      task: {type: string}
      note: {type: string}
  decisions:
    description: x
  plain:
    description: no context
`

// newContextRepo is newDedupAttrRepo with the context ontology (or none when
// ont is nil and noOntology is set).
func newContextRepo(t *testing.T, ontYAML string) (*repos.RepoInstance, *store.Service, context.Context, store.BatchEmbedder) {
	t.Helper()
	svc, err := store.Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	emb := newLenEmbedder(t)
	svc.SetEmbedder(emb)
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))
	var o *fact.Ontology
	if ontYAML != "" {
		o, err = fact.ParseNewOntology([]byte(ontYAML))
		require.NoError(t, err)
	}
	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "test", UID: nextTestRepoUID(), AgentBranch: "agent/test",
		Svc: svc, Ontology: o, OntologyRoot: "kb", Embedder: emb,
	})
	return ri, svc, repos.WithRepoInstance(context.Background(), ri), emb
}

func ctxLearnItem(topic, category, title string, ctx map[string]any) map[string]any {
	item := map[string]any{
		"topic": topic, "category": category, "title": title,
		"body": "Body of " + title + ".", "type": "observation", "confidence": 0.8,
		"domain": []any{"cross-check"}, "entities": []any{title}, "refs": []any{},
	}
	if ctx != nil {
		item["context"] = ctx
	}
	return item
}

func learnItems(items ...map[string]any) mcpgo.CallToolRequest {
	facts := make([]any, len(items))
	for i, it := range items {
		facts[i] = it
	}
	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{"moment_name": "ctx", "facts": facts}
	return req
}

func branchTip(t *testing.T, svc *store.Service) string {
	t.Helper()
	h, err := svc.Branches().HeadCommit(context.Background(), "agent/test")
	require.NoError(t, err)
	return h
}

// learnedPaths returns every committed file of a learn result, in order.
func learnedPaths(t *testing.T, res *mcpgo.CallToolResult) []string {
	t.Helper()
	var parsed struct {
		Commits []struct {
			File string `json:"file"`
		} `json:"commits"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &parsed))
	out := make([]string, len(parsed.Commits))
	for i, c := range parsed.Commits {
		out[i] = c.File
	}
	return out
}

// A learn with a valid context writes it as one flow map; a time value is
// normalised to UTC.
func TestLearn_ContextPersists(t *testing.T) {
	_, svc, ctx, emb := newContextRepo(t, contextOntologyYAML)
	res, err := LearnHandler(emb)(ctx, learnItems(ctxLearnItem("verdicts", "t-17", "Verdict A",
		map[string]any{"task": "t-17", "verdict": "disagree", "score": 0.25, "at": "2026-10-01T02:00:00+02:00"})))
	require.NoError(t, err)
	require.False(t, res.IsError, resultText(t, res))
	content := readContent(t, svc, mergedFactPath(t, res))
	require.Contains(t, content, "\ncontext: {at: \"2026-10-01T00:00:00Z\", score: 0.25, task: t-17, verdict: disagree}\n")
}

// Every shape bound refuses the WHOLE call, naming the fact's index and the
// key, and nothing is written: the branch tip does not move. Fact 0 is valid
// in each case, so the refusal is of the batch, not of one fact. C7: an object
// or array value is refused by the HANDLER, not only by the JSON schema
// (scripts bypass the schema).
func TestLearn_ContextShapeRefusedWholeCall(t *testing.T) {
	_, svc, ctx, emb := newContextRepo(t, contextOntologyYAML)
	good := ctxLearnItem("notes", "a", "Good note", map[string]any{"task": "t-1"})
	seventeen := map[string]any{}
	for i := 0; i < 17; i++ {
		seventeen[fmt.Sprintf("k%02d", i)] = "x"
	}
	cases := []struct {
		name string
		ctx  map[string]any
		key  string
	}{
		{"17 keys", seventeen, "17 keys"},
		{"33-char key", map[string]any{strings.Repeat("k", 33): "x"}, `key "` + strings.Repeat("k", 33) + `"`},
		{"257 bytes", map[string]any{"note": strings.Repeat("x", 257)}, `key "note"`},
		{"newline", map[string]any{"note": "a\nb"}, `key "note"`},
		{"tab", map[string]any{"note": "a\tb"}, `key "note"`},
		{"line separator", map[string]any{"note": "a b"}, `key "note"`},
		// Invalid UTF-8 cannot reach this handler: decodeArg's JSON round trip
		// turns it into U+FFFD. SerializeFact's own gate refuses it
		// (fact.TestContext_SerializeShapeGate).
		{"list value", map[string]any{"note": []any{"a"}}, `key "note"`},
		{"object value", map[string]any{"note": map[string]any{"a": "b"}}, `key "note"`},
		{"null value", map[string]any{"note": nil}, `key "note"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := branchTip(t, svc)
			res, err := LearnHandler(emb)(ctx, learnItems(good, ctxLearnItem("notes", "b", "Bad note", c.ctx)))
			require.NoError(t, err)
			require.True(t, res.IsError, "must be refused")
			text := resultText(t, res)
			require.Contains(t, text, "fact 1:", "names the fact's index")
			require.Contains(t, text, c.key, "names the key")
			require.Equal(t, before, branchTip(t, svc), "nothing written: the branch tip did not move")
		})
	}
}

// The typed gate on learn: undeclared, wrong type, pattern, required, and a
// 129-byte value with no max_len; and a private-state fact with context.
func TestLearn_ContextTypedRefusals(t *testing.T) {
	_, svc, ctx, emb := newContextRepo(t, contextOntologyYAML)
	cases := []struct {
		item map[string]any
		want string
	}{
		{ctxLearnItem("verdicts", "t-1", "V", map[string]any{"task": "t-1", "verdict": "agree", "extra": "x"}), `context key "extra" at verdicts/t-1: not declared`},
		{ctxLearnItem("plain", "a", "P", map[string]any{"task": "t-1"}), `context key "task" at plain/a: not declared`},
		{ctxLearnItem("verdicts", "t-1", "V", map[string]any{"task": "t-1", "verdict": "agree", "score": "high"}), `context key "score"`},
		{ctxLearnItem("verdicts", "t-1", "V", map[string]any{"task": "x-1", "verdict": "agree"}), "pattern"},
		{ctxLearnItem("verdicts", "t-1", "V", map[string]any{"task": "t-1"}), `context key "verdict" at verdicts/t-1: required`},
		{ctxLearnItem("verdicts", "t-1", "V", nil), "required"},
		{ctxLearnItem("notes", "a", "N", map[string]any{"note": strings.Repeat("x", 129)}), "at most 128"},
	}
	for i, c := range cases {
		before := branchTip(t, svc)
		res, err := LearnHandler(emb)(ctx, learnItems(c.item))
		require.NoError(t, err)
		require.True(t, res.IsError, "case %d must be refused", i)
		require.Contains(t, resultText(t, res), c.want, "case %d", i)
		require.Contains(t, resultText(t, res), "fact 0:", "case %d", i)
		require.Equal(t, before, branchTip(t, svc))
	}

	// Private state: no topic, so nothing declares a key.
	before := branchTip(t, svc)
	res, err := LearnHandler(emb)(ctx, learnItems(map[string]any{
		"path": ".knomit/jobs/slot.md", "title": "Slot", "body": "state", "context": map[string]any{"task": "t-1"},
	}))
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Contains(t, resultText(t, res), "context is not allowed here")
	require.Equal(t, before, branchTip(t, svc))
}

// C2: with NO ontology, learn and update both skip ValidateFact — and must
// still refuse a context rather than write it unchecked.
func TestContext_NilOntologyRefusesEverywhere(t *testing.T) {
	_, svc, ctx, emb := newContextRepo(t, "")
	before := branchTip(t, svc)
	res, err := LearnHandler(emb)(ctx, learnItems(ctxLearnItem("notes", "a", "N", map[string]any{"task": "t-1"})))
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Contains(t, resultText(t, res), "context is not allowed here")
	require.Equal(t, before, branchTip(t, svc))

	// A fact without context still writes; an update adding one is refused.
	res, err = LearnHandler(emb)(ctx, learnItems(ctxLearnItem("notes", "a", "N", nil)))
	require.NoError(t, err)
	require.False(t, res.IsError, resultText(t, res))
	path := mergedFactPath(t, res)
	before = branchTip(t, svc)
	up := callUpdate(t, ctx, map[string]any{"file": path, "moment_name": "m", "updates": map[string]any{"context": map[string]any{"task": "t-1"}}})
	require.True(t, up.IsError)
	require.Contains(t, resultText(t, up), "context is not allowed here")
	require.Equal(t, before, branchTip(t, svc))
}

// C2, the learn-dedup path with no ontology: an EXISTING fact that carries a
// context (hand-written, via git) wins a merge; the merged fact would write
// that context with nothing to check it, so the learn is refused.
func TestLearn_DedupMergeNilOntologyRefusesWinnersContext(t *testing.T) {
	_, svc, ctx, emb := newContextRepo(t, "")
	const existing = "kb/notes/a/aaaaaaaa.md"
	title, body := "Shared claim", "The same claim, said once."
	f := fact.NewFact(existing)
	f.Title, f.Body, f.Type, f.Confidence, f.Sources = title, body, fact.Observation, 0.95, 1
	f.Domain, f.Entities, f.Refs = []string{"x"}, []string{"Shared claim"}, []string{}
	f.Context = map[string]any{"task": "t-1"}
	raw, err := fact.SerializeFact(f)
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(context.Background(), "agent/test", existing, raw, "seed", "test")
	require.NoError(t, err)

	before := branchTip(t, svc)
	item := ctxLearnItem("notes", "a", title, nil)
	item["body"], item["confidence"], item["entities"] = body, 0.5, []any{"Shared claim"}
	res, err := LearnHandler(emb)(ctx, learnItems(item))
	require.NoError(t, err)
	require.True(t, res.IsError, "the merge would write an unchecked context: %s", resultText(t, res))
	require.Contains(t, resultText(t, res), "dedup-merge into "+existing)
	require.Equal(t, before, branchTip(t, svc))
}

// Learn's dedup merge keeps the WINNER's map whole, nothing from the loser;
// when the existing fact wins, the result says the incoming context was not
// applied (as for expires).
//
// SABOTAGE: take the loser's map, or union both → red.
func TestLearn_DedupMergeKeepsWinnersWholeMap(t *testing.T) {
	_, svc, ctx, emb := newContextRepo(t, contextOntologyYAML)
	const existing = "kb/notes/a/aaaaaaaa.md"
	title, body := "Shared note", "The same note, said once."
	seed := func(conf float64) {
		f := fact.NewFact(existing)
		f.Title, f.Body, f.Type, f.Confidence, f.Sources = title, body, fact.Observation, conf, 1
		f.Domain, f.Entities, f.Refs = []string{"x"}, []string{"Shared note"}, []string{}
		f.Context = map[string]any{"note": "existing"}
		raw, err := fact.SerializeFact(f)
		require.NoError(t, err)
		_, err = svc.Facts().WriteFact(context.Background(), "agent/test", existing, raw, "seed", "test")
		require.NoError(t, err)
	}
	learn := func(conf float64) *mcpgo.CallToolResult {
		item := ctxLearnItem("notes", "a", title, map[string]any{"task": "t-9"})
		item["body"], item["confidence"], item["entities"] = body, conf, []any{"Shared note"}
		res, err := LearnHandler(emb)(ctx, learnItems(item))
		require.NoError(t, err)
		require.False(t, res.IsError, resultText(t, res))
		require.Equal(t, existing, mergedFactPath(t, res), "fixture: must merge into the existing fact")
		return res
	}

	seed(0.95)
	res := learn(0.5) // existing wins
	got, err := fact.ParseFact(existing, readContent(t, svc, existing))
	require.NoError(t, err)
	require.Equal(t, map[string]any{"note": "existing"}, got.Context, "the existing winner's map, whole; no incoming key")
	require.Contains(t, resultText(t, res), "existing fact kept; context not applied; set it with knomit_update on "+existing)

	seed(0.5)
	res = learn(0.95) // incoming wins
	got, err = fact.ParseFact(existing, readContent(t, svc, existing))
	require.NoError(t, err)
	require.Equal(t, map[string]any{"task": "t-9"}, got.Context, "the incoming winner's map, whole; the loser's key is absent")
	require.NotContains(t, resultText(t, res), "context not applied")
}

// Update: absent keeps the map; an object replaces it whole; {} clears it;
// null is refused. A confidence-only update keeps the map (C3) — this build
// keeps the key on every rewrite (an older binary dropped it). The typed gate
// runs on the replacement.
//
// SABOTAGE: treat an absent context as clear → the confidence-only case red.
func TestUpdate_ContextKeepReplaceClear(t *testing.T) {
	_, svc, ctx, emb := newContextRepo(t, contextOntologyYAML)
	res, err := LearnHandler(emb)(ctx, learnItems(ctxLearnItem("notes", "a", "Note", map[string]any{"task": "t-1", "note": "n"})))
	require.NoError(t, err)
	require.False(t, res.IsError, resultText(t, res))
	path := mergedFactPath(t, res)
	read := func() fact.Fact {
		f, err := fact.ParseFact(path, readContent(t, svc, path))
		require.NoError(t, err)
		return f
	}
	update := func(updates map[string]any) *mcpgo.CallToolResult {
		return callUpdate(t, ctx, map[string]any{"file": path, "moment_name": "m", "updates": updates})
	}

	r := update(map[string]any{"confidence": 0.6})
	require.False(t, r.IsError, resultText(t, r))
	require.Equal(t, map[string]any{"task": "t-1", "note": "n"}, read().Context, "absent keeps the map")

	r = update(map[string]any{"context": map[string]any{"note": "m"}})
	require.False(t, r.IsError, resultText(t, r))
	require.Equal(t, map[string]any{"note": "m"}, read().Context, "an object replaces the WHOLE map: task is gone")

	r = update(map[string]any{"context": map[string]any{"note": "a\nb"}})
	require.True(t, r.IsError)
	require.Contains(t, resultText(t, r), `updates.context: invalid context: key "note"`)

	r = update(map[string]any{"context": map[string]any{"nope": "x"}})
	require.True(t, r.IsError)
	require.Contains(t, resultText(t, r), `context key "nope" at notes/a: not declared`)
	require.Contains(t, resultText(t, r), "send a corrected context, or {} to clear it")

	r = update(map[string]any{"context": nil})
	require.True(t, r.IsError)
	require.Contains(t, resultText(t, r), "send {} to clear")

	r = update(map[string]any{"context": map[string]any{}})
	require.False(t, r.IsError, resultText(t, r))
	require.Nil(t, read().Context, "{} clears")
	require.NotContains(t, readContent(t, svc, path), "context")
}

// A fact that arrived via git with a well-shaped but UNDECLARED key reads and
// is kept; its next MCP write is refused with an actionable message, and
// sending a corrected map (or {}) fixes it.
func TestUpdate_HandPushedUndeclaredContextMustBeFixedOnNextWrite(t *testing.T) {
	_, svc, ctx, _ := newContextRepo(t, contextOntologyYAML)
	const p = "kb/notes/a/bbbbbbbb.md"
	raw := "---\ntype: observation\ndomain: []\nconfidence: 0.8\nsources: 1\nentities: []\nrefs: []\ncontext: {rogue: x}\n---\n# Hand pushed\n\nbody\n"
	_, err := svc.Facts().WriteFact(context.Background(), "agent/test", p, raw, "sync", "test")
	require.NoError(t, err)
	f, err := fact.ParseFact(p, readContent(t, svc, p))
	require.NoError(t, err)
	require.Equal(t, map[string]any{"rogue": "x"}, f.Context, "kept on read: the ontology is not consulted")

	r := callUpdate(t, ctx, map[string]any{"file": p, "moment_name": "m", "updates": map[string]any{"confidence": 0.5}})
	require.True(t, r.IsError)
	text := resultText(t, r)
	require.Contains(t, text, `context key "rogue" at notes/a: not declared`)
	require.Contains(t, text, "send a corrected context, or {} to clear it")

	r = callUpdate(t, ctx, map[string]any{"file": p, "moment_name": "m", "updates": map[string]any{"confidence": 0.5, "context": map[string]any{}}})
	require.False(t, r.IsError, resultText(t, r))
}

// C3, F04 move: learn with retract writes the moved fact at its NEW topic.
// The moved fact carries only the context the caller sends — learn builds a
// new fact, nothing is copied from the retracted one — and the new topic's
// typed gate runs on it.
func TestLearn_MoveRunsTheNewTopicsContextGate(t *testing.T) {
	_, svc, ctx, emb := newContextRepo(t, contextOntologyYAML)
	res, err := LearnHandler(emb)(ctx, learnItems(ctxLearnItem("notes", "a", "Movable", map[string]any{"note": "n"})))
	require.NoError(t, err)
	require.False(t, res.IsError, resultText(t, res))
	src := mergedFactPath(t, res)

	move := func(ctxMap map[string]any) *mcpgo.CallToolResult {
		req := learnItems(ctxLearnItem("verdicts", "t-5", "Movable", ctxMap))
		req.Params.Arguments.(map[string]any)["retract"] = []any{src}
		r, err := LearnHandler(emb)(ctx, req)
		require.NoError(t, err)
		return r
	}
	before := branchTip(t, svc)
	r := move(map[string]any{"note": "n"})
	require.True(t, r.IsError, "note is not declared under verdicts")
	require.Contains(t, resultText(t, r), `context key "note" at verdicts/t-5: not declared`)
	require.Equal(t, before, branchTip(t, svc))

	r = move(map[string]any{"task": "t-5", "verdict": "agree"})
	require.False(t, r.IsError, resultText(t, r))
	moved, err := fact.ParseFact("x", readContent(t, svc, learnedPaths(t, r)[0]))
	require.NoError(t, err)
	require.Equal(t, map[string]any{"task": "t-5", "verdict": "agree"}, moved.Context, "only what the caller sent")
}
