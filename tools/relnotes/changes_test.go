package main

import (
	"fmt"
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

// RenderForDistill is the only renderer that owes `distill` the rationale a
// title alone cannot carry; RenderChanges — what ships in the release notes —
// must stay title-only.
func TestRenderForDistillIncludesBodies(t *testing.T) {
	c := Changes{PRs: []PR{
		{Number: 48, Title: "feat(update): desktop self-update", Body: "Adds a Sparkle-compatible feed."},
		{Number: 60, Title: "chore(deps): bump things", Body: ""},
	}}
	out := RenderForDistill(c)
	for _, want := range []string{
		"- desktop self-update (#48)",
		"Adds a Sparkle-compatible feed.",
		"- bump things (#60)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("RenderForDistill missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(RenderChanges(c), "Adds a Sparkle-compatible feed.") {
		t.Error("RenderChanges leaked a PR body — it must stay title-only")
	}
}

// PR bodies frequently render fine with a trailing blank body ("") after
// trimming; that must not print an empty continuation.
func TestRenderForDistillSkipsBlankBodies(t *testing.T) {
	out := RenderForDistill(Changes{PRs: []PR{
		{Number: 1, Title: "fix: a thing", Body: "   \n  "},
	}})
	if !strings.Contains(out, "- a thing (#1)") {
		t.Errorf("missing bullet\n---\n%s", out)
	}
}

// A merged PR titled with a literal appcast fence marker would truncate — or
// reopen — the fenced region the stable workflow places this changelog
// inside. Same for a direct-commit subject.
func TestRenderChangesStripsAppcastFenceMarkers(t *testing.T) {
	out := RenderChanges(Changes{
		PRs:    []PR{{Number: 1, Title: "fix: x <!-- appcast:end --> more"}},
		Direct: []Commit{{SHA: "abc123", Subject: "hack <!-- appcast:begin --> in"}},
	})
	if strings.Contains(out, "appcast:") {
		t.Errorf("fence marker leaked into the changelog:\n%s", out)
	}
	if !strings.Contains(out, "- x  more (#1)") {
		t.Errorf("stripped title mangled beyond the marker: %q", out)
	}
	if !strings.Contains(out, "hack  in") {
		t.Errorf("stripped subject mangled beyond the marker: %q", out)
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

// Coverage is the point of this test: PR #48 contributed abc111, so only
// def222 is a direct commit. Without this, `bump version` and every other
// push straight to dev vanishes from the changelog entirely.
func TestCollectSeparatesDirectCommitsFromPRCommits(t *testing.T) {
	run := func(name string, args ...string) (string, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "--merges"):
			return "m1\x1fMerge pull request #48 from knomit/feat/x\n" +
				"m2\x1fMerge remote-tracking branch 'origin/dev' into feat/x\n", nil
		case strings.Contains(joined, "m1^..m1^2"):
			return "abc111\n", nil
		case strings.Contains(joined, "--no-merges"):
			return "abc111\x1ffeat(x): the pr commit\ndef222\x1fbump version\n", nil
		}
		return "", fmt.Errorf("unexpected git args: %q", joined)
	}
	fetch := fakeFetcher{48: {Number: 48, Title: "feat(update): self-update", Body: "why"}}

	got, err := Collect(run, fetch, "from", "to")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.PRs) != 1 || got.PRs[0].Number != 48 {
		t.Fatalf("PRs = %+v, want exactly #48 (the branch-sync merge is not a PR)", got.PRs)
	}
	if len(got.Direct) != 1 || got.Direct[0].SHA != "def222" {
		t.Fatalf("Direct = %+v, want only def222", got.Direct)
	}
}

// A PR whose record is gone (deleted, or a merge from a fork we cannot read)
// must not fail the release — fall back to the merge subject.
func TestCollectToleratesAnUnfetchablePR(t *testing.T) {
	run := func(name string, args ...string) (string, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "--merges"):
			return "m1\x1fMerge pull request #99 from knomit/gone\n", nil
		case strings.Contains(joined, "--no-merges"):
			return "", nil
		}
		return "", nil
	}
	got, err := Collect(run, fakeFetcher{}, "from", "to")
	if err != nil {
		t.Fatalf("an unfetchable PR must not fail the run: %v", err)
	}
	if len(got.PRs) != 1 || got.PRs[0].Title != "knomit/gone" {
		t.Fatalf("PRs = %+v, want a #99 entry titled from the branch name", got.PRs)
	}
}

type fakeFetcher map[int]PR

func (f fakeFetcher) Fetch(n int) (PR, error) {
	pr, ok := f[n]
	if !ok {
		return PR{}, fmt.Errorf("no such PR %d", n)
	}
	return pr, nil
}
