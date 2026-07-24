// internal/okf/log.go
package okf

import (
	"sort"
	"strings"
)

// RenderLog renders the reserved log.md. Entries are grouped by ISO date,
// newest date first; within a date, Creation before Update, then by Path.
func RenderLog(entries []LogEntry) []byte {
	sorted := make([]LogEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if !a.Date.Equal(b.Date) {
			return a.Date.After(b.Date) // newest first
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind // "Creation" < "Update"
		}
		return a.Path < b.Path
	})

	var b strings.Builder
	b.WriteString("# Log\n\n")
	if len(sorted) == 0 {
		b.WriteString("No changes recorded.\n")
		return []byte(b.String())
	}

	curDate := ""
	for _, e := range sorted {
		d := e.Date.UTC().Format("2006-01-02")
		if d != curDate {
			if curDate != "" {
				b.WriteString("\n")
			}
			b.WriteString("## " + d + "\n\n")
			curDate = d
		}
		title := e.Title
		if title == "" {
			title = e.Path
		}
		b.WriteString("- **" + e.Kind + "** " + title + "\n")
	}
	return []byte(b.String())
}
