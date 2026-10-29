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
func TestBookTitle_Rules(t *testing.T) {
	for _, c := range []struct {
		name string
		s    BookSearch
		want string
	}{
		{"text as a sentence", BookSearch{Text: "what is the best way to architect an agentic harness?"},
			"What is the best way to architect an agentic harness"},
		{"quotes dropped", BookSearch{Text: `"security vulnerability research"`, Entities: []string{"Anthropic"}},
			"Security vulnerability research for Anthropic"},
		{"text, entity, domain", BookSearch{Text: "best way to search", Entities: []string{"Anthropic"}, Domains: []string{"cost"}},
			"Best way to search for Anthropic (cost)"},
		{"two entities", BookSearch{Text: "x", Entities: []string{"A", "B"}}, "X for A and B"},
		{"many entities", BookSearch{Text: "x", Entities: []string{"A", "B", "C", "D"}}, "X for A, B and 2 more"},
		{"domains in parentheses", BookSearch{Text: "harness", Domains: []string{"cost", "security"}}, "Harness (cost, security)"},
		{"no text: entities lead", BookSearch{Entities: []string{"Anthropic"}, Domains: []string{"cost"}}, "Anthropic (cost)"},
		{"domains only", BookSearch{Domains: []string{"multi-agent", "orchestration"}}, "Multi-agent and orchestration"},
		{"domains only, many", BookSearch{Domains: []string{"a", "b", "c", "d", "e"}}, "A, b and 3 more"},
		{"path beside text is left out", BookSearch{Text: "x", Path: "kb/a/"}, "X"},
		{"other filters beside text are left out", BookSearch{Text: "x", Types: []string{"policy"}, Origins: []string{"authored"}, Motifs: []string{"m"}}, "X"},
		{"other filters beside entities are left out", BookSearch{Entities: []string{"A"}, Types: []string{"policy"}, Path: "kb/a/"}, "A"},
		{"other filters beside domains are left out", BookSearch{Domains: []string{"cost"}, Motifs: []string{"m-n"}, MinConfidence: 0.8}, "Cost"},
	} {
		require.Equal(t, c.want, BookTitle(c.s, "kb", "alpha"), c.name)
	}
}

// The fallback title (user ruling, 2026-10-29): with no text, entities or
// domains, the title is [head] [about <motifs>] [in <path>] [qualifiers],
// in words, sentence case. One case per row of the rule, then combinations.
//
// Sabotage: drop a part, keep the motif's hyphens, capitalize the motif
// after "about", pluralize "hypothesis" as "hypothesiss", or keep the old
// "Search results" fallback, and a case fails.
func TestBookTitle_FallbackRule(t *testing.T) {
	for _, c := range []struct {
		name string
		s    BookSearch
		want string
	}{
		// head
		{"one type, plural", BookSearch{Types: []string{"hypothesis"}}, "Hypotheses"},
		{"type ending in y", BookSearch{Types: []string{"policy"}}, "Policies"},
		{"type ending in s", BookSearch{Types: []string{"process"}}, "Processes"},
		{"plain type", BookSearch{Types: []string{"observation"}}, "Observations"},
		{"synthesis", BookSearch{Types: []string{"synthesis"}}, "Syntheses"},
		{"two types", BookSearch{Types: []string{"hypothesis", "policy"}}, "Hypotheses and policies"},
		{"many types", BookSearch{Types: []string{"hypothesis", "policy", "signal", "frame"}}, "Hypotheses, policies and 2 more"},
		{"kind", BookSearch{Kinds: []string{"pragmatic"}}, "Pragmatic facts"},
		{"two kinds", BookSearch{Kinds: []string{"epistemic", "pragmatic"}}, "Epistemic and pragmatic facts"},
		{"origin", BookSearch{Origins: []string{"distilled"}}, "Distilled facts"},
		{"type wins over kind and origin", BookSearch{Types: []string{"policy"}, Kinds: []string{"pragmatic"}, Origins: []string{"authored"}}, "Policies"},
		{"kind wins over origin", BookSearch{Kinds: []string{"epistemic"}, Origins: []string{"authored"}}, "Epistemic facts"},
		// motifs
		{"one motif alone is its words", BookSearch{Motifs: []string{"misattributed-env-defect"}}, "Misattributed env defect"},
		{"two motifs alone", BookSearch{Motifs: []string{"handle-outlives-target", "last-user-teardown"}}, "Handle outlives target and last user teardown"},
		{"many motifs alone", BookSearch{Motifs: []string{"a-b", "c-d", "e", "f", "g"}}, "A b, c d and 3 more"},
		{"type about a motif", BookSearch{Types: []string{"hypothesis"}, Motifs: []string{"misattributed-env-defect"}}, "Hypotheses about misattributed env defect"},
		{"motif with a path is Facts about", BookSearch{Motifs: []string{"stale-pin"}, Path: "kb/gotchas/"}, "Facts about stale pin in Gotchas"},
		// path
		{"type in a path", BookSearch{Types: []string{"invariant"}, Path: "kb/architecture/fleet/"}, "Invariants in Architecture › fleet"},
		{"path alone", BookSearch{Path: "kb/architecture/fleet/"}, "Facts in Architecture › fleet"},
		{"path outside the ontology root", BookSearch{Path: "notes/x"}, "Facts in Notes › x"},
		// qualifiers
		{"confidence alone", BookSearch{MinConfidence: 0.8}, "Facts (confidence ≥ 0.8)"},
		{"since the fork alone", BookSearch{SinceFork: true}, "Facts changed in this experiment"},
		{"motif with a qualifier is Facts about", BookSearch{Motifs: []string{"m-n"}, MinConfidence: 0.5}, "Facts about m n (confidence ≥ 0.5)"},
		// combinations
		{"everything", BookSearch{Types: []string{"hypothesis"}, Motifs: []string{"misattributed-env-defect", "stale-pin"}, Path: "kb/gotchas/ci/",
			SinceFork: true, MinConfidence: 0.8}, "Hypotheses about misattributed env defect and stale pin in Gotchas › ci changed in this experiment (confidence ≥ 0.8)"},
		{"origin, path, confidence", BookSearch{Origins: []string{"distilled"}, Path: "kb/decisions/", MinConfidence: 0.65}, "Distilled facts in Decisions (confidence ≥ 0.65)"},
		{"kind about motifs", BookSearch{Kinds: []string{"pragmatic"}, Motifs: []string{"a-b", "c"}}, "Pragmatic facts about a b and c"},
		// left out: they never make a title
		{"excluded types, episodes, similarity are left out", BookSearch{Types: []string{"policy"}, ExcludeTypes: []string{"signal"},
			Episodes: []string{"add"}, MinSimilarity: 0.5, ExcludeKinds: []string{"epistemic"}}, "Policies"},
		// nothing at all
		{"nothing", BookSearch{}, "All facts in alpha"},
		{"only skipped filters", BookSearch{ExcludeTypes: []string{"signal"}, Episodes: []string{"add"}}, "All facts in alpha"},
	} {
		require.Equal(t, c.want, BookTitle(c.s, "kb", "alpha"), c.name)
	}
	require.Equal(t, "All facts", BookTitle(BookSearch{}, "kb", ""), "no name")
}

// The full-search line names every filter, the skipped ones included.
func TestSearchLine_SpellsOutTheWholeSearch(t *testing.T) {
	s := BookSearch{Text: "x?", Entities: []string{"A"}, Domains: []string{"d"}, Path: "kb/p/", Types: []string{"policy"},
		Motifs: []string{"m"}, MotifMatch: "stem", MinConfidence: 0.8, Origins: []string{"authored"}, Sort: SortRecent}
	require.Equal(t, `text "x?" · entities A · domains d · path kb/p/ · type policy · origin authored · motifs (stem) m · confidence ≥ 0.8 · sorted by recent`, SearchLine(s))
}
