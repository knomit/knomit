package skills

import (
	"io/fs"
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
