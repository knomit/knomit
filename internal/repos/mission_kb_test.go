package repos_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/repos"
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

// ---- the knowledge base (examples/mission-kb/)

// newMissionKB boots one real instance whose repo is created from the
// shipped knowledge-base ontology, as the README says to create it.
func newMissionKB(t *testing.T) *missionNode {
	t.Helper()
	files := kbTemplateFiles(t)
	require.Contains(t, files, ".knomit/ontology.yaml", "the knowledge-base template must ship its ontology")
	m, tools := newMissionManager(t, mHostAgent, "mission-kb", nil)
	ri, err := m.Create(context.Background(), repos.CreateSpec{Name: "kb", Mode: "custom", OntologyYAML: files[".knomit/ontology.yaml"]}, nil)
	require.NoError(t, err)
	return &missionNode{name: "K", m: m, ri: ri, branch: mHostAgent, tools: tools}
}

// hypothesisLines is the post-task skill's hypothesis-format block reduced
// to the lines a hypothesis body starts with, placeholders filled. It is
// taken from the SHIPPED skill on purpose: if the block and the ontology's
// rules ever disagree, the template tells sessions to write what its own
// knowledge base refuses, and this is where that turns red.
func hypothesisLines(t *testing.T, period string) []string {
	t.Helper()
	block := fencedBlock(t, templateFiles(t)[".knomit/skills/post-task/SKILL.md"], "text", "hypothesis-format")
	fill := regexp.MustCompile(`<[^>]*>`)
	var out []string
	for _, l := range strings.Split(block, "\n") {
		switch {
		case strings.Contains(l, "predicted:"):
			out = append(out, fill.ReplaceAllString(l, period))
		case strings.Contains(l, "settles_true_if:"):
			out = append(out, fill.ReplaceAllString(l, "the registry's public record lists the filing within the period"))
		case strings.Contains(l, "settles_false_if:"):
			out = append(out, fill.ReplaceAllString(l, "the registry's public record has no filing when the period ends"))
		}
	}
	require.Len(t, out, 3, "the block has the three format lines: %q", block)
	return out
}

// hypothesis is a forecast fact built from those lines.
func hypothesis(category string, lines []string, expires string) map[string]any {
	f := map[string]any{
		"topic": "forecast", "category": category, "type": "hypothesis",
		"title": "Prediction for " + category, "confidence": 0.4, "sources": 1,
		"body":     strings.Join(lines, "\n") + "\n\nReasoning: the evidence points this way.",
		"entities": []any{"subject-a"},
	}
	if expires != "" {
		f["expires"] = expires
	}
	return f
}

func without(lines []string, token string) []string {
	var out []string
	for _, l := range lines {
		if !strings.Contains(l, token) {
			out = append(out, l)
		}
	}
	return out
}

// TestMissionKB_ReadmeRoute creates the knowledge base exactly as the README
// says (git init; cp -R examples/mission-kb/. into it; commit; knomit clones
// that repository), on a branch that is not named after any default, and
// checks the ontology took: a hypothesis in the skill's format lands, one
// missing a settlement line is refused by name.
//
// SABOTAGE: drop the settles-false-if rule → the second learn lands → red.
func TestMissionKB_ReadmeRoute(t *testing.T) {
	originRoot := t.TempDir()
	repoDir := filepath.Join(originRoot, "my-mission-kb")
	git := func(dir string, args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = dir
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=m", "GIT_AUTHOR_EMAIL=m@example.invalid",
			"GIT_COMMITTER_NAME=m", "GIT_COMMITTER_EMAIL=m@example.invalid")
		out, err := c.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
	git(originRoot, "init", "-q", "-b", "trunk", "my-mission-kb")
	for p, content := range kbTemplateFiles(t) {
		target := filepath.Join(repoDir, filepath.FromSlash(p))
		require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o755))
		require.NoError(t, os.WriteFile(target, []byte(content), 0o644))
	}
	git(repoDir, "add", "-A")
	git(repoDir, "commit", "-q", "-m", "mission knowledge base")

	newMissionClock(t)
	m, tools := newMissionManager(t, mHostAgent, "mission-kb", func(c *config.Config) { c.LocalOriginRoot = originRoot })
	ri, err := m.Create(context.Background(), repos.CreateSpec{Name: "my-mission-kb", Mode: "clone",
		Origin: &repos.OriginSpec{URL: repoDir}}, nil)
	require.NoError(t, err)
	require.NoError(t, ri.OntologyError())
	k := &missionNode{name: "K", m: m, ri: ri, branch: mHostAgent, tools: tools}

	month := hypothesisLines(t, "2026-10")
	_, isErr, text := k.call(t, "learn", map[string]any{"moment_name": "forecast", "facts": []any{
		hypothesis("subject-a/month", month, "2026-10-31T23:59:59Z")}})
	require.False(t, isErr, text)
	_, isErr, text = k.call(t, "learn", map[string]any{"moment_name": "forecast", "facts": []any{
		hypothesis("subject-a/month", without(month, "settles_false_if:"), "2026-10-31T23:59:59Z")}})
	require.True(t, isErr, "the cloned knowledge base enforces the format")
	require.Contains(t, text, `"settles-false-if"`)
}

// TestMissionKB_Settings: the knowledge-base ontology parses as a NEW
// ontology, keeps the mission repo's repo settings, has forecast with
// learn_dedup: off, and has no due-retract (nobody retracts a hypothesis
// because it settled).
//
// SABOTAGE: drop `learn_dedup: off` under forecast → red; add an `on: due`
// trigger to forecast → red.
func TestMissionKB_Settings(t *testing.T) {
	files := kbTemplateFiles(t)
	raw := []byte(files[".knomit/ontology.yaml"])
	o, err := fact.ParseNewOntology(raw)
	require.NoError(t, err)
	cs, err := fact.ReadConsensus(raw)
	require.NoError(t, err)
	require.Equal(t, fact.ConsensusAuto, cs.Mode)
	sy, err := fact.ReadSync(raw)
	require.NoError(t, err)
	require.True(t, sy.RealtimePush() && sy.RealtimePull())
	require.True(t, o.LearnDedupOff("forecast"), "forecast must be learn_dedup: off")
	require.True(t, o.LearnDedupOff("verdicts"), "verdicts must be learn_dedup: off")
	var trig struct {
		Topics map[string]struct {
			Triggers []struct {
				Name string `yaml:"name"`
				On   string `yaml:"on"`
			} `yaml:"triggers"`
		} `yaml:"topics"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &trig))
	for topic, tp := range trig.Topics {
		for _, tr := range tp.Triggers {
			require.NotEqual(t, "due", tr.On, "%s/%s: no trigger acts on a hypothesis's expires; people settle it", topic, tr.Name)
		}
	}
	branchWord := regexp.MustCompile(`\b(main|master|trunk|develop)\b`)
	for p, content := range files {
		require.Empty(t, branchWord.FindAllString(content, -1), "%s names a branch", p)
	}
}

// TestMissionKB_ForecastValidations runs the shipped hypothesis format
// against the shipped knowledge-base ontology on a real instance, through the
// real knomit_learn and knomit_update handlers: the skill's own block is
// ACCEPTED at every granularity, and each way of breaking it is refused by
// the rule that names the broken line.
//
// SABOTAGE: delete the settles-false-if rule → the case without that line is
// accepted → red; compare expires by date only → the one-second-early cases
// are accepted → red; bullet the format lines in the skill → the accepted
// cases are refused → red; delete hypothesis-only → the insight is accepted
// → red.
func TestMissionKB_ForecastValidations(t *testing.T) {
	newMissionClock(t)
	k := newMissionKB(t)
	learn := func(f map[string]any) (string, bool, string) {
		out, isErr, text := k.call(t, "learn", map[string]any{"moment_name": "forecast", "facts": []any{f}})
		if isErr {
			return "", true, text
		}
		return out["commits"].([]any)[0].(map[string]any)["file"].(string), false, text
	}

	year, month, day := hypothesisLines(t, "2026"), hypothesisLines(t, "2026-10"), hypothesisLines(t, "2026-10-23")
	for name, f := range map[string]map[string]any{
		"year":                 hypothesis("subject-a/year", year, "2026-12-31T23:59:59Z"),
		"month (31 days)":      hypothesis("subject-a/month", month, "2026-10-31T23:59:59Z"),
		"month (30 days)":      hypothesis("subject-a/month", hypothesisLines(t, "2026-11"), "2026-11-30T23:59:59Z"),
		"month (leap feb)":     hypothesis("subject-a/month", hypothesisLines(t, "2028-02"), "2028-02-29T23:59:59Z"),
		"day":                  hypothesis("subject-a/day", day, "2026-10-23T23:59:59Z"),
		"same instant, offset": hypothesis("subject-a/month", month, "2026-11-01T01:59:59+02:00"),
		"words after period":   hypothesis("subject-a/month", hypothesisLines(t, "2026-10 (the announcement)"), "2026-10-31T23:59:59Z"),
		"counter":              hypothesis("subject-a/month", append(append([]string{}, month...), "counters: kb/forecast/subject-a/month/x.md"), "2026-10-31T23:59:59Z"),
	} {
		_, isErr, text := learn(f)
		require.False(t, isErr, "%s: the skill's own format must be accepted: %s", name, text)
	}

	bulleted := append([]string{"- " + month[0]}, month[1:]...)
	insight := hypothesis("subject-a/month", month, "2026-10-31T23:59:59Z")
	insight["type"] = "insight"
	for _, c := range []struct {
		name, rule string
		f          map[string]any
	}{
		{"not a hypothesis", "hypothesis-only", insight},
		{"no predicted line", "predicted", hypothesis("subject-a/month", without(month, "predicted:"), "2026-10-31T23:59:59Z")},
		{"bulleted predicted line", "predicted", hypothesis("subject-a/month", bulleted, "2026-10-31T23:59:59Z")},
		{"no such day", "predicted", hypothesis("subject-a/day", hypothesisLines(t, "2026-02-30"), "2026-03-02T23:59:59Z")},
		{"no such month", "predicted", hypothesis("subject-a/month", hypothesisLines(t, "2026-13"), "2027-01-31T23:59:59Z")},
		{"no settles_true_if", "settles-true-if", hypothesis("subject-a/month", without(month, "settles_true_if:"), "2026-10-31T23:59:59Z")},
		{"no settles_false_if", "settles-false-if", hypothesis("subject-a/month", without(month, "settles_false_if:"), "2026-10-31T23:59:59Z")},
		{"no expires", "expires-ends-period", hypothesis("subject-a/month", month, "")},
		{"year, a second early", "expires-ends-period", hypothesis("subject-a/year", year, "2026-12-31T23:59:58Z")},
		{"month, a second early", "expires-ends-period", hypothesis("subject-a/month", month, "2026-10-31T23:59:58Z")},
		{"month, the 30th of a 31-day month", "expires-ends-period", hypothesis("subject-a/month", month, "2026-10-30T23:59:59Z")},
		{"day, a second early", "expires-ends-period", hypothesis("subject-a/day", day, "2026-10-23T23:59:58Z")},
		{"day, its start", "expires-ends-period", hypothesis("subject-a/day", day, "2026-10-23T00:00:00Z")},
		{"day filed under month", "granularity-in-path", hypothesis("subject-a/month", day, "2026-10-23T23:59:59Z")},
		{"no granularity segment", "granularity-in-path", hypothesis("subject-a", month, "2026-10-31T23:59:59Z")},
	} {
		_, isErr, text := learn(c.f)
		require.True(t, isErr, "%s must be refused", c.name)
		require.Contains(t, text, `"`+c.rule+`"`, "%s is refused by rule %s", c.name, c.rule)
	}

	// knomit_update runs the same rules: the fold (or anyone) cannot move a
	// hypothesis's expires off its period or drop a settlement line, and an
	// update that keeps the format lands.
	path, isErr, text := learn(hypothesis("subject-a/month", month, "2026-10-31T23:59:59Z"))
	require.False(t, isErr, text)
	update := func(args map[string]any) (bool, string) {
		args["file"], args["moment_name"] = path, "fold: xcheck-1"
		_, isErr, text := k.call(t, "update", args)
		return isErr, text
	}
	isErr, text = update(map[string]any{"updates": map[string]any{"confidence": 0.55}})
	require.False(t, isErr, "a confidence change keeps the format: %s", text)
	isErr, text = update(map[string]any{"updates": map[string]any{"expires": "2026-10-30T23:59:59Z"}})
	require.True(t, isErr)
	require.Contains(t, text, `"expires-ends-period"`)
	isErr, text = update(map[string]any{"ops": []any{map[string]any{"op": "str_replace", "old_str": month[2], "new_str": ""}}})
	require.True(t, isErr)
	require.Contains(t, text, `"settles-false-if"`)
}

// verdictLines is the post-task skill's verdict-format block reduced to the
// three lines a verdict body starts with, filled.
func verdictLines(t *testing.T, verdict, target, confidence string) []string {
	t.Helper()
	block := fencedBlock(t, templateFiles(t)[".knomit/skills/post-task/SKILL.md"], "text", "verdict-format")
	fill := regexp.MustCompile(`<[^>]*>`)
	var out []string
	for _, l := range strings.Split(block, "\n") {
		switch {
		case strings.Contains(l, "verdict:"):
			out = append(out, fill.ReplaceAllString(l, verdict))
		case strings.Contains(l, "target:"):
			out = append(out, fill.ReplaceAllString(l, target))
		case strings.Contains(l, "suggested_confidence:"):
			out = append(out, fill.ReplaceAllString(l, confidence))
		}
	}
	require.Len(t, out, 3, "the block has the three verdict lines: %q", block)
	return out
}

func verdictFact(category string, lines []string, refs ...string) map[string]any {
	r := make([]any, len(refs))
	for i, x := range refs {
		r[i] = x
	}
	return map[string]any{
		"topic": "verdicts", "category": category, "type": "observation",
		"title": "Verdict on a hypothesis", "confidence": 0.7, "sources": 1,
		"body":     strings.Join(lines, "\n") + "\n\nReasons: the evidence.",
		"entities": []any{"subject-a"}, "refs": r,
	}
}

// TestMissionKB_VerdictValidations: a cross-check's verdict written from the
// skill's own verdict-format block is accepted on a real instance, and each
// broken line is refused by its rule.
//
// SABOTAGE: delete the refs-target rule → the verdict whose refs omit its
// target is accepted → red; bullet the block's lines → red.
func TestMissionKB_VerdictValidations(t *testing.T) {
	newMissionClock(t)
	k := newMissionKB(t)
	out, isErr, text := k.call(t, "learn", map[string]any{"moment_name": "forecast", "facts": []any{
		hypothesis("subject-a/month", hypothesisLines(t, "2026-10"), "2026-10-31T23:59:59Z")}})
	require.False(t, isErr, text)
	target := out["commits"].([]any)[0].(map[string]any)["file"].(string)
	evidence := "https://example.org/registry/record-1"
	learn := func(f map[string]any) (bool, string) {
		_, isErr, text := k.call(t, "learn", map[string]any{"moment_name": "xcheck-1", "facts": []any{f}})
		return isErr, text
	}

	for name, f := range map[string]map[string]any{
		"corroborate":       verdictFact("xcheck-1", verdictLines(t, "corroborate", target, "0.55"), target, evidence),
		"contradict":        verdictFact("xcheck-1", verdictLines(t, "contradict", target, "0.3"), target, evidence),
		"confidence 1":      verdictFact("xcheck-2", verdictLines(t, "corroborate", target, "1"), target),
		"confidence 0":      verdictFact("xcheck-2", verdictLines(t, "contradict", target, "0"), target),
		"same target again": verdictFact("xcheck-2", verdictLines(t, "corroborate", target, "0.55"), target, evidence),
	} {
		isErr, text := learn(f)
		require.False(t, isErr, "%s: the skill's own verdict format must be accepted: %s", name, text)
	}

	good := verdictLines(t, "corroborate", target, "0.55")
	for _, c := range []struct {
		name, rule string
		f          map[string]any
	}{
		{"no verdict line", "verdict", verdictFact("xcheck-1", without(good, "verdict:"), target)},
		{"a verdict that is neither", "verdict", verdictFact("xcheck-1", verdictLines(t, "maybe", target, "0.5"), target)},
		{"bulleted verdict line", "verdict", verdictFact("xcheck-1", append([]string{"- " + good[0]}, good[1:]...), target)},
		{"no target line", "target", verdictFact("xcheck-1", without(good, "target:"), target)},
		{"no suggested confidence", "suggested-confidence", verdictFact("xcheck-1", without(good, "suggested_confidence:"), target)},
		{"confidence above 1", "suggested-confidence", verdictFact("xcheck-1", verdictLines(t, "corroborate", target, "1.4"), target)},
		{"refs without the target", "refs-target", verdictFact("xcheck-1", good, evidence)},
	} {
		isErr, text := learn(c.f)
		require.True(t, isErr, "%s must be refused", c.name)
		require.Contains(t, text, `"`+c.rule+`"`, "%s is refused by rule %s", c.name, c.rule)
	}
}

// TestMissionTemplate_CrossCheckNeverUpdates: the skills never have a
// cross-check update what it checks, and the fold is one task, posted after
// the round's acks, the only writer.
//
// SABOTAGE: tell the cross-check to knomit_update its target with a higher
// confidence → red; drop "posted only after EVERY cross-check" → red; replace
// the check: lines with "do not check your own" → red (F-R4); read a
// checker's author off a commit that may be a merge (no --no-merges) → red.
func TestMissionTemplate_CrossCheckNeverUpdates(t *testing.T) {
	files := templateFiles(t)
	post := files[".knomit/skills/post-task/SKILL.md"]
	xcheck := fencedBlock(t, post, "text", "verdict-format")
	for _, l := range strings.Split(normalized(xcheck), ". ") {
		if strings.Contains(l, "knomit_update") {
			require.Contains(t, strings.ToLower(l), "never knomit_update", "a cross-check may only be told NOT to update: %q", l)
		}
	}
	require.Contains(t, normalized(xcheck), "topic: verdicts, category: <this task's id>")
	fold := fencedBlock(t, post, "text", "fold-task")
	require.Contains(t, normalized(fold), "make ONE knomit_update")
	require.Contains(t, normalized(fold), "refs replace the whole list")
	for _, anchor := range []string{
		"posted only after EVERY cross-check of the round has acknowledged",
		"It is the only writer of the hypotheses.",
		"knomit does not hold the fold back until the acks are in",
	} {
		require.Contains(t, normalized(post), anchor)
	}
	work := files[".knomit/skills/work-task/SKILL.md"]
	// F-R4: the paths are named, never "not your own".
	require.Contains(t, normalized(xcheck), "check exactly the facts on the check: lines below, and no other")
	require.Contains(t, xcheck, "\ncheck: <path>\n")
	for _, anchor := range []string{
		"git log --diff-filter=A --format=%an -1 <consensus branch> -- <path>",
		"git log --no-merges -1 --format=%an <its agent branch>",
		"Never write \"do not check your own\" instead",
	} {
		require.Contains(t, normalized(post), anchor)
	}
	require.Contains(t, normalized(work), "A task that lists `check: <path>` lines checks exactly those paths, and no other")
	for _, anchor := range []string{
		"`knomit_update` only a fact your task tells you to update, by its path.",
		"never updates the facts it checks.",
		"Only a fold task updates the facts the verdicts point at.",
	} {
		require.Contains(t, normalized(work), anchor)
	}
}
