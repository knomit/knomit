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

// BuildNameCatalogue is the catalogue the loader's description carries in an
// adaptive chat: "- name" per tool, sorted. The purposes are not repeated
// there, because every tool that is not loaded is in the tools block as a stub
// that carries its purpose (StubPurposes); the loader keeps the list of valid
// names it takes. budget is the allowance in tokens (bytes/4); 0 or less means no limit. If the
// names do not fit it, the error is a *CatalogueTooBigError and no tool count
// or size is assumed anywhere.
func BuildNameCatalogue(tools []llm.ToolDef, budget int) (string, error) {
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.Name
	}
	sort.Strings(names)
	for i, n := range names {
		names[i] = "- " + n
	}
	only := strings.Join(names, "\n")
	if budget > 0 && len(only)/4 > budget {
		return "", &CatalogueTooBigError{Need: len(only) / 4, Budget: budget}
	}
	return only, nil
}

// stubPurposeLimit caps the derived purpose of a tool with no row in
// toolPurposes (a script tool), in bytes: a stub is meant to be small.
const stubPurposeLimit = 160

// StubPurposes are the one-line purposes the stubs carry, by tool name: the
// approved line of toolPurposes, or for a tool with no row the first sentence
// of its own description, so no new copy is written for a stub.
func StubPurposes(tools []llm.ToolDef) map[string]string {
	out := make(map[string]string, len(tools))
	for _, t := range tools {
		if p, ok := toolPurposes[t.Name]; ok {
			out[t.Name] = p
			continue
		}
		out[t.Name] = derivedPurpose(t.Description, stubPurposeLimit)
	}
	return out
}
