// internal/okf/log.go
package okf

import (
	"path"
	"sort"
	"strconv"
	"strings"
)

// RenderLog renders the reserved log.md, or nil when there is nothing to
// report — an empty changelog is not a document, and every other view in this
// package already declines to write one (see renderRetired, buildDigests).
//
// Two rules shape what it contains.
//
// Updates are filtered by MeaningfulRevisions, the same rule the per-fact
// `# History` section uses. An Update whose Delta is empty changed nothing this
// mapper tracks — a retag, an origin change — and rendered as a bare
// "**Update** <title>" it asserts that something happened while being unable to
// say what. On a real 1208-fact corpus that was ~160 of 322 Update rows, with
// another 79 reporting only that a ref COUNT moved.
//
// Creations collapse to one row per day carrying their count. They are the
// dominant event in any growing knowledge base — 1456 of 1778 rows on that same
// corpus, 172 KB of a 177 KB file — and naming every one of them buries the
// revisions a reader came for. The count keeps the fact that the base grew, and
// by how much, which is the part a changelog must not lose; WHICH facts arrived
// is what the directory indexes and each document's own `generated.at` are for.
//
// Structure follows the spec (§9): a FLAT list of date-grouped entries, newest
// first, with headings in ISO 8601 YYYY-MM-DD form. The month index at the top
// is a jump bar of links rather than a second heading level — grouping by month
// the way views/ does would read better but would break both of those rules.
func RenderLog(entries []LogEntry) []byte {
	var kept []LogEntry
	for _, e := range entries {
		if e.Kind == "Creation" || e.Delta != "" {
			kept = append(kept, e)
		}
	}
	if len(kept) == 0 {
		return nil
	}

	sort.SliceStable(kept, func(i, j int) bool {
		a, b := kept[i], kept[j]
		if !a.Date.Equal(b.Date) {
			return a.Date.After(b.Date) // newest first
		}
		if a.Kind != b.Kind {
			return kindRank(a.Kind) < kindRank(b.Kind)
		}
		if a.Title != b.Title {
			return a.Title < b.Title
		}
		return a.Path < b.Path
	})

	// Group by day, preserving the newest-first order the sort established.
	var days []string
	seen := map[string]bool{}
	created := map[string]int{}        // day -> facts added
	updates := map[string][]LogEntry{} // day -> revisions and withdrawals worth naming
	for _, e := range kept {
		day := e.Date.UTC().Format("2006-01-02")
		if !seen[day] {
			seen[day] = true
			days = append(days, day)
		}
		if e.Kind == "Creation" {
			created[day]++
			continue
		}
		updates[day] = append(updates[day], e)
	}

	var b strings.Builder
	b.WriteString("# Log\n\n")
	jump := logJumpBar(days, created, updates)
	b.WriteString(jump)

	for i, day := range days {
		// A blank line before every heading except a first one that has no jump
		// bar above it to be separated from.
		if i > 0 || jump != "" {
			b.WriteString("\n")
		}
		b.WriteString("## " + day + "\n\n")
		if n := created[day]; n > 0 {
			b.WriteString("- **Creation** " + pluralFacts(n) + " added\n")
		}
		for _, e := range updates[day] {
			title := e.Title
			if title == "" {
				title = e.Path
			}
			b.WriteString("- **" + e.Kind + "** " + title + " — " + e.Delta + "\n")
		}
	}
	return []byte(b.String())
}

// partitionLog splits the changelog across the logs the spec allows (§9: a
// log.md MAY appear at any level, recording changes to that scope). Every event
// lands in exactly one log — no event is told twice.
//
// A revision or a withdrawal belongs to the folder holding the fact it happened
// to, when that folder still exists. Scope is DIRECT CHILDREN, the same rule
// index.md follows: rolling a subtree up would repeat one fact's history in
// every directory above it, five deep on a real ontology. When the folder is
// gone — its last fact retired — the event has nowhere local to live and falls
// to the root, which is the only scope that still contains it.
//
// Creations are the exception, and always go to the root. They are 93% of all
// events, so partitioning them empties the root log of everything except the
// deleted corner of the knowledge base: measured on a 1208-fact corpus, the root
// would have kept 215 rows describing only facts whose folder no longer exists,
// while the 1362 facts actually added scattered into 922 folder files, 747 of
// which would say nothing but "1 fact added" — something the folder's own
// index.md and each document's `generated.at` already say. Held at the root they
// stay one answer to "what changed here lately", and folder logs exist only
// where a belief actually moved.
func partitionLog(entries []LogEntry, liveDirs map[string]bool) (root []LogEntry, byDir map[string][]LogEntry) {
	byDir = map[string][]LogEntry{}
	for _, e := range entries {
		dir := path.Dir(e.Path)
		if e.Kind == "Creation" || !liveDirs[dir] {
			root = append(root, e)
			continue
		}
		byDir[dir] = append(byDir[dir], e)
	}
	return root, byDir
}

// retirementLogEntries turns withdrawals into changelog rows.
//
// §9 names **Deprecation** as one of its three conventional labels, and until
// now the log emitted none: a knowledge base that reported only what it added
// and edited was describing half of what happened to it. On a real corpus that
// was 247 withdrawals, recorded in views/retired.md and nowhere in the log.
func retirementLogEntries(retired []Retirement, opts RenderOpts) []LogEntry {
	titleOf := make(map[string]string, len(retired))
	for _, r := range retired {
		titleOf[r.Path] = r.Title
	}
	out := make([]LogEntry, 0, len(retired))
	for _, r := range retired {
		out = append(out, LogEntry{
			Date:  r.Date,
			Kind:  "Deprecation",
			Title: r.Title,
			Path:  r.Path,
			Delta: retirementDelta(r, titleOf, opts),
		})
	}
	return out
}

// retirementDelta says how a fact left. A successor is NAMED but never linked:
// the same row is rendered into the root log and into a folder log at a
// different depth, and a relative link correct in one would be broken in the
// other. views/retired.md is the view that links.
func retirementDelta(r Retirement, titleOf map[string]string, opts RenderOpts) string {
	if r.Kind != RetiredSuperseded {
		return RetiredRetracted
	}
	if t := successorTitle(r.SuccessorPath, titleOf, opts); t != "" {
		return RetiredSuperseded + " by " + t
	}
	return RetiredSuperseded
}

// successorTitle names a replacement: the live document's title when it is still
// in the bundle, otherwise the withdrawn one's own recorded title.
func successorTitle(successorPath string, titleOf map[string]string, opts RenderOpts) string {
	if strings.TrimSpace(successorPath) == "" {
		return ""
	}
	if opts.ResolveFact != nil {
		if target, ok := opts.ResolveFact(successorPath); ok && target.Title != "" {
			return target.Title
		}
	}
	return strings.TrimSpace(titleOf[successorPath])
}

// kindRank orders the three labels within a day as the change itself reads:
// what arrived, what moved, what left. Alphabetical would file a withdrawal
// between the two.
func kindRank(kind string) int {
	switch kind {
	case "Creation":
		return 0
	case "Update":
		return 1
	default:
		return 2 // Deprecation
	}
}

// logJumpBar indexes the log by month, linking each to the first day it holds.
// A month heading would be the natural way to do this — it is what every view
// under views/ does — but §9 requires a flat list with YYYY-MM-DD headings, so
// the index has to be links rather than structure.
//
// It is omitted for a log spanning one month, where it would only repeat the
// single heading below it.
func logJumpBar(days []string, created map[string]int, updates map[string][]LogEntry) string {
	var months []string
	firstDay := map[string]string{}
	count := map[string]int{}
	for _, day := range days {
		m := day[:7]
		if _, seen := firstDay[m]; !seen {
			months = append(months, m)
			firstDay[m] = day
		}
		count[m] += created[day] + len(updates[day])
	}
	if len(months) < 2 {
		return ""
	}
	parts := make([]string, 0, len(months))
	for _, m := range months {
		parts = append(parts, "["+m+"](#"+firstDay[m]+") ("+strconv.Itoa(count[m])+")")
	}
	return "**Months:** " + strings.Join(parts, " · ") + "\n"
}
