// internal/okf/retired.go
package okf

import (
	"sort"
	"strconv"
	"strings"
)

// retiredFile is the single index of withdrawn knowledge.
const retiredFile = viewsRoot + "/retired.md"

// renderRetired renders the index of facts the knowledge base has withdrawn.
//
// This is the ONLY place a retired fact appears in a bundle. It is deliberately
// an index and never a set of concept documents: OKF consumers may ignore
// optional frontmatter, so a `status: deprecated` document would be ingested as
// an ordinary claim — re-asserting exactly what the knowledge base disavowed.
// An index entry names the fact and its fate without restating the claim; the
// original text stays reachable through the git history the bundle ships in.
//
// Nothing here writes back to a live document: a superseded fact's successor is
// linked FORWARD from this page only, so the live corpus carries no trace of
// what it replaced.
//
// An empty list produces no document rather than an empty one.
func renderRetired(retired []Retirement, opts RenderOpts) []byte {
	if len(retired) == 0 {
		return nil
	}

	// Copy before sorting: the caller's slice is not ours to reorder.
	entries := make([]Retirement, len(retired))
	copy(entries, retired)
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if !a.Date.Equal(b.Date) {
			return a.Date.After(b.Date) // newest first
		}
		if a.Title != b.Title {
			return a.Title < b.Title
		}
		return a.Path < b.Path
	})

	// A successor that is itself retired has no document to link, but its title
	// is right here in the list — so it can still be named. Built before
	// rendering because a successor may appear later in the ordering.
	titleOf := make(map[string]string, len(entries))
	for _, e := range entries {
		titleOf[e.Path] = e.Title
	}

	superseded, retracted := 0, 0
	for _, e := range entries {
		if e.Kind == RetiredSuperseded {
			superseded++
		} else {
			retracted++
		}
	}

	// Two-level grouping, identical in shape to the digests: months in the jump
	// bar, days as subsections, so every view under views/ reads the same way.
	var months []string
	monthDays := map[string][]string{} // month -> ordered days
	byDay := map[string][]Retirement{} // day -> entries
	monthCount := map[string]int{}     // month -> retirement count
	for _, e := range entries {
		day := e.Date.UTC().Format("2006-01-02")
		month := day[:7]
		if _, seen := monthCount[month]; !seen {
			months = append(months, month)
		}
		if _, seen := byDay[day]; !seen {
			monthDays[month] = append(monthDays[month], day)
		}
		monthCount[month]++
		byDay[day] = append(byDay[day], e)
	}

	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("type: Retired Knowledge\n")
	b.WriteString("title: " + yamlScalar("Retired") + "\n")
	b.WriteString("knomit_view: retired\n")
	b.WriteString("knomit_member_count: " + strconv.Itoa(len(entries)) + "\n")
	b.WriteString("---\n\n")
	b.WriteString("# Retired\n\n")
	b.WriteString(pluralFacts(len(entries)) + " " + haveOrHas(len(entries)) +
		" been withdrawn — " + strconv.Itoa(superseded) + " superseded by a better statement, " +
		strconv.Itoa(retracted) + " retracted outright. " +
		"Listed for the record; their claims are no longer asserted.\n\n")

	jump := make([]string, 0, len(months))
	for _, m := range months {
		jump = append(jump, "["+m+"](#"+anchorFor(m)+") ("+strconv.Itoa(monthCount[m])+")")
	}
	b.WriteString("**Months:** " + strings.Join(jump, " · ") + "\n")

	for _, m := range months {
		b.WriteString("\n## " + m + "\n")
		for _, day := range monthDays[m] {
			b.WriteString("\n### " + day + "\n\n")
			for _, e := range byDay[day] {
				b.WriteString("- **" + retiredKind(e.Kind) + "** " + escapeLinkText(e.Title))
				if e.Kind == RetiredSuperseded {
					b.WriteString(successorSuffix(e.SuccessorPath, titleOf, opts))
				}
				b.WriteString("\n")
			}
		}
	}
	return []byte(b.String())
}

// successorSuffix renders " → <replacement>" for a superseded fact: a link when
// the replacement is a live document in this bundle, its bare title when the
// replacement was itself retired, and nothing at all when neither is known.
// A retired fact's replacement is never invented — an unresolvable successor
// yields no arrow rather than a broken link or a raw path.
func successorSuffix(successorPath string, titleOf map[string]string, opts RenderOpts) string {
	if strings.TrimSpace(successorPath) == "" {
		return ""
	}
	if opts.ResolveFact != nil {
		if target, ok := opts.ResolveFact(successorPath); ok {
			return " → [" + escapeLinkText(target.Title) + "](" + relLink(viewsRoot, target.Path) + ")"
		}
	}
	if title := strings.TrimSpace(titleOf[successorPath]); title != "" {
		return " → " + escapeLinkText(title)
	}
	return ""
}

// retiredKind normalizes a retirement kind for display. Anything not explicitly
// marked superseded is reported as retracted: claiming a replacement exists
// when the commit did not name one would be a fabrication, while the reverse
// only understates.
func retiredKind(kind string) string {
	if kind == RetiredSuperseded {
		return RetiredSuperseded
	}
	return RetiredRetracted
}

func haveOrHas(n int) string {
	if n == 1 {
		return "has"
	}
	return "have"
}
