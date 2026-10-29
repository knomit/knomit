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
		{"path only", BookSearch{Path: "kb/architecture/fleet/"}, "Architecture › fleet"},
		{"path beside text is left out", BookSearch{Text: "x", Path: "kb/a/"}, "X"},
		{"skipped filters only", BookSearch{Types: []string{"observation"}, Motifs: []string{"m"}, MinConfidence: 0.8}, "Search results"},
		{"skipped filters do not enter", BookSearch{Text: "x", Types: []string{"policy"}, Origins: []string{"authored"}}, "X"},
	} {
		require.Equal(t, c.want, BookTitle(c.s, "kb"), c.name)
	}
}

// The full-search line names every filter, the skipped ones included.
func TestSearchLine_SpellsOutTheWholeSearch(t *testing.T) {
	s := BookSearch{Text: "x?", Entities: []string{"A"}, Domains: []string{"d"}, Path: "kb/p/", Types: []string{"policy"},
		Motifs: []string{"m"}, MotifMatch: "stem", MinConfidence: 0.8, Origins: []string{"authored"}, Sort: SortRecent}
	require.Equal(t, `text "x?" · entities A · domains d · path kb/p/ · type policy · origin authored · motifs (stem) m · confidence ≥ 0.8 · sorted by recent`, SearchLine(s))
}
