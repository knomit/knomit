package knomitapi

import (
	"fmt"
	"sort"
	"strings"
)

const (
	// globalsPageSize is the page size for the global-principles fetch. It
	// equals the /facts handler's maximum limit (500), so one request covers
	// any realistic corpus and paging is the exception.
	globalsPageSize = 500
	// globalsMaxPages is a runaway guard on the paging loop, not a cap anyone
	// should reach: 20 pages is 10,000 global principles.
	globalsMaxPages = 20
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

// fetchGlobalPrinciples returns EVERY global designer principle, ordered by
// path. Global principles are designer-authored and rare by construction, so
// the block is deliberately uncapped: a principle written today must load in
// every later session, not only until N newer writes push it out.
//
// The server orders pages by committed_at DESC and offers no sort parameter,
// so the order is fixed here by path: the block is then stable across sessions
// and does not reshuffle each time any principle is written or edited.
func fetchGlobalPrinciples(repo, branch string) []FactSummary {
	var all []FactSummary
	for page := 0; page < globalsMaxPages; page++ {
		got := FetchFacts(GlobalPrinciplesURL(repo, branch, globalsPageSize, page*globalsPageSize))
		all = append(all, got...)
		if len(got) < globalsPageSize {
			break
		}
	}
	globals := FilterGlobalPrinciples(all)
	sort.SliceStable(globals, func(i, j int) bool { return globals[i].Path < globals[j].Path })
	return globals
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
	globals := fetchGlobalPrinciples(repo, branch)

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
			fmt.Fprintf(&sb, "  • %s: %s%s\n", PrincipleShortPath(f.Path), f.Title, ExpiryNote(f))
		}
		sb.WriteString("\n")
	} else if len(invariantsFallback) > 0 {
		sb.WriteString("LOAD-BEARING INVARIANTS:\n")
		for _, f := range invariantsFallback {
			fmt.Fprintf(&sb, "  - %s%s\n    %s\n", f.Title, ExpiryNote(f), f.Path)
		}
		sb.WriteString("\n")
	}
	if len(recent) > 0 {
		sb.WriteString("Recent work in this repo:\n")
		for _, f := range recent {
			fmt.Fprintf(&sb, "  - %s: %s%s\n", f.Path, f.Title, ExpiryNote(f))
		}
	}
	return sb.String(), Stats{
		Globals:            len(globals),
		InvariantsFallback: len(invariantsFallback),
		Recent:             len(recent),
	}
}
