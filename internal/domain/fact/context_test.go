package fact

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// ctxFact is a fact file whose frontmatter ends with the given context line(s).
func ctxFact(contextYAML string) string {
	return "---\ntype: observation\ndomain: []\nconfidence: 0.8\nsources: 1\nentities: []\nrefs: []\n" +
		contextYAML + "---\n# Verdict\n\nbody\n"
}

// A fact with context serializes it as ONE flow map with sorted keys after
// expires, and reads back to the same typed values; a second round trip is
// byte-identical.
func TestContext_SerializeParseRoundTrip(t *testing.T) {
	f := NewFact("kb/verdicts/t-17/a.md")
	f.Title, f.Body, f.Type, f.Confidence, f.Sources = "Verdict", "body", Observation, 0.8, 1
	f.Expires = "2026-10-01T00:00:00Z"
	f.Domain, f.Entities, f.Refs = []string{}, []string{}, []string{}
	f.Context = map[string]any{"verdict": "disagree", "task": "t-17", "score": 0.5, "final": true, "count": 3}

	text, err := SerializeFact(f)
	require.NoError(t, err)
	require.Contains(t, text,
		"expires: \"2026-10-01T00:00:00Z\"\ncontext: {count: 3, final: true, score: 0.5, task: t-17, verdict: disagree}\n---\n",
		"one flow map, sorted keys, after expires")

	back, err := ParseFact(f.Path(), text)
	require.NoError(t, err)
	require.Empty(t, back.ContextWarnings)
	require.Equal(t, map[string]any{"verdict": "disagree", "task": "t-17", "score": 0.5, "final": true, "count": 3.0}, back.Context)

	again, err := SerializeFact(back)
	require.NoError(t, err)
	require.Equal(t, text, again, "byte-identical after the first normalisation")
}

// A string that would read back as another type is quoted on write, so it
// stays a string: "true", "17", a timestamp.
func TestContext_StringsThatLookTypedStayStrings(t *testing.T) {
	f := NewFact("kb/x/a.md")
	f.Title, f.Type, f.Confidence = "T", Observation, 0.5
	f.Context = map[string]any{"a": "true", "b": "17", "c": "2026-10-01", "d": "null", "e": "a: b"}
	text, err := SerializeFact(f)
	require.NoError(t, err)
	back, err := ParseFact(f.Path(), text)
	require.NoError(t, err)
	require.Equal(t, f.Context, back.Context, "every value reads back as the same string")
}

// An unquoted date in a hand-written file is the expires trap: yaml.v3 would
// make it a time.Time. Read by tag, it stays the STRING as written.
func TestContext_UnquotedDateStaysAString(t *testing.T) {
	f, err := ParseFact("kb/x/a.md", ctxFact("context: {due: 2026-10-01, at: 2026-10-01T10:00:00Z}\n"))
	require.NoError(t, err)
	require.Empty(t, f.ContextWarnings)
	require.Equal(t, "2026-10-01", f.Context["due"])
	require.Equal(t, "2026-10-01T10:00:00Z", f.Context["at"])
}

// C4: lenient read never makes a fact unloadable. Every malformed shape loads
// the fact with NO context and one warning — including a map with one good key
// and one bad one (the whole map is dropped, never half of it) and a duplicate
// key (yaml.v3 does not check duplicates inside a yaml.Node).
func TestContext_MalformedMapDroppedWholeFactStillLoads(t *testing.T) {
	cases := map[string]string{
		"scalar":           "context: \"x\"\n",
		"list":             "context: [a]\n",
		"list value":       "context: {a: [1]}\n",
		"object value":     "context: {a: {b: 1}}\n",
		"null value":       "context: {a: ~}\n",
		"alias":            "anchor: &x hello\ncontext: {a: *x}\n",
		"duplicate key":    "context: {a: x, a: y}\n",
		"one good one bad": "context: {good: x, bad: [1]}\n",
		"newline escape":   "context: {a: \"x\\ny\"}\n",
		"tab escape":       "context: {a: \"x\\ty\"}\n",
		"line separator":   "context: {a: \"x\\u2028y\"}\n",
		"bidi override":    "context: {a: \"x\\u202Ey\"}\n",
		"bidi isolate":     "context: {a: \"x\\u2067y\"}\n",
		"bidi LRM":         "context: {a: \"x\\u200Ey\"}\n",
		"bidi RLM":         "context: {a: \"x\\u200Fy\"}\n",
		"bidi ALM":         "context: {a: \"x\\u061Cy\"}\n",
		"bad key":          "context: {Task: x}\n",
		"long key":         "context: {" + strings.Repeat("k", 33) + ": x}\n",
		"over hard cap":    "context: {a: " + strings.Repeat("x", 257) + "}\n",
		"seventeen keys":   "context: {" + seventeenKeys() + "}\n",
		"infinite number":  "context: {a: .inf}\n",
	}
	for name, yml := range cases {
		t.Run(name, func(t *testing.T) {
			f, err := ParseFact("kb/x/a.md", ctxFact(yml))
			require.NoError(t, err, "a malformed context must never make the fact unloadable")
			require.Nil(t, f.Context, "the whole map is dropped")
			require.Len(t, f.ContextWarnings, 1)
			require.Equal(t, "Verdict", f.Title)
		})
	}
	t.Run("absent, null and empty are no context and no warning", func(t *testing.T) {
		for _, yml := range []string{"", "context:\n", "context: ~\n", "context: {}\n"} {
			f, err := ParseFact("kb/x/a.md", ctxFact(yml))
			require.NoError(t, err)
			require.Nil(t, f.Context, yml)
			require.Empty(t, f.ContextWarnings, yml)
		}
	})
}

func seventeenKeys() string {
	parts := make([]string, 17)
	for i := range parts {
		parts[i] = "k" + string(rune('a'+i)) + ": x"
	}
	return strings.Join(parts, ", ")
}

// The shape gate in SerializeFact refuses each bound by one, naming the key.
func TestContext_SerializeShapeGate(t *testing.T) {
	base := func(ctx map[string]any) Fact {
		f := NewFact("kb/x/a.md")
		f.Title, f.Type, f.Confidence = "T", Observation, 0.5
		f.Context = ctx
		return f
	}
	seventeen := map[string]any{}
	sixteen := map[string]any{}
	for i := 0; i < 17; i++ {
		k := "k" + string(rune('a'+i))
		seventeen[k] = "x"
		if i < 16 {
			sixteen[k] = "x"
		}
	}
	ok := []map[string]any{
		sixteen,
		{strings.Repeat("k", 32): "x"},
		{"a": strings.Repeat("x", 256)},
		{"a_1": "ünïcødé ok", "b": -1.5, "c": false},
		// Just outside both bidi ranges: U+2029 is refused as a line break,
		// so the neighbours checked are U+202F (narrow no-break space) and
		// U+206A (deprecated, but not a bidi isolate).
		{"a": "x y", "b": "x⁪y"},
	}
	// Neighbours of the three implicit marks stay allowed: U+200D ZERO WIDTH
	// JOINER (emoji sequences), U+200B ZERO WIDTH SPACE, U+061B ARABIC
	// SEMICOLON and U+061D.
	ok = append(ok, map[string]any{"a": "x\u200dy", "b": "x\u200by", "c": "x\u061by", "d": "x\u061dy"})
	for _, c := range ok {
		_, err := SerializeFact(base(c))
		require.NoError(t, err)
	}
	bad := []struct {
		ctx  map[string]any
		want string
	}{
		{seventeen, "17 keys"},
		{map[string]any{strings.Repeat("k", 33): "x"}, `key "kkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkk"`},
		{map[string]any{"Task": "x"}, `key "Task"`},
		{map[string]any{"task-id": "x"}, `key "task-id"`},
		{map[string]any{"a": strings.Repeat("x", 257)}, `key "a"`},
		{map[string]any{"a": "x\ny"}, `key "a"`},
		{map[string]any{"a": "x\ry"}, `key "a"`},
		{map[string]any{"a": "x\ty"}, `key "a"`},
		{map[string]any{"a": "x\u2028y"}, `key "a"`},
		{map[string]any{"a": "x\u0085y"}, `key "a"`},
		// Bidi format characters, each end of both ranges (user ruling).
		{map[string]any{"a": "x‪y"}, "bidirectional"},
		{map[string]any{"a": "x‮y"}, "bidirectional"},
		{map[string]any{"a": "x⁦y"}, "bidirectional"},
		{map[string]any{"a": "x⁩y"}, "bidirectional"},
		// The implicit directional marks (user ruling 2026-10-06).
		{map[string]any{"a": "x\u200ey"}, "found U+200E"},
		{map[string]any{"a": "x\u200fy"}, "found U+200F"},
		{map[string]any{"a": "x\u061cy"}, "found U+061C"},
		{map[string]any{"a": "\xff"}, `key "a"`},
		{map[string]any{"a": []any{"x"}}, `key "a"`},
		{map[string]any{"a": map[string]any{"b": 1}}, `key "a"`},
		{map[string]any{"a": nil}, `key "a"`},
	}
	for _, c := range bad {
		_, err := SerializeFact(base(c.ctx))
		require.ErrorIs(t, err, ErrInvalidContext, "%v", c.ctx)
		require.Contains(t, err.Error(), c.want)
	}
}

const verdictOntologyYAML = `id: t
name: T
topics:
  verdicts:
    description: verdicts
    context:
      task:    {type: string, required: true, pattern: "^t-[a-z0-9-]{1,40}$"}
      verdict: {type: enum, values: [agree, disagree, unsure], required: true}
      score:   {type: number, min: 0, max: 1}
      note:    {type: string}
      long:    {type: string, max_len: 256}
      short:   {type: string, max_len: 3}
      final:   {type: bool}
      at:      {type: time}
    children:
      strict:
        description: child redeclares note
        context:
          note: {type: string, max_len: 2}
  plain:
    description: no context anywhere
`

func verdictOntology(t *testing.T) *Ontology {
	t.Helper()
	o, err := ParseNewOntology([]byte(verdictOntologyYAML))
	require.NoError(t, err)
	return o
}

func TestValidateContext_TypedGate(t *testing.T) {
	o := verdictOntology(t)
	good := func(extra map[string]any) map[string]any {
		m := map[string]any{"task": "t-17", "verdict": "disagree"}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	require.NoError(t, ValidateContext(o, "verdicts/t-17", good(nil)))
	require.NoError(t, ValidateContext(o, "verdicts", good(map[string]any{"score": 1.0, "final": true, "at": "2026-10-01T00:00:00Z", "note": strings.Repeat("x", 128), "long": strings.Repeat("x", 256)})))

	refused := []struct {
		topic, key string
		ctx        map[string]any
		want       string
	}{
		{"verdicts", "nope", good(map[string]any{"nope": "x"}), "not declared"},
		{"plain", "task", map[string]any{"task": "t-1"}, "not declared"},
		{"verdicts", "task", map[string]any{"verdict": "agree"}, "required"},
		{"verdicts", "verdict", map[string]any{"task": "t-1"}, "required"},
		{"verdicts", "task", map[string]any{"task": "x-1", "verdict": "agree"}, "pattern"},
		{"verdicts", "verdict", good(map[string]any{"verdict": "maybe"}), "not one of"},
		{"verdicts", "score", good(map[string]any{"score": 1.5}), "above the maximum"},
		{"verdicts", "score", good(map[string]any{"score": -0.1}), "below the minimum"},
		{"verdicts", "score", good(map[string]any{"score": "0.5"}), "must be a number"},
		{"verdicts", "final", good(map[string]any{"final": "true"}), "must be a bool"},
		{"verdicts", "task", map[string]any{"task": 17.0, "verdict": "agree"}, "must be a string"},
		{"verdicts", "note", good(map[string]any{"note": strings.Repeat("x", 129)}), "at most 128"},
		{"verdicts", "short", good(map[string]any{"short": "abcd"}), "at most 3"},
		{"verdicts", "at", good(map[string]any{"at": "2026-10-01"}), "RFC 3339"},
	}
	for _, c := range refused {
		err := ValidateContext(o, c.topic, c.ctx)
		var ce *ContextError
		require.ErrorAs(t, err, &ce, "%s %v", c.topic, c.ctx)
		require.Equal(t, c.key, ce.Key)
		require.Equal(t, c.topic, ce.Topic)
		require.Contains(t, err.Error(), c.want)
		require.Contains(t, err.Error(), "send a corrected context, or {} to clear it", "the refusal is actionable")
	}
}

// A child's redeclaration overrides the parent's: the CHILD's max_len 2 is
// what refuses "abc", which the parent's default 128 would accept.
func TestValidateContext_ChildRedeclarationWins(t *testing.T) {
	o := verdictOntology(t)
	ctx := map[string]any{"task": "t-1", "verdict": "agree", "note": "abc"}
	require.NoError(t, ValidateContext(o, "verdicts/other", ctx), "parent rule: 3 bytes is fine")
	err := ValidateContext(o, "verdicts/strict/deeper", ctx)
	require.ErrorContains(t, err, "at most 2", "the child's own rule applied")
	require.NoError(t, ValidateContext(o, "verdicts/strict", map[string]any{"task": "t-1", "verdict": "agree", "note": "ab"}))
	require.Error(t, ValidateContext(o, "verdicts/strict", map[string]any{"verdict": "agree"}), "inherited required keys still apply under the child")
}

// No ontology, or no context declared anywhere on the walk: every key is
// undeclared, and an empty map is fine.
func TestValidateContext_NothingDeclared(t *testing.T) {
	require.ErrorIs(t, ValidateContext(nil, "verdicts", map[string]any{"a": "x"}), ErrContextWithoutOntology)
	require.NoError(t, ValidateContext(nil, "verdicts", nil))
	f := NewFact("kb/verdicts/a.md")
	f.Context = map[string]any{"a": "x"}
	require.ErrorIs(t, ValidateFact(nil, "verdicts", f), ErrContextWithoutOntology, "ValidateFact refuses with a nil ontology too")
	require.NoError(t, ValidateContext(verdictOntology(t), "plain", map[string]any{}))
}

// NormalizeContext rewrites time-typed values to UTC Z, and nothing else.
func TestNormalizeContext_TimesToUTC(t *testing.T) {
	o := verdictOntology(t)
	got := NormalizeContext(o, "verdicts", map[string]any{"at": "2026-10-01T02:00:00+02:00", "note": "2026-10-01T02:00:00+02:00", "score": 1})
	require.Equal(t, "2026-10-01T00:00:00Z", got["at"])
	require.Equal(t, "2026-10-01T02:00:00+02:00", got["note"], "a string key is never reinterpreted")
	require.Equal(t, 1.0, got["score"], "ints widen to float64")
}

// A bad declaration is a WARNING on the open path and fatal for a new
// ontology (the F02 rule), and it is POISONED: its key is refused with the
// reason rather than falling back to anything.
func TestContextDeclarations_ParseRules(t *testing.T) {
	bad := map[string]string{
		"unknown type":      `k: {type: list}`,
		"no type":           `k: {required: true}`,
		"enum no values":    `k: {type: enum}`,
		"values on string":  `k: {type: string, values: [a]}`,
		"pattern on number": `k: {type: number, pattern: "x"}`,
		"bad pattern":       `k: {type: string, pattern: "("}`,
		"min on string":     `k: {type: string, min: 1}`,
		"min above max":     `k: {type: number, min: 2, max: 1}`,
		"max_len 300":       `k: {type: string, max_len: 300}`,
		"max_len on enum":   `k: {type: enum, values: [a], max_len: 3}`,
		"bad key":           `Bad-Key: {type: string}`,
		// An enum value is a context string and takes the same shape rule,
		// bidi characters included. A git-arrived ontology carrying one keeps
		// LOADING (no fall-back to the embedded default); only that key is
		// poisoned. The three implicit marks (user ruling 2026-10-06) take
		// exactly the path the override and isolate already took.
		"enum value RLO": `k: {type: enum, values: ["a‮b"]}`,
		"enum value LRM": `k: {type: enum, values: ["a‎b"]}`,
		"enum value RLM": `k: {type: enum, values: ["a‏b"]}`,
		"enum value ALM": `k: {type: enum, values: ["a؜b"]}`,
	}
	for name, decl := range bad {
		t.Run(name, func(t *testing.T) {
			yml := "id: t\nname: T\ntopics:\n  a:\n    description: x\n    context:\n      " + decl + "\n"
			o, err := ParseOntology([]byte(yml))
			require.NoError(t, err, "the open path must keep reading the repository's own ontology")
			_, err = ParseNewOntology([]byte(yml))
			require.Error(t, err, "a new ontology is told now")
			key := strings.TrimSpace(strings.SplitN(decl, ":", 2)[0])
			err = ValidateContext(o, "a", map[string]any{key: "x"})
			require.ErrorContains(t, err, "declaration is invalid", "poisoned, not dropped")
		})
	}
}

// Serialize keeps context declarations (initSeed and custom create serialize
// before committing), and a stored ontology with declarations the preset lacks
// has diverged from it, so the boot refresh leaves it alone.
func TestContextDeclarations_SerializeAndDivergence(t *testing.T) {
	o := verdictOntology(t)
	out, err := o.Serialize()
	require.NoError(t, err)
	back, err := ParseNewOntology(out)
	require.NoError(t, err, string(out))
	require.Equal(t, o.Topics["verdicts"].Context, back.Topics["verdicts"].Context)
	require.Equal(t, o.Topics["verdicts"].Children["strict"].Context, back.Topics["verdicts"].Children["strict"].Context)

	// The same taxonomy with no context blocks.
	plain, err := ParseNewOntology([]byte("id: t\nname: T\ntopics:\n  verdicts:\n    description: verdicts\n" +
		"    children:\n      strict:\n        description: x\n  plain:\n    description: x\n"))
	require.NoError(t, err)
	require.Equal(t, DivergenceContext, o.SubsetDivergence(plain))
	require.Equal(t, "", o.SubsetDivergence(back), "identical declarations are a subset")
}

// Rules and triggers see fact.context (the same map), {} when absent.
func TestFactToJS_Context(t *testing.T) {
	yml := "id: t\nname: T\ntopics:\n  v:\n    description: x\n    context:\n      score: {type: number}\n" +
		"    validations:\n      - name: score-bounded\n        message: score must be at most 1\n        rule: \"fact.context.score === undefined || fact.context.score <= 1\"\n"
	o, err := ParseNewOntology([]byte(yml))
	require.NoError(t, err)
	f := NewFact("kb/v/a.md")
	f.Context = map[string]any{"score": 0.5}
	require.NoError(t, ValidateFact(o, "v", f))
	f.Context = map[string]any{"score": 2.0}
	var ve *ValidationError
	require.ErrorAs(t, ValidateFact(o, "v", f), &ve, "the rule ran over fact.context")
	f.Context = nil
	require.NoError(t, ValidateFact(o, "v", f), "fact.context is {} when absent: reading a key never throws")
	require.Equal(t, map[string]any{}, FactGlobal(NewFact("kb/v/b.md"))["context"])
}

// JSON carries context as an object (work items, REST), and reads it back.
func TestContext_JSONRoundTrip(t *testing.T) {
	f := NewFact("kb/v/a.md")
	f.Context = map[string]any{"task": "t-17", "score": 0.5}
	b, err := f.MarshalJSON()
	require.NoError(t, err)
	require.Contains(t, string(b), `"context":{"score":0.5,"task":"t-17"}`)
	var back Fact
	require.NoError(t, back.UnmarshalJSON(b))
	require.Equal(t, f.Context, back.Context)
	plain, err := NewFact("kb/v/b.md").MarshalJSON()
	require.NoError(t, err)
	require.NotContains(t, string(plain), "context", "omitted when empty")
}

func TestExtractContext(t *testing.T) {
	require.Equal(t, map[string]any{"task": "t-17"}, ExtractContext([]byte(ctxFact("context: {task: t-17}\n"))))
	require.Nil(t, ExtractContext([]byte(ctxFact(""))))
	require.Nil(t, ExtractContext([]byte(ctxFact("context: {a: [1]}\n"))), "lenient exactly like ParseFact")
	require.Nil(t, ExtractContext([]byte("no frontmatter, says context")))
}
