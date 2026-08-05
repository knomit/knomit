package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// PR is one merged pull request in the range. Body carries the PR description,
// which is what gives `distill` the rationale behind a change rather than just
// its subject line.
type PR struct {
	Number int
	Title  string
	Body   string
}

// Commit is a non-merge commit reachable from no PR merge — work pushed
// straight to the branch. Listing these separately is not a nicety: a
// changelog that silently drops commits is worse than a verbose one.
type Commit struct {
	SHA     string
	Subject string
}

type Changes struct {
	PRs    []PR
	Direct []Commit
}

// mergePRRe matches ONLY the merge subject `gh`/GitHub writes for a pull
// request. Matching bare `--merges` would also pick up branch syncs
// ("Merge remote-tracking branch 'origin/dev' into ..."), which are not
// changelog entries. The trailing space is load-bearing: it anchors the digit
// run so `#7` cannot swallow a longer number.
var mergePRRe = regexp.MustCompile(`^Merge pull request #(\d+) `)

func parseMergeSubject(subject string) (int, bool) {
	m := mergePRRe.FindStringSubmatch(subject)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

// conventionalRe splits `type(scope): subject`. The scope is optional and
// discarded — it duplicates information the grouping heading already carries.
var conventionalRe = regexp.MustCompile(`^([a-z]+)(?:\([^)]*\))?: (.+)$`)

func splitConventional(title string) (string, string) {
	m := conventionalRe.FindStringSubmatch(title)
	if m == nil {
		return "", title
	}
	return m[1], m[2]
}

// sections is ordered: users care about features and fixes first, and
// everything unrecognised lands in Other rather than inventing a heading.
var sections = []struct {
	heading string
	types   []string
}{
	{"Features", []string{"feat"}},
	{"Fixes", []string{"fix"}},
	{"Performance", []string{"perf"}},
	{"Docs", []string{"docs"}},
}

func sectionFor(typ string) string {
	for _, s := range sections {
		for _, t := range s.types {
			if t == typ {
				return s.heading
			}
		}
	}
	return "Other"
}

func RenderChanges(c Changes) string {
	grouped := map[string][]string{}
	for _, pr := range c.PRs {
		typ, subject := splitConventional(pr.Title)
		h := sectionFor(typ)
		grouped[h] = append(grouped[h], fmt.Sprintf("- %s (#%d)", subject, pr.Number))
	}

	var b strings.Builder
	order := make([]string, 0, len(sections)+1)
	for _, s := range sections {
		order = append(order, s.heading)
	}
	order = append(order, "Other")

	for _, h := range order {
		lines := grouped[h]
		if len(lines) == 0 {
			continue
		}
		fmt.Fprintf(&b, "### %s\n\n", h)
		for _, l := range lines {
			fmt.Fprintln(&b, l)
		}
		b.WriteString("\n")
	}

	if len(c.Direct) > 0 {
		b.WriteString("### Direct commits\n\n")
		for _, cm := range c.Direct {
			fmt.Fprintf(&b, "- %s (`%s`)\n", cm.Subject, cm.SHA)
		}
		b.WriteString("\n")
	}

	return b.String()
}
