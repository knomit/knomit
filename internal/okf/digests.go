package okf

import (
	"path"
	"sort"
	"strings"
	"time"
)

// digestSpec describes a single-file view over one knomit leaf type.
//
// These are the higher-order facts — derived rather than observed — so
// gathering them into one chronological page makes the knowledge base's own
// reasoning legible in a way the topic tree cannot: synthesis and hypothesis
// facts are scattered across topics by subject, not by their nature.
type digestSpec struct {
	leafType string // knomit leaf type to collect
	file     string // bundle path
	okfType  string // OKF `type` for the generated document
	title    string // the page's own heading
	name     string // lowercase label on views/index.md, matching the directories
	blurb    string
}

var digestSpecs = []digestSpec{
	{
		leafType: "synthesis",
		file:     viewsRoot + "/synthesis.md",
		okfType:  "Synthesis Digest",
		title:    "Synthesis",
		name:     "synthesis",
		blurb:    "Higher-order facts distilled from clusters of other facts, newest first.",
	},
	{
		leafType: "hypothesis",
		file:     viewsRoot + "/hypotheses.md",
		okfType:  "Hypothesis Digest",
		title:    "Hypotheses",
		name:     "hypotheses",
		blurb:    "Falsifiable predictions derived from patterns, newest first. These carry inherent uncertainty — they are not grounded observations.",
	},
}

// digestEntry is one dated fact on a digest page.
type digestEntry struct {
	date       time.Time
	title      string
	typ        string
	bundlePath string
}

// buildDigests renders the single-file chronological views, and returns the
// index entries pointing at them. A type with no facts produces no page rather
// than an empty one.
func buildDigests(facts []FactInput, pathOf map[string]string) (map[string][]byte, []indexEntry) {
	byType := map[string][]digestEntry{}
	for _, fi := range facts {
		lt := string(fi.Fact.Type)
		bp, ok := pathOf[fi.Fact.Path()]
		if !ok {
			continue // skipped fact
		}
		byType[lt] = append(byType[lt], digestEntry{
			date:       fi.Timestamp,
			title:      firstNonEmpty(fi.Fact.Title, lt),
			typ:        okfType(fi.Fact.Path(), lt),
			bundlePath: bp,
		})
	}

	files := map[string][]byte{}
	var entries []indexEntry
	for _, spec := range digestSpecs {
		members := byType[spec.leafType]
		if len(members) == 0 {
			continue
		}
		files[spec.file] = renderDigest(spec, members)
		entries = append(entries, indexEntry{
			name:   spec.name,
			target: path.Base(spec.file),
			note:   pluralFacts(len(members)),
		})
	}
	return files, entries
}

// renderDigest renders one digest page: frontmatter, a jump bar of dates, then
// the facts grouped by day, newest first.
func renderDigest(spec digestSpec, entries []digestEntry) []byte {
	// Newest first; stable within a day by title then path.
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if !a.date.Equal(b.date) {
			return a.date.After(b.date)
		}
		if a.title != b.title {
			return a.title < b.title
		}
		return a.bundlePath < b.bundlePath
	})

	// Two-level grouping: months in the jump bar, days as subsections within.
	// A flat list of every day is a wall of anchors that gets less usable the
	// more the knowledge base grows; months stay a bounded, scannable index
	// (twelve per year) while the day headings preserve the detail.
	var months []string
	monthDays := map[string][]string{}     // month -> ordered days
	byDay := map[string][]digestEntry{}    // day -> entries
	monthCount := map[string]int{}         // month -> fact count
	for _, e := range entries {
		day := e.date.UTC().Format("2006-01-02")
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
	b.WriteString("type: " + spec.okfType + "\n")
	b.WriteString("title: " + yamlScalar(spec.title) + "\n")
	b.WriteString("knomit_view: " + spec.leafType + "\n")
	b.WriteString("knomit_member_count: " + itoa(len(entries)) + "\n")
	b.WriteString("---\n\n")
	b.WriteString("# " + spec.title + "\n\n")
	b.WriteString(pluralFacts(len(entries)) + ". " + spec.blurb + "\n\n")

	jump := make([]string, 0, len(months))
	for _, m := range months {
		jump = append(jump, "["+m+"](#"+anchorFor(m)+") ("+itoa(monthCount[m])+")")
	}
	b.WriteString("**Months:** " + strings.Join(jump, " · ") + "\n")

	for _, m := range months {
		b.WriteString("\n## " + m + "\n")
		for _, day := range monthDays[m] {
			b.WriteString("\n### " + day + "\n\n")
			for _, e := range byDay[day] {
				b.WriteString("- [" + escapeLinkText(e.title) + "](" +
					relLink(viewsRoot, e.bundlePath) + ") — " + e.typ + "\n")
			}
		}
	}
	return []byte(b.String())
}
