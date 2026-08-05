package store

import "strings"

// defaultOntologyRoot mirrors config.Defaults().OntologyRoot. The store cannot
// import internal/config (config is the outer layer), so the default lives here
// and SetOntologyRoot carries the configured value in from the repos builder.
const defaultOntologyRoot = "kb"

// ontologyRoot returns the root every fact path lives under, defaulting for a
// bare repoHandler that was never configured (tests, ad-hoc tooling).
func (rh *repoHandler) ontologyRoot() string {
	if rh.factRoot == "" {
		return defaultOntologyRoot
	}
	return rh.factRoot
}

// isFactPath reports whether path is eligible for the fact index: a .md file
// inside the ontology root.
//
// Membership is decided by LOCATION, never by whether the bytes happen to
// parse. Those are not the same test: fact.ParseFact accepts any file that
// opens with YAML frontmatter and an H1 heading, which is the ordinary shape of
// a markdown document — so a parse-based rule hands index membership to whoever
// authored the file. That became live when the repo description (README.md, at
// the tree root) turned into a user-editable field: a manifest written with
// frontmatter would parse, index as a fact, and then be reported by Verify as a
// ghost branch_facts row, because checkFactsCoherence builds its expected set
// from the ontology root alone. Location is the rule Verify already enforces;
// this is the same rule applied on the way in.
//
// Callers still keep their parse-failure skips: this bounds WHERE a fact can
// live, parsing decides whether a file in that place is a well-formed one.
func (rh *repoHandler) isFactPath(path string) bool {
	return strings.HasPrefix(path, rh.ontologyRoot()+"/") && strings.HasSuffix(path, ".md")
}
