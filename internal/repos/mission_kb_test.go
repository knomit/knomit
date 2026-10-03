package repos_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// fencedBlock returns the body of the fenced block whose info string is
// "<lang> <marker>" in a skill, without the fences. The skills mark the
// blocks a test reads this way, so the text a session is told to use is the
// text the test runs.
func fencedBlock(t *testing.T, doc, lang, marker string) string {
	t.Helper()
	open := "```" + lang + " " + marker + "\n"
	i := strings.Index(doc, open)
	require.GreaterOrEqual(t, i, 0, "no ```%s %s block", lang, marker)
	rest := doc[i+len(open):]
	j := strings.Index(rest, "\n```")
	require.GreaterOrEqual(t, j, 0, "the ```%s %s block is not closed", lang, marker)
	return rest[:j+1]
}

// missionLeak matches content that belongs to one mission, not to the
// template: the first mission's subject and its instruments. The template is
// generic; a mission's charter supplies these.
var missionLeak = regexp.MustCompile(`(?i)\b(ipo|edgar|anthropic|first-trade|roadshow)\b|\bS-1\b`)

// TestMissionTemplate_PostTaskHypothesisFormat: the post-task skill carries
// the hypothesis format as a block to paste into every task that asks for
// predictions, with the three lines a hypothesis body starts with and the
// expires rule, and nothing in the template names one mission's subject.
//
// SABOTAGE: drop the settles_false_if line from the block → red; write "IPO"
// into a skill → red.
func TestMissionTemplate_PostTaskHypothesisFormat(t *testing.T) {
	files := templateFiles(t)
	skill := files[".knomit/skills/post-task/SKILL.md"]
	block := fencedBlock(t, skill, "text", "hypothesis-format")
	for _, anchor := range []string{
		"topic: forecast, category: <subject>/<granularity>",
		"type: hypothesis",
		"confidence = the probability",
		"\npredicted: <",
		"\nsettles_true_if: <",
		"\nsettles_false_if: <",
		`"counters: <path>"`,
		"expires = the last second of the predicted period, in UTC",
		"Never retract a\nhypothesis because it settled",
	} {
		require.Contains(t, block, anchor, "the hypothesis-format block must say this")
	}
	require.Contains(t, skill, "carries the hypothesis format IN ITS BODY, every time")
	require.Contains(t, files["README.md"], "### The hypothesis format")

	for p, content := range allTemplateFiles(t) {
		require.Empty(t, missionLeak.FindAllString(content, -1), "%s names one mission's subject; the charter supplies it", p)
	}
}

// allTemplateFiles is every file of the mission template and of its
// knowledge-base companion, keyed by a path naming which one.
func allTemplateFiles(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for p, c := range templateFiles(t) {
		out["mission/"+p] = c
	}
	for p, c := range kbTemplateFiles(t) {
		out["mission-kb/"+p] = c
	}
	return out
}

// kbTemplateDir is the knowledge-base companion of the mission template.
var kbTemplateDir = filepath.Join("..", "..", "examples", "mission-kb")

// kbTemplateFiles is every file of examples/mission-kb/, keyed by its repo
// path (forward slashes); empty while the directory does not exist.
func kbTemplateFiles(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(kbTemplateDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(kbTemplateDir, p)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return out
	}
	require.NoError(t, err)
	return out
}
