package materialize

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The mechanical title rules of design §5, one case per rule and the
// design's own examples.
//
// Sabotage: put the domains before the entities, keep the trailing "?", or
// drop the capital on a domains-only title, and a case fails.
func TestFactSetTitle_Rules(t *testing.T) {
	for _, c := range []struct {
		name string
		s    FactSetSearch
		want string
	}{
		{"text as a sentence", FactSetSearch{Text: "what is the best way to architect an agentic harness?"},
			"What is the best way to architect an agentic harness"},
		{"quotes dropped", FactSetSearch{Text: `"security vulnerability research"`, Entities: []string{"Anthropic"}},
			"Security vulnerability research for Anthropic"},
		{"text, entity, domain", FactSetSearch{Text: "best way to search", Entities: []string{"Anthropic"}, Domains: []string{"cost"}},
			"Best way to search for Anthropic (cost)"},
		{"two entities", FactSetSearch{Text: "x", Entities: []string{"A", "B"}}, "X for A and B"},
		{"many entities", FactSetSearch{Text: "x", Entities: []string{"A", "B", "C", "D"}}, "X for A, B and 2 more"},
		{"domains in parentheses", FactSetSearch{Text: "harness", Domains: []string{"cost", "security"}}, "Harness (cost, security)"},
		{"no text: entities lead", FactSetSearch{Entities: []string{"Anthropic"}, Domains: []string{"cost"}}, "Anthropic (cost)"},
		{"domains only", FactSetSearch{Domains: []string{"multi-agent", "orchestration"}}, "Multi-agent and orchestration"},
		{"domains only, many", FactSetSearch{Domains: []string{"a", "b", "c", "d", "e"}}, "A, b and 3 more"},
		{"path beside text is left out", FactSetSearch{Text: "x", Path: "kb/a/"}, "X"},
		{"other filters beside text are left out", FactSetSearch{Text: "x", Types: []string{"policy"}, Origins: []string{"authored"}, Motifs: []string{"m"}}, "X"},
		{"other filters beside entities are left out", FactSetSearch{Entities: []string{"A"}, Types: []string{"policy"}, Path: "kb/a/"}, "A"},
		{"other filters beside domains are left out", FactSetSearch{Domains: []string{"cost"}, Motifs: []string{"m-n"}, MinConfidence: 0.8}, "Cost"},
	} {
		require.Equal(t, c.want, FactSetTitle(c.s, "kb", "alpha"), c.name)
	}
}

// The fallback title (user ruling, 2026-10-29): with no text, entities or
// domains, the title is [head] [about <motifs>] [in <path>] [qualifiers],
// in words, sentence case. One case per row of the rule, then combinations.
//
// Sabotage: drop a part, keep the motif's hyphens, capitalize the motif
// after "about", pluralize "hypothesis" as "hypothesiss", or keep the old
// "Search results" fallback, and a case fails.
func TestFactSetTitle_FallbackRule(t *testing.T) {
	for _, c := range []struct {
		name string
		s    FactSetSearch
		want string
	}{
		// head
		{"one type, plural", FactSetSearch{Types: []string{"hypothesis"}}, "Hypotheses"},
		{"type ending in y", FactSetSearch{Types: []string{"policy"}}, "Policies"},
		{"type ending in s", FactSetSearch{Types: []string{"process"}}, "Processes"},
		{"plain type", FactSetSearch{Types: []string{"observation"}}, "Observations"},
		{"synthesis", FactSetSearch{Types: []string{"synthesis"}}, "Syntheses"},
		{"two types", FactSetSearch{Types: []string{"hypothesis", "policy"}}, "Hypotheses and policies"},
		{"many types", FactSetSearch{Types: []string{"hypothesis", "policy", "signal", "frame"}}, "Hypotheses, policies and 2 more"},
		{"kind", FactSetSearch{Kinds: []string{"pragmatic"}}, "Pragmatic facts"},
		{"two kinds", FactSetSearch{Kinds: []string{"epistemic", "pragmatic"}}, "Epistemic and pragmatic facts"},
		{"origin", FactSetSearch{Origins: []string{"distilled"}}, "Distilled facts"},
		{"type wins over kind and origin", FactSetSearch{Types: []string{"policy"}, Kinds: []string{"pragmatic"}, Origins: []string{"authored"}}, "Policies"},
		{"kind wins over origin", FactSetSearch{Kinds: []string{"epistemic"}, Origins: []string{"authored"}}, "Epistemic facts"},
		// motifs
		{"one motif alone is its words", FactSetSearch{Motifs: []string{"misattributed-env-defect"}}, "Misattributed env defect"},
		{"two motifs alone", FactSetSearch{Motifs: []string{"handle-outlives-target", "last-user-teardown"}}, "Handle outlives target and last user teardown"},
		{"many motifs alone", FactSetSearch{Motifs: []string{"a-b", "c-d", "e", "f", "g"}}, "A b, c d and 3 more"},
		{"type about a motif", FactSetSearch{Types: []string{"hypothesis"}, Motifs: []string{"misattributed-env-defect"}}, "Hypotheses about misattributed env defect"},
		{"motif with a path is Facts about", FactSetSearch{Motifs: []string{"stale-pin"}, Path: "kb/gotchas/"}, "Facts about stale pin in Gotchas"},
		// path
		{"type in a path", FactSetSearch{Types: []string{"invariant"}, Path: "kb/architecture/fleet/"}, "Invariants in Architecture › fleet"},
		{"path alone", FactSetSearch{Path: "kb/architecture/fleet/"}, "Facts in Architecture › fleet"},
		{"path outside the ontology root", FactSetSearch{Path: "notes/x"}, "Facts in Notes › x"},
		// qualifiers
		{"confidence alone", FactSetSearch{MinConfidence: 0.8}, "Facts (confidence ≥ 0.8)"},
		{"since the fork alone", FactSetSearch{SinceFork: true}, "Facts changed in this experiment"},
		{"motif with a qualifier is Facts about", FactSetSearch{Motifs: []string{"m-n"}, MinConfidence: 0.5}, "Facts about m n (confidence ≥ 0.5)"},
		// combinations
		{"everything", FactSetSearch{Types: []string{"hypothesis"}, Motifs: []string{"misattributed-env-defect", "stale-pin"}, Path: "kb/gotchas/ci/",
			SinceFork: true, MinConfidence: 0.8}, "Hypotheses about misattributed env defect and stale pin in Gotchas › ci changed in this experiment (confidence ≥ 0.8)"},
		{"origin, path, confidence", FactSetSearch{Origins: []string{"distilled"}, Path: "kb/decisions/", MinConfidence: 0.65}, "Distilled facts in Decisions (confidence ≥ 0.65)"},
		{"kind about motifs", FactSetSearch{Kinds: []string{"pragmatic"}, Motifs: []string{"a-b", "c"}}, "Pragmatic facts about a b and c"},
		// left out: they never make a title
		{"excluded types, episodes, similarity are left out", FactSetSearch{Types: []string{"policy"}, ExcludeTypes: []string{"signal"},
			Episodes: []string{"add"}, MinSimilarity: 0.5, ExcludeKinds: []string{"epistemic"}}, "Policies"},
		// nothing at all
		{"nothing", FactSetSearch{}, "All facts in alpha"},
		{"only skipped filters", FactSetSearch{ExcludeTypes: []string{"signal"}, Episodes: []string{"add"}}, "All facts in alpha"},
	} {
		require.Equal(t, c.want, FactSetTitle(c.s, "kb", "alpha"), c.name)
	}
	require.Equal(t, "All facts", FactSetTitle(FactSetSearch{}, "kb", ""), "no name")
}

// The full-search line names every filter, the skipped ones included.
func TestSearchLine_SpellsOutTheWholeSearch(t *testing.T) {
	s := FactSetSearch{Text: "x?", Entities: []string{"A"}, Domains: []string{"d"}, Path: "kb/p/", Types: []string{"policy"},
		Motifs: []string{"m"}, MotifMatch: "stem", MinConfidence: 0.8, Origins: []string{"authored"}, Sort: SortRecent}
	require.Equal(t, `text "x?" · entities A · domains d · path kb/p/ · type policy · origin authored · motifs (stem) m · confidence ≥ 0.8 · sorted by recent`, SearchLine(s))
}
