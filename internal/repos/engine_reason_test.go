package repos

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// Unit probes of engine_reason.js's own functions: the embedded script with
// its last line (the call to main) cut, then one expression evaluated on the
// same sandbox with the globals a run gets. No host function is called.
func evalReasonJS(t *testing.T, globals map[string]any, expr string) any {
	t.Helper()
	src := strings.TrimRight(embeddedReasonJS, "\n")
	i := strings.LastIndex(src, "\n")
	require.Equal(t, "main();", strings.TrimSpace(src[i+1:]), "the script ends by calling main")
	prog, err := fact.CompileScript("reason-probe", src[:i]+"\n"+expr)
	require.NoError(t, err)
	base := map[string]any{
		"question": "q",
		"run":      map[string]any{"id": "reason-00000000", "trust": 0.5, "max_moves": 12, "max_steps": 60},
		"world":    map[string]any{"repo": "r", "repo_id": "aaaaaaaaaaaa", "branch": "agent/x", "writable": true, "root": "kb"},
		"ontology": map[string]any{"context": map[string]any{}, "topics": []any{"markets", "science"}},
	}
	for k, v := range globals {
		base[k] = v
	}
	out, err := fact.RunScriptResult(context.Background(), prog, "reason-probe", base, fact.ScriptHost{}, time.Now().UTC())
	require.NoError(t, err)
	return out
}

// placeOf files under the write repo's OWN ontology root, which may be more
// than one segment, and refuses a path in another mount or under a topic
// the ontology does not declare (fix-later 9). Sabotage: restore the PR 1
// body (segs[1] as the topic) (red: the multi-segment root files the answer
// under topic "kb").
func TestReasonJS_PlaceOfMultiSegmentRoot(t *testing.T) {
	out := evalReasonJS(t, map[string]any{
		"world": map[string]any{"repo": "r", "repo_id": "aaaaaaaaaaaa", "branch": "agent/x", "writable": true, "root": "notes/kb"},
	}, `[
		placeOf("notes/kb/markets/firms/a1.md"),
		placeOf("kb://aaaaaaaaaaaa/notes/kb/science/earth/deep/a2.md"),
		placeOf("kb://bbbbbbbbbbbb/notes/kb/markets/firms/a3.md"),
		placeOf("notes/kb/gossip/firms/a4.md"),
		placeOf("kb/markets/firms/a5.md")
	]`)
	got := out.([]any)
	require.Equal(t, map[string]any{"topic": "markets", "category": "firms"}, got[0])
	require.Equal(t, map[string]any{"topic": "science", "category": "earth/deep"}, got[1], "its own kb:// form is local")
	require.Nil(t, got[2], "another mount's fact has no place in this repo")
	require.Nil(t, got[3], "a topic the write repo's ontology does not declare")
	require.Nil(t, got[4], "outside the root")
}

// The posterior (fix-later 4): one strong support reaches the trust bar on
// its own; several give the weakest; a synthesis whose member is also a
// support adds nothing on top; a contradiction weighed and outweighed leaves
// it; one that is not outweighed takes it under W/2. Sabotage: return the PR
// 1 formula s/(s+c+1) (red: one support of 0.9 gives 0.47).
func TestReasonJS_PosteriorCalibration(t *testing.T) {
	out := evalReasonJS(t, nil, `
		function put(path, type, conf, sources, verdict, refs) {
			st.facts[path] = {path: path, type: type, kind: "epistemic", confidence: conf, sources: sources, verdict: verdict,
				refs: refs || [], via: {question: true}, relevant: "yes"};
			st.order.push(path);
		}
		var r = {};
		put("kb/markets/a/1.md", "observation", 0.9, 1, "supports");
		r.one = posteriorOf();
		put("kb/markets/a/2.md", "observation", 0.7, 2, "supports");
		r.two = posteriorOf();
		put("kb/markets/a/3.md", "synthesis", 0.6, 1, "supports", ["kb/markets/a/1.md"]);
		r.synthOverMember = posteriorOf();
		put("kb/markets/a/4.md", "observation", 0.8, 1, "contradicts");
		r.outweighed = posteriorOf();
		put("kb/markets/a/5.md", "observation", 0.9, 3, "contradicts");
		r.notOutweighed = posteriorOf();
		put("kb/markets/a/6.md", "hypothesis", 0.99, 9, "supports");
		r.hypothesisNoMass = posteriorOf();
		r`)
	m := out.(map[string]any)
	require.Equal(t, 0.9, m["one"], "one support of 0.9 is 0.9, over the default bar 0.5")
	require.Equal(t, 0.7, m["two"], "the weakest premise")
	require.Equal(t, 0.7, m["synthOverMember"], "a synthesis citing a support adds nothing on top")
	require.Equal(t, 0.7, m["outweighed"], "C 0.8 < S 2.3: weighed and outweighed")
	// S = 0.9 + 1.4 = 2.3; C = 0.8 + 2.7 = 3.5; 0.7 * 2.3 / 5.8 = 0.2776
	require.Equal(t, 0.28, m["notOutweighed"])
	require.Equal(t, 0.28, m["hypothesisNoMass"], "a hypothesis never carries support mass")
}
