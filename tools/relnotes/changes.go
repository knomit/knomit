package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// PR is one merged pull request in the range. Body carries the PR
// description; RenderForDistill renders it beneath each bullet so `distill`
// sees the rationale behind a change, not just its subject line.
// RenderChanges — the changelog readers actually see — never touches it.
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

// appcastFenceBegin/End mirror the markers tools/appcast splits a release
// body on. The stable workflow's no-distill path places this changelog
// INSIDE that fence, so a PR titled or committed with either marker literally
// would truncate — or reopen — the fenced region in the published feed.
const (
	appcastFenceBegin = "<!-- appcast:begin -->"
	appcastFenceEnd   = "<!-- appcast:end -->"
)

func stripFenceMarkers(s string) string {
	s = strings.ReplaceAll(s, appcastFenceBegin, "")
	s = strings.ReplaceAll(s, appcastFenceEnd, "")
	return s
}

func RenderChanges(c Changes) string {
	return renderChanges(c, false)
}

// RenderForDistill renders the same grouped changelog as RenderChanges, with
// each PR's body appended beneath its bullet. It exists only to be piped into
// `distill`: the extra context is what lets the model describe *why* a change
// matters instead of paraphrasing a title.
func RenderForDistill(c Changes) string {
	return renderChanges(c, true)
}

func renderChanges(c Changes, withBodies bool) string {
	grouped := map[string][]string{}
	for _, pr := range c.PRs {
		typ, subject := splitConventional(stripFenceMarkers(pr.Title))
		h := sectionFor(typ)
		line := fmt.Sprintf("- %s (#%d)", subject, pr.Number)
		if withBodies {
			if body := strings.TrimSpace(stripFenceMarkers(pr.Body)); body != "" {
				line += "\n\n  " + body + "\n"
			}
		}
		grouped[h] = append(grouped[h], line)
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
			fmt.Fprintf(&b, "- %s (`%s`)\n", stripFenceMarkers(cm.Subject), cm.SHA)
		}
		b.WriteString("\n")
	}

	return b.String()
}

// unitSep separates fields in git's --pretty output. A literal 0x1f cannot
// appear in a commit subject, unlike any punctuation we might otherwise pick.
const unitSep = "\x1f"

type runner func(name string, args ...string) (string, error)

type prFetcher interface {
	Fetch(number int) (PR, error)
}

func execRunner(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err,
			strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

type ghFetcher struct{ run runner }

func (g ghFetcher) Fetch(n int) (PR, error) {
	out, err := g.run("gh", "pr", "view", strconv.Itoa(n), "--json", "title,body",
		"-q", ".title+\"\\u001f\"+.body")
	if err != nil {
		return PR{}, err
	}
	title, body, _ := strings.Cut(strings.TrimRight(out, "\n"), unitSep)
	return PR{Number: n, Title: title, Body: body}, nil
}

func splitLines(out string) []string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// Collect resolves the range into PRs plus the commits no PR accounts for.
func Collect(run runner, fetch prFetcher, from, to string) (Changes, error) {
	rangeSpec := from + ".." + to

	merges, err := run("git", "log", "--merges", "--reverse",
		"--pretty=format:%h"+unitSep+"%s", rangeSpec)
	if err != nil {
		return Changes{}, err
	}

	var out Changes
	covered := map[string]bool{}
	for _, line := range splitLines(merges) {
		sha, subject, _ := strings.Cut(line, unitSep)
		n, ok := parseMergeSubject(subject)
		if !ok {
			continue // branch sync, not a pull request
		}

		pr, ferr := fetch.Fetch(n)
		if ferr != nil {
			// Best effort by design: a PR record we cannot read is a worse
			// changelog entry, not a failed release. Fall back to the branch
			// name the merge subject carries.
			fields := strings.Fields(subject)
			title := fmt.Sprintf("#%d", n)
			if len(fields) > 0 {
				title = fields[len(fields)-1]
			}
			pr = PR{Number: n, Title: title}
			fmt.Fprintf(os.Stderr, "relnotes: PR #%d unreadable (%v), using %q\n", n, ferr, title)
		}
		out.PRs = append(out.PRs, pr)

		// The commits this merge brought in are first-parent-exclusive:
		// everything reachable from the merged head but not from the base.
		owned, oerr := run("git", "log", "--no-merges", "--pretty=format:%h",
			sha+"^.."+sha+"^2")
		if oerr != nil {
			return Changes{}, oerr
		}
		for _, c := range splitLines(owned) {
			covered[strings.TrimSpace(c)] = true
		}
	}

	all, err := run("git", "log", "--no-merges", "--reverse",
		"--pretty=format:%h"+unitSep+"%s", rangeSpec)
	if err != nil {
		return Changes{}, err
	}
	for _, line := range splitLines(all) {
		sha, subject, _ := strings.Cut(line, unitSep)
		if covered[sha] {
			continue
		}
		out.Direct = append(out.Direct, Commit{SHA: sha, Subject: subject})
	}

	return out, nil
}

func runChanges(args []string) error {
	fs := flag.NewFlagSet("changes", flag.ExitOnError)
	from := fs.String("from", "", "range start revision (exclusive)")
	to := fs.String("to", "HEAD", "range end revision (inclusive)")
	bodiesOut := fs.String("bodies-out", "",
		"also write a body-enriched changelog to this path, for piping into `distill`")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *from == "" {
		return fmt.Errorf("changes needs -from")
	}
	c, err := Collect(execRunner, ghFetcher{run: execRunner}, *from, *to)
	if err != nil {
		return err
	}
	// Written from the same Collect result, not a second `gh` pass: PR bodies
	// are already in memory, and re-fetching them would double the API calls
	// this command makes for output nobody but `distill` reads.
	if *bodiesOut != "" {
		if err := os.WriteFile(*bodiesOut, []byte(RenderForDistill(c)), 0o644); err != nil {
			return err
		}
	}
	fmt.Print(RenderChanges(c))
	return nil
}
