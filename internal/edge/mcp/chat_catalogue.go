package mcp

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"knomit/internal/engine/llm"
)

// toolPurposes is the one authored table of catalogue lines: for each tool the
// server registers, what it is for, in one line. The wording is the user's
// (design C3, approved); the catalogue builder reads nothing else for a tool
// that has a row. A new tool without a row fails
// TestCatalogue_EveryRegisteredToolHasAPurpose; a tool registered at runtime
// (a script tool) has no row and gets a derived purpose instead.
var toolPurposes = map[string]string{
	"knomit_repos":       "list the repos and lenses you can bind to",
	"knomit_bind":        "bind to one repo or lens and get the binding handle every other call needs",
	"knomit_query":       "search facts by text, entities, domain, path or confidence",
	"knomit_explain":     "read one fact in full, with its history and provenance",
	"knomit_materialize": "read a fact together with the facts it rests on, as one document",
	"knomit_changes":     "list facts added, changed or removed under a folder since a commit",
	"knomit_skill":       "list or read this repo's own how-to skills",
	"knomit_learn":       "write a new fact (the person approves first)",
	"knomit_update":      "change an existing fact (the person approves first)",
	"knomit_retract":     "remove a fact (the person approves first)",
	"knomit_review":      "run the knowledge-base maintenance review",
	"knomit_hypothesize": "generate predictions from existing facts; only when the person asks",
	"knomit_experiment":  "open, sync, commit or roll back an experiment branch",
}

// CatalogueTooBigError says the tool names alone do not fit the catalogue
// budget: Need tokens (bytes/4) against Budget. The chat turns it into the
// opening-too-big refusal (llm.OpeningTooBigError) when it opens.
type CatalogueTooBigError struct{ Need, Budget int }

func (e *CatalogueTooBigError) Error() string {
	return fmt.Sprintf("the tool catalogue needs about %d tokens for the names alone, and %d fit", e.Need, e.Budget)
}

// BuildCatalogue is the catalogue the loader tool's description carries: one
// line per tool, "- name: purpose", sorted by name. It is exactly the tools the
// principal was shown, so a read-only principal's holds no write tool.
//
// budget is the catalogue's allowance in tokens (bytes/4); 0 or less means no
// limit. A tool with no row in toolPurposes gets the first sentence of its
// description, cut to its share of the budget. If the lines do not fit, the
// catalogue is the names only ("- name"); if the names do not fit, the error is
// a *CatalogueTooBigError and no tool count or size is assumed anywhere.
func BuildCatalogue(tools []llm.ToolDef, budget int) (string, error) {
	sorted := append([]llm.ToolDef(nil), tools...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	lines := make([]string, len(sorted))
	names := make([]string, len(sorted))
	for i, t := range sorted {
		lead := "- " + t.Name + ": "
		purpose, ok := toolPurposes[t.Name]
		if !ok {
			// Each tool's share of the budget, less the words around the purpose.
			share := -1
			if budget > 0 {
				share = max(4*budget/len(sorted)-len(lead), 0)
			}
			purpose = derivedPurpose(t.Description, share)
		}
		lines[i] = lead + purpose
		names[i] = "- " + t.Name
	}
	full := strings.Join(lines, "\n")
	if budget <= 0 || len(full)/4 <= budget {
		return full, nil
	}
	only := strings.Join(names, "\n")
	if len(only)/4 <= budget {
		return only, nil
	}
	return "", &CatalogueTooBigError{Need: len(only) / 4, Budget: budget}
}

// derivedPurpose is the first sentence of a description (up to the first
// sentence end), at most limit bytes when limit is not negative, cut at a rune
// boundary.
func derivedPurpose(desc string, limit int) string {
	s := strings.Join(strings.Fields(desc), " ")
	for i := 0; i < len(s); i++ {
		if (s[i] == '.' || s[i] == '!' || s[i] == '?') && (i+1 == len(s) || s[i+1] == ' ') {
			s = s[:i+1]
			break
		}
	}
	if limit >= 0 && len(s) > limit {
		s = s[:limit]
		for len(s) > 0 && !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
		s = strings.TrimRight(s, " ")
	}
	return s
}
