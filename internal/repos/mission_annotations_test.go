package repos_test

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"

	"knomit/internal/repos"
	"knomit/internal/store"
)

// F26: knowledge goes to the TARGET knowledge base the charter names, as
// ordinary facts; the mission repo holds coordination plus one topic,
// annotations/<task id>/, of facts ABOUT the target's facts. A cross-check
// writes annotations (context.kind: verdict) in the mission repo and never
// touches the target; ONE fold task, inside one experiment on the target, is
// the only writer of the hypotheses.

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

// TestMissionTemplate_NoMissionFormat: #410's mission-specific shape is gone
// — no companion knowledge-base template, no hypothesis format, no forecast
// topic — and nothing in examples/ names one mission's subject.
//
// SABOTAGE: restore examples/mission-kb/ → red; paste a `hypothesis-format`
// block back into post-task → red; write "IPO" into a skill → red.
func TestMissionTemplate_NoMissionFormat(t *testing.T) {
	examples := filepath.Join("..", "..", "examples")
	_, err := os.Stat(filepath.Join(examples, "mission-kb"))
	require.True(t, os.IsNotExist(err), "examples/mission-kb/ is gone (F26): %v", err)
	gone := regexp.MustCompile(`hypothesis-format|forecast|mission-kb`)
	seen := 0
	require.NoError(t, filepath.WalkDir(examples, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		seen++
		require.Empty(t, gone.FindAllString(string(b), -1), "%s still carries #410's mission format", p)
		return nil
	}))
	require.Greater(t, seen, 5, "fixture: the walk read the template")
	for p, content := range templateFiles(t) {
		require.Empty(t, missionLeak.FindAllString(content, -1), "%s names one mission's subject; the charter supplies it", p)
	}
}

// TestMissionTemplate_CrossCheckNeverUpdates: the skills never have a
// cross-check update what it checks or write to the target knowledge base;
// it writes annotations in the mission repo. The fold is one task, posted
// after the round's acks, the only writer, in one experiment.
//
// SABOTAGE: tell the cross-check to knomit_update its target → red; drop
// "posted only after EVERY cross-check" → red; replace the check: lines with
// "do not check your own" → red (F-R4); read a checker's author off a commit
// that may be a merge (no --no-merges) → red; a fold that refs the
// annotations from the knowledge base → red.
func TestMissionTemplate_CrossCheckNeverUpdates(t *testing.T) {
	files := templateFiles(t)
	post := files[".knomit/skills/post-task/SKILL.md"]
	xcheck := fencedBlock(t, post, "text", "cross-check-task")
	for _, l := range strings.Split(normalized(xcheck), ". ") {
		if strings.Contains(l, "knomit_update") {
			require.Contains(t, strings.ToLower(l), "never knomit_update", "a cross-check may only be told NOT to update: %q", l)
		}
	}
	for _, anchor := range []string{
		"write nothing to that knowledge base",
		"write ONE annotation with knomit_learn on the MISSION handle",
		"topic: annotations, category: <this task's id>",
		`context: {"kind": "verdict"`,
		"kb://<its repo id>/<its path>",
		"check exactly the facts on the check: lines below, and no other",
	} {
		require.Contains(t, normalized(xcheck), anchor)
	}
	require.Contains(t, xcheck, "\ncheck: <path>\n")

	fold := normalized(fencedBlock(t, post, "text", "fold-task"))
	for _, anchor := range []string{
		"knomit_query on the MISSION handle with path <root>/annotations/<id>/",
		`context {"kind": "verdict"}`,
		"this task's ONE experiment on the knowledge base it names",
		"refs replace the whole list",
		"Never ref the annotations from the knowledge base.",
		"with distinct_from naming the hypothesis it counters",
	} {
		require.Contains(t, fold, anchor)
	}
	for _, anchor := range []string{
		"posted only after EVERY cross-check of the round has acknowledged",
		"It is the only writer of the hypotheses.",
		"knomit does not hold the fold back until the acks are in",
		"git log --diff-filter=A --format=%an -1 <consensus branch> -- <path>",
		"git log --no-merges -1 --format=%an <its agent branch>",
		"Never write \"do not check your own\" instead",
	} {
		require.Contains(t, normalized(post), anchor)
	}
	work := normalized(files[".knomit/skills/work-task/SKILL.md"])
	for _, anchor := range []string{
		"A task that lists `check: <path>` lines checks exactly those paths, and no other",
		"`knomit_update` only a fact your task tells you to update, by its path.",
		"never updates the facts it checks, and writes nothing to the knowledge base.",
		"Only a fold task updates the facts the annotations point at.",
	} {
		require.Contains(t, work, anchor)
	}
}

// skeletonFor returns the work-task skill's ```json <tool> skeleton whose
// text contains marker, with its placeholders filled from fill (any other
// <...> becomes "x"), as call arguments without the binding, and the binding
// placeholder it names ("<mission>" or "<kb>"): the test routes the call to
// the repo that placeholder stands for, so a skeleton pointed at the wrong
// handle writes to the wrong repo here too.
func skeletonFor(t *testing.T, tool, marker string, fill map[string]string) (map[string]any, string) {
	t.Helper()
	skill := templateFiles(t)[".knomit/skills/work-task/SKILL.md"]
	re := regexp.MustCompile("(?s)```json " + tool + "\n(.*?)\n```")
	for _, m := range re.FindAllStringSubmatch(skill, -1) {
		if !strings.Contains(m[1], marker) {
			continue
		}
		s := m[1]
		var binding string
		if b := regexp.MustCompile(`"binding": "([^"]*)"`).FindStringSubmatch(s); b != nil {
			binding = b[1]
		}
		for k, v := range fill {
			s = strings.ReplaceAll(s, k, v)
		}
		s = regexp.MustCompile(`<[^>"]*>`).ReplaceAllString(s, "x")
		var args map[string]any
		require.NoError(t, json.Unmarshal([]byte(s), &args), s)
		delete(args, "binding")
		return args, binding
	}
	t.Fatalf("no %s skeleton containing %q", tool, marker)
	return nil, ""
}

// tips is every branch head of a repo.
func tips(t *testing.T, n *missionNode) map[string]string {
	t.Helper()
	svc := n.svc(t)
	bs, err := svc.Branches().ListBranches(context.Background())
	require.NoError(t, err)
	out := map[string]string{}
	for _, b := range bs {
		h, err := svc.Branches().HeadCommit(context.Background(), b.Name)
		require.NoError(t, err)
		out[b.Name] = h
	}
	return out
}

// newCommits is every commit reachable from tip and not from base.
func newCommits(t *testing.T, n *missionNode, base, tip string) []store.CommitInfo {
	t.Helper()
	svc := n.svc(t)
	walk := func(from string, stop map[plumbing.Hash]bool) map[plumbing.Hash]store.CommitInfo {
		out := map[plumbing.Hash]store.CommitInfo{}
		queue := []plumbing.Hash{plumbing.NewHash(from)}
		for len(queue) > 0 {
			h := queue[0]
			queue = queue[1:]
			if _, done := out[h]; done || stop[h] {
				continue
			}
			c, err := svc.Triggers().CommitInfo(context.Background(), h)
			require.NoError(t, err)
			out[h] = c
			queue = append(queue, c.Parents...)
		}
		return out
	}
	old := map[plumbing.Hash]bool{}
	for h := range walk(base, nil) {
		old[h] = true
	}
	var out []store.CommitInfo
	for _, c := range walk(tip, old) {
		out = append(out, c)
	}
	return out
}

// TestMission_CrossCheckAnnotatesFoldWrites is F26's single-writer check on
// two instances. H hosts the mission repo (the shipped template) and the
// target knowledge base; P is a member of the mission. A hypothesis with an
// ordinary body is accepted in the target. Two cross-checks, one on each
// instance, write their verdicts with the work-task skill's own annotation
// call: they land at two paths in the mission repo and make NO commit on the
// target. The fold then reads each task's verdicts by path plus context and,
// in one experiment on the target, updates the hypothesis: only the fold's
// commits are new on the target, each stamped with the mission and the fold
// task, and the hypothesis's history shows the earlier revision and the
// fold's change.
//
// SABOTAGE: point the skill's annotation skeleton at "<kb>" (the cross-check
// writes on the target) → red on the target's tips; file it under a shared
// category instead of `<task id>` → red on the two folders; let the fold's
// update land outside its experiment (bind it to the agent branch) → red on
// "the fold's work waits in its experiment".
func TestMission_CrossCheckAnnotatesFoldWrites(t *testing.T) {
	newMissionClock(t)
	h, url := newMissionHost(t, nil)
	p := newMissionPeer(t, url)
	exchange(t, h, p)

	// The target knowledge base: an ordinary one, NOT made from any mission
	// template, with dedup on.
	kbRI, err := h.m.Create(context.Background(), repos.CreateSpec{Name: "agentic", Mode: "custom", OntologyYAML: `id: agentic
name: Agentic
description: An ordinary knowledge base.
topics:
  findings:
    description: Evidence.
  hypotheses:
    description: Predictions.
`}, nil)
	require.NoError(t, err)
	target := &missionNode{name: "T", m: h.m, ri: kbRI, branch: mHostAgent, tools: h.tools}

	const run = "run-0123456789abcdef0123456789abcdef"
	traceOf := func(task string) map[string]any {
		return map[string]any{"Knomit-Trace": "kb", "Mission-Task": task, "Knomit-Run": run}
	}

	// A hypothesize task's result: an ordinary hypothesis, no format lines.
	out, isErr, text := target.call(t, "learn", map[string]any{"moment_name": "hyp-1: hypotheses", "trace": traceOf("hyp-1"),
		"facts": []any{map[string]any{
			"topic": "hypotheses", "category": "agents/memory", "type": "hypothesis",
			"title": "Agent memory moves into the harness within a year", "confidence": 0.6,
			"body":     "Harnesses are absorbing memory management; the gather lanes found three cases.",
			"entities": []any{"agent memory"},
		}}})
	require.False(t, isErr, "an ordinary hypothesis is accepted in the target: %s", text)
	hyp := out["commits"].([]any)[0].(map[string]any)["file"].(string)
	kbRef := "kb://" + kbRI.ID()[:12] + "/" + hyp
	require.Regexp(t, `^kb://[0-9a-f]{12}/`, kbRef)

	// Two cross-checks, one per instance, each with the skill's own call.
	before := tips(t, target)
	for _, c := range []struct {
		n             *missionNode
		task, verdict string
	}{{h, "xcheck-h", "corroborate"}, {p, "xcheck-p", "contradict"}} {
		args, binding := skeletonFor(t, "knomit_learn", `"topic": "annotations"`, map[string]string{
			"<mission repo name>": "kb", "<task id>": c.task, "<run id>": run,
			"kb://<repo id>/<the checked fact's path>": kbRef, "<your evidence>": "https://example.org/evidence",
		})
		f := args["facts"].([]any)[0].(map[string]any)
		f["context"].(map[string]any)["verdict"] = c.verdict
		on := map[string]*missionNode{"<mission>": c.n, "<kb>": target}[binding]
		require.NotNil(t, on, "the skeleton names a known handle: %q", binding)
		_, isErr, text := on.call(t, "learn", args)
		require.False(t, isErr, "%s: the skill's annotation call lands in the mission repo: %s", c.task, text)
	}
	exchange(t, h, p)
	require.Equal(t, before, tips(t, target), "the cross-checks made no commit on the target knowledge base")
	verdicts := h.paths(t, mHostAgent, "kb/annotations/")
	require.Len(t, verdicts, 2, "two verdicts on one target, two paths: %v", verdicts)
	require.True(t, strings.HasPrefix(verdicts[0], "kb/annotations/xcheck-h/") && strings.HasPrefix(verdicts[1], "kb/annotations/xcheck-p/"), "%v", verdicts)

	// The fold, on H: read each cross-check's verdicts by path plus context.
	for _, task := range []string{"xcheck-h", "xcheck-p"} {
		args, binding := skeletonFor(t, "knomit_query", `"context"`, map[string]string{"<root>": "kb", "<cross-check task id>": task})
		on := map[string]*missionNode{"<mission>": h, "<kb>": target}[binding]
		require.NotNil(t, on, "the skeleton names a known handle: %q", binding)
		out, isErr, text := on.call(t, "query", args)
		require.False(t, isErr, text)
		facts := out["facts"].([]any)
		require.Len(t, facts, 1, "%s: exactly its verdict", task)
		require.True(t, strings.HasPrefix(facts[0].(map[string]any)["file"].(string), "kb/annotations/"+task+"/"))
	}
	// One experiment on the target; the update inside it; the commit.
	svc := target.svc(t)
	_, err = svc.Experiments().OpenExperiment(context.Background(), "fold-1", "", mHostAgent)
	require.NoError(t, err)
	inExp := &missionNode{name: "T-exp", m: h.m, ri: kbRI, branch: store.ExperimentBranch("fold-1"), tools: h.tools}
	_, isErr, text = inExp.call(t, "update", map[string]any{"file": hyp, "moment_name": "fold: xcheck-h xcheck-p", "trace": traceOf("fold-1"),
		"updates": map[string]any{"confidence": 0.5}, "ops": []any{map[string]any{"op": "append", "text": "Cross-checked: one corroboration, one contradiction."}}})
	require.False(t, isErr, text)
	require.Equal(t, before[mHostAgent], tips(t, target)[mHostAgent], "fixture: the fold's work waits in its experiment")
	// The commit carries the fold's trace too, as the skill's call does.
	ctx, err := store.WithAgentTrace(context.Background(), store.Trailers{Trace: "kb", Run: run,
		Extra: []store.TrailerEntry{{Key: "Mission-Task", Value: "fold-1"}}})
	require.NoError(t, err)
	_, err = svc.Experiments().CommitExperiment(ctx, "fold-1", nil)
	require.NoError(t, err)

	after := tips(t, target)
	require.NotEqual(t, before[mHostAgent], after[mHostAgent], "the fold moved the target's agent branch")
	added := newCommits(t, target, before[mHostAgent], after[mHostAgent])
	require.NotEmpty(t, added)
	for _, c := range added {
		require.Equal(t, "fold-1", store.TrailerValue(c.Message, "Mission-Task"), "only the fold wrote the target: %q", c.Message)
		require.Equal(t, "kb", store.TrailerValue(c.Message, store.TrailerTrace), "stamped with the mission: %q", c.Message)
	}

	// History: the hypothesis's earlier revision and the fold's change.
	out, isErr, text = target.call(t, "explain", map[string]any{"file": hyp})
	require.False(t, isErr, text)
	root := out["facts"].([]any)[0].(map[string]any)
	revs := root["history"].(map[string]any)["revisions"].([]any)
	require.GreaterOrEqual(t, len(revs), 2, "the earlier revision and the fold's: %v", revs)
	require.Contains(t, revs[0].(map[string]any)["message"], "fold: xcheck-h xcheck-p")
}
