package main

import (
	"strings"
	"testing"
)

// A branch-sync merge is a merge commit but NOT a pull request. The measured
// dev-latest range had 10 merge commits and only 9 PRs; listing the sync as a
// changelog entry is exactly the noise this tool exists to remove.
func TestParseMergeSubject(t *testing.T) {
	for _, tc := range []struct {
		subject string
		want    int
		ok      bool
	}{
		{"Merge pull request #68 from knomit/feat/kb-repo-layout", 68, true},
		{"Merge remote-tracking branch 'origin/dev' into feat/desktop-codesigning", 0, false},
		{"Merge branch 'dev'", 0, false},
		{"feat(web): add a thing", 0, false},
		{"Merge pull request #7", 0, false}, // no trailing space: not the gh format
	} {
		got, ok := parseMergeSubject(tc.subject)
		if got != tc.want || ok != tc.ok {
			t.Errorf("parseMergeSubject(%q) = (%d, %v), want (%d, %v)",
				tc.subject, got, ok, tc.want, tc.ok)
		}
	}
}

func TestSplitConventional(t *testing.T) {
	for _, tc := range []struct{ in, typ, subject string }{
		{"feat(update): add appcast tool", "feat", "add appcast tool"},
		{"fix: correct the thing", "fix", "correct the thing"},
		{"perf(review): build a page lazily", "perf", "build a page lazily"},
		{"bump version", "", "bump version"},
		{"WIP: no colon type here", "", "WIP: no colon type here"},
	} {
		typ, subject := splitConventional(tc.in)
		if typ != tc.typ || subject != tc.subject {
			t.Errorf("splitConventional(%q) = (%q, %q), want (%q, %q)",
				tc.in, typ, subject, tc.typ, tc.subject)
		}
	}
}

func TestRenderChangesGroupsAndKeepsDirectCommits(t *testing.T) {
	out := RenderChanges(Changes{
		PRs: []PR{
			{Number: 48, Title: "feat(update): desktop self-update"},
			{Number: 51, Title: "fix(desktop): ad-hoc sign the bundle"},
			{Number: 60, Title: "chore(deps): bump things"},
		},
		Direct: []Commit{{SHA: "0abeb089", Subject: "bump version"}},
	})
	for _, want := range []string{
		"### Features",
		"- desktop self-update (#48)",
		"### Fixes",
		"- ad-hoc sign the bundle (#51)",
		"### Other",
		"- bump things (#60)",
		"### Direct commits",
		"- bump version (`0abeb089`)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q\n---\n%s", want, out)
		}
	}
}

// An empty section must not render its heading — a changelog with a bare
// "### Fixes" and nothing under it reads as a rendering bug.
func TestRenderChangesOmitsEmptySections(t *testing.T) {
	out := RenderChanges(Changes{PRs: []PR{{Number: 1, Title: "feat: only a feature"}}})
	if strings.Contains(out, "### Fixes") || strings.Contains(out, "### Direct commits") {
		t.Errorf("empty sections rendered\n---\n%s", out)
	}
}
