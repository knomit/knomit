package okf

import (
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
	title    string
	blurb    string
}

var digestSpecs = []digestSpec{
	{
		leafType: "synthesis",
		file:     viewsRoot + "/synthesis.md",
		okfType:  "Synthesis Digest",
		title:    "Synthesis",
		blurb:    "Higher-order facts distilled from clusters of other facts, newest first.",
	},
	{
		leafType: "hypothesis",
		file:     viewsRoot + "/hypotheses.md",
		okfType:  "Hypothesis Digest",
		title:    "Hypotheses",
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

// buildDigests renders the single-file chronological views. A type with no
// facts produces no page rather than an empty one.
func buildDigests(facts []FactInput, pathOf map[string]string) map[string][]byte {
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
	for _, spec := range digestSpecs {
		entries := byType[spec.leafType]
		if len(entries) == 0 {
			continue
		}
		files[spec.file] = renderDigest(spec, entries)
	}
	return files
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

	// Group by ISO day, preserving the newest-first order established above.
	var days []string
	byDay := map[string][]digestEntry{}
	for _, e := range entries {
		d := e.date.UTC().Format("2006-01-02")
		if _, seen := byDay[d]; !seen {
			days = append(days, d)
		}
		byDay[d] = append(byDay[d], e)
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

	jump := make([]string, 0, len(days))
	for _, d := range days {
		jump = append(jump, "["+d+"](#"+anchorFor(d)+")")
	}
	b.WriteString("**Jump to:** " + strings.Join(jump, " · ") + "\n")

	for _, d := range days {
		b.WriteString("\n## " + d + "\n\n")
		for _, e := range byDay[d] {
			b.WriteString("- [" + escapeLinkText(e.title) + "](" +
				relLink(viewsRoot, e.bundlePath) + ") — " + e.typ + "\n")
		}
	}
	return []byte(b.String())
}
