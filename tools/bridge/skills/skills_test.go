package skills

import (
	"github.com/stretchr/testify/require"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestFS_ContainsAllTenSkills pins the shipped skill set. Both hosts scaffold
// from this FS, so a skill silently dropped here disappears from every agent
// at once.
func TestFS_ContainsAllTenSkills(t *testing.T) {
	want := []string{
		"knomit-decided", "knomit-harden", "knomit-hypothesize", "knomit-principle",
		"knomit-recall", "knomit-remember", "knomit-retract", "knomit-review",
		"knomit-update", "knomit-why",
	}

	var got []string
	err := fs.WalkDir(FS, Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, "/SKILL.md") {
			return nil
		}
		rest := strings.TrimPrefix(p, Root+"/")
		got = append(got, strings.TrimSuffix(rest, "/SKILL.md"))
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	sort.Strings(got)
	if len(got) != len(want) {
		t.Fatalf("got %d skills %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("skill[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestFS_NoClaudeCodeSpecificText guards the host-neutrality of the shared
// templates: they are scaffolded verbatim into Antigravity, where these names
// mean nothing.
func TestFS_NoClaudeCodeSpecificText(t *testing.T) {
	banned := []string{"AskUserQuestion", "CLAUDE.md", ".claude/"}
	err := fs.WalkDir(FS, Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, readErr := fs.ReadFile(FS, p)
		if readErr != nil {
			return readErr
		}
		for _, s := range banned {
			if strings.Contains(string(b), s) {
				t.Errorf("%s contains host-specific text %q", p, s)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

// TestTemplates_MatchTheRepoDeployedCopies keeps the embedded templates and the
// copies under .claude/skills byte-identical.
//
// They ARE the same text in two places: the templates are what a bridge deploys
// to a user's agent host, and .claude/skills is this repo's own deployment,
// which is checked in (the .gitignore un-ignores it deliberately). Nothing kept
// them in sync but habit.
//
// Added after editing a template and having to NOTICE, unprompted, that a
// second copy existed. Every one of the ten was in sync at the time, so the
// invariant was real and maintained by hand — which is the state a silent drift
// starts from: the next editor gets the same coin-flip, and a stale deployed
// skill is invisible until someone reads both files side by side.
func TestTemplates_MatchTheRepoDeployedCopies(t *testing.T) {
	const deployed = "../../../.claude/skills"

	entries, err := fs.ReadDir(FS, Root)
	require.NoError(t, err)
	require.NotEmpty(t, entries, "the scan must actually be reading templates")

	checked := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		want, err := fs.ReadFile(FS, path.Join(Root, name, "SKILL.md"))
		if err != nil {
			continue // a template with no SKILL.md is not this test's business
		}
		got, err := os.ReadFile(filepath.Join(deployed, name, "SKILL.md"))
		require.NoErrorf(t, err,
			"skill %q has an embedded template but no deployed copy under "+
				".claude/skills — either deploy it or remove the template", name)
		require.Equalf(t, string(want), string(got),
			"skill %q has DRIFTED: the embedded template and this repo's deployed copy "+
				"under .claude/skills differ. They are the same text in two places; edit "+
				"both, or the deployed skill silently lags what ships to users.", name)
		checked++
	}
	require.Greater(t, checked, 5, "the scan must cover the skill set, not a corner of it")
}
