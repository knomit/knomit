package knomitapi

import (
	"fmt"
	"strings"
)

const (
	// maxGlobals bounds the PROJECT PRINCIPLES block.
	maxGlobals = 7
	// maxInvariants bounds the rollout fallback block.
	maxInvariants = 5
	// maxRecent is how many "Recent work" lines are shown.
	maxRecent = 5
	// recentFetch is how many recent facts are fetched to fill maxRecent after
	// removing any that already appear in the principles or invariants block.
	recentFetch = 20
)

// Stats reports what SessionContext put in the block, for the caller's log line.
type Stats struct {
	Globals            int
	InvariantsFallback int
	Recent             int
	SkipReason         string
}

// SessionContext builds the corpus-context block for repo@branch. Returns
// ("", Stats{SkipReason: "no_facts"}) when there is nothing worth saying, so
// callers can stay silent rather than emit an empty header.
//
// Principles and invariants are fetched with targeted server-side queries, NOT
// by filtering a recent-N page. Principles are written rarely and then sit
// still, so filtering a recent window drops nearly all of them — the block used
// to under-report by an order of magnitude on a normal corpus.
func SessionContext(repo, branch string) (string, Stats) {
	globals := FilterGlobalPrinciples(FetchFacts(GlobalPrinciplesURL(repo, branch, maxGlobals)), maxGlobals)

	// Rollout fallback: until designers seed global principles, surface
	// load-bearing invariants so agents don't regress to zero context. Once any
	// global principle exists this goes dark.
	var invariantsFallback []FactSummary
	if len(globals) == 0 {
		invariantsFallback = FilterByPathPrefix(
			FetchFacts(InvariantFactsURL(repo, branch, maxInvariants)), "kb/invariants/", maxInvariants)
	}

	// Recent work, minus anything already printed above. A principle that is
	// also among the most recent facts would otherwise be listed twice in the
	// same block — which happens exactly after a burst of principle-writing,
	// when the block matters most.
	shown := make(map[string]bool, len(globals)+len(invariantsFallback))
	for _, f := range globals {
		shown[f.Path] = true
	}
	for _, f := range invariantsFallback {
		shown[f.Path] = true
	}
	recent := make([]FactSummary, 0, maxRecent)
	for _, f := range FetchFacts(RecentFactsURL(repo, branch, recentFetch)) {
		if shown[f.Path] {
			continue
		}
		recent = append(recent, f)
		if len(recent) >= maxRecent {
			break
		}
	}

	if len(globals) == 0 && len(invariantsFallback) == 0 && len(recent) == 0 {
		return "", Stats{SkipReason: "no_facts"}
	}

	var sb strings.Builder
	sb.WriteString("Known facts from knomit for this codebase:\n\n")
	if len(globals) > 0 {
		sb.WriteString("PROJECT PRINCIPLES:\n")
		for _, f := range globals {
			fmt.Fprintf(&sb, "  • %s: %s\n", PrincipleShortPath(f.Path), f.Title)
		}
		sb.WriteString("\n")
	} else if len(invariantsFallback) > 0 {
		sb.WriteString("LOAD-BEARING INVARIANTS:\n")
		for _, f := range invariantsFallback {
			fmt.Fprintf(&sb, "  - %s\n    %s\n", f.Title, f.Path)
		}
		sb.WriteString("\n")
	}
	if len(recent) > 0 {
		sb.WriteString("Recent work in this repo:\n")
		for _, f := range recent {
			fmt.Fprintf(&sb, "  - %s: %s\n", f.Path, f.Title)
		}
	}
	return sb.String(), Stats{
		Globals:            len(globals),
		InvariantsFallback: len(invariantsFallback),
		Recent:             len(recent),
	}
}
