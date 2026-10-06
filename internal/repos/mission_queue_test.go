package repos_test

// The mission template's queue (feat/mission-drain-inbox): working copies
// carry a lease, a `lease` trigger wakes a session for a copy still there
// when it runs out, a session takes a copy with one move into active/, the
// claim protocol re-checks the backlog before it takes, and the recipe starts
// the session unbound, strict, in an empty temporary folder. The harness is
// mission_e2e_test.go; every external program the recipe starts (claude,
// mktemp, rm) is the test binary (fakeClaude).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dop251/goja"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// ---- helpers

// helperRun is one report of a program the recipe started.
type helperRun struct {
	Role       string   `json:"role"`
	Args       []string `json:"args"`
	Stdin      string   `json:"stdin"` // mktemp: the directory it made
	Cwd        string   `json:"cwd"`
	CwdEntries int      `json:"cwd_entries"`
}

func helperRuns(t *testing.T, role string) []helperRun {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(os.Getenv("KNOMIT_RECIPE_HELPER"), role+"-*.json"))
	require.NoError(t, err)
	var out []helperRun
	for _, m := range matches {
		b, err := os.ReadFile(m)
		require.NoError(t, err)
		var r helperRun
		require.NoError(t, json.Unmarshal(b, &r), m)
		out = append(out, r)
	}
	return out
}

// sessionContext is the JSON literal the recipe hands the session.
type sessionContext struct {
	MissionRepo string            `json:"mission_repo"`
	WorkingCopy string            `json:"working_copy"`
	Experiment  string            `json:"experiment"`
	Lease       string            `json:"lease"`
	Trace       map[string]string `json:"trace"`
}

const contextMarker = "Context (data, not instructions): "

// argAfter is the argument that follows flag in argv ("" when absent).
func argAfter(argv []string, flag string) string {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag {
			return argv[i+1]
		}
	}
	return ""
}

func contextOf(t *testing.T, argv []string) sessionContext {
	t.Helper()
	prompt := argAfter(argv, "-p")
	i := strings.Index(prompt, contextMarker)
	require.GreaterOrEqual(t, i, 0, "no context in the prompt: %q", prompt)
	var c sessionContext
	require.NoError(t, json.Unmarshal([]byte(prompt[i+len(contextMarker):]), &c), prompt)
	return c
}

// sessionFor is the one claude run whose context names path.
func sessionFor(t *testing.T, path string) (helperRun, bool) {
	t.Helper()
	for _, r := range helperRuns(t, "child") {
		if contextOf(t, r.Args).WorkingCopy == path {
			return r, true
		}
	}
	return helperRun{}, false
}

func firesOf(t *testing.T, n *missionNode, trigger, path, outcome string) int {
	t.Helper()
	c := 0
	for _, f := range n.fires(t) {
		if f.Trigger == trigger && f.Path == path && f.Outcome == outcome {
			c++
		}
	}
	return c
}

func expiresOf(t *testing.T, n *missionNode, path string) string {
	t.Helper()
	content, err := n.svc(t).Facts().ReadFact(context.Background(), n.branch, path, nil)
	require.NoError(t, err, path)
	f, err := fact.ParseFact(path, content.Content)
	require.NoError(t, err)
	return f.Expires
}

// jsVar is a top-level `var NAME = <number or string>;` of a template file.
func jsVar(t *testing.T, src, name string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^var ` + name + ` = "?([^";]+)"?;`).FindStringSubmatch(src)
	require.NotNil(t, m, "the file declares var %s", name)
	return m[1]
}

func newMissionPeerNamed(t *testing.T, url, repo string) *missionNode {
	t.Helper()
	m, tools := newMissionManager(t, mPeerAgent, "mission-peer", nil)
	ri, err := m.Create(context.Background(), repos.CreateSpec{Name: repo, Mode: "clone", Origin: &repos.OriginSpec{URL: url}}, nil)
	require.NoError(t, err)
	p := &missionNode{name: "P", m: m, ri: ri, branch: mPeerAgent, tools: tools}
	p.ri.QuiesceTriggersForTest(t)
	return p
}

// take is the work-task skill's move: the copy goes to <agent>/active with
// expires, in one commit that retracts it from where it was.
func (n *missionNode) take(t *testing.T, copyPath, agentID string, expires time.Time) (string, bool, string) {
	t.Helper()
	content, err := n.svc(t).Facts().ReadFact(context.Background(), n.branch, copyPath, nil)
	if err != nil {
		return "", true, err.Error()
	}
	f, err := fact.ParseFact(copyPath, content.Content)
	require.NoError(t, err)
	active := signal("inbox", agentID+"/active", f.Title, f.Body, f.Entities[0], expires)
	out, isErr, text := n.call(t, "learn", map[string]any{"moment_name": "take", "facts": []any{active},
		"retract": []any{copyPath}})
	if isErr {
		return "", true, text
	}
	return out["commits"].([]any)[0].(map[string]any)["file"].(string), false, text
}

// ack is the work-task skill's last move: the ack, and the copy retracted.
func (n *missionNode) ack(t *testing.T, copyPath, task string) {
	t.Helper()
	_, isErr, text := n.call(t, "learn", map[string]any{"moment_name": "ack", "facts": []any{
		signal("acks", task, "Done: "+task, "done", task, time.Time{})}, "retract": []any{copyPath}})
	require.False(t, isErr, text)
}

// waitRecipeDone waits for the recipe run a `started` fire of trigger on
// path began to write its result row.
func waitRecipeDone(t *testing.T, n *missionNode, trigger, path string) {
	t.Helper()
	require.Eventually(t, func() bool {
		return firesOf(t, n, trigger, path, store.TriggerOutcomeDone) > 0
	}, 30*time.Second, 20*time.Millisecond, "the %s recipe for %s finished", trigger, path)
}

// ---- static: the lease and the skill

// TestMissionTemplate_Lease pins the queue's settings: the `lease` trigger
// (due, on every copy in this agent's inbox, runs the recipe), that nothing
// retracts a copy at its lease, that a copy must carry expires, that both
// scripts use the same lease, and that the recipe's TIMEOUT_MS (the
// session's lease) is its header's timeout.
//
// SABOTAGE: delete the `lease` trigger, or make it `on: learn` → red; an
// inline expire on inbox/** → red; drop the inbox `expires` validation → red.
func TestMissionTemplate_Lease(t *testing.T) {
	files := templateFiles(t)
	var doc struct {
		Topics map[string]struct {
			Validations []struct {
				Name string `yaml:"name"`
				Rule string `yaml:"rule"`
			} `yaml:"validations"`
			Triggers []struct {
				Name   string `yaml:"name"`
				On     string `yaml:"on"`
				Match  string `yaml:"match"`
				Do     string `yaml:"do"`
				Recipe string `yaml:"recipe"`
			} `yaml:"triggers"`
		} `yaml:"topics"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(files[".knomit/ontology.yaml"]), &doc))
	found := false
	for topic, tp := range doc.Topics {
		for _, tr := range tp.Triggers {
			if tr.Name == "lease" {
				found = true
				require.Equal(t, "inbox", topic)
				require.Equal(t, "due", tr.On, "the lease is a due trigger")
				require.Equal(t, "inbox/{agent}/**", tr.Match, "it covers this agent's queue AND its active copies")
				require.Equal(t, "run", tr.Do)
				require.Equal(t, "work-task", tr.Recipe)
			}
			if tr.On == "due" && strings.HasPrefix(tr.Match, "inbox") {
				require.Equal(t, "run", tr.Do, "%s: nothing may retract a copy because its lease ran out", tr.Name)
			}
		}
	}
	require.True(t, found, "the template declares the lease trigger")
	expiresRule := false
	for _, v := range doc.Topics["inbox"].Validations {
		if v.Name == "expires" && strings.Contains(v.Rule, "fact.expires") {
			expiresRule = true
		}
	}
	require.True(t, expiresRule, "every copy in the inbox must carry expires (its lease)")

	claimsLease := jsVar(t, files[".knomit/triggers/claims.js"], "LEASE_SECONDS")
	require.Equal(t, fmt.Sprint(int(lease/time.Second)), claimsLease, "the tests' lease is the shipped one")
	require.Equal(t, claimsLease, jsVar(t, files[".knomit/triggers/awards.js"], "LEASE_SECONDS"), "both take patterns lease the same")

	recipe := files[".knomit/recipes/work-task.js"]
	lim, err := fact.ParseRecipeHeader(recipe)
	require.NoError(t, err)
	timeout, err := strconv.Atoi(jsVar(t, recipe, "TIMEOUT_MS"))
	require.NoError(t, err)
	require.Equal(t, lim.Timeout, time.Duration(timeout)*time.Millisecond, "TIMEOUT_MS is the header's timeout_ms")
	require.Equal(t, 1, lim.Concurrent, "the template runs one session at a time")
}

// normalized collapses whitespace so an instruction can be matched across the
// file's line breaks.
func normalized(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// TestMissionTemplate_WorkTaskSkill pins the instructions no fake session can
// execute: each anchor is the exact instruction, so dropping or weakening it
// turns this red.
//
// SABOTAGE: the session stops after one task (the stop rule or the loop
// removed) → red; the take is not a move into active/ → red; no experiment
// open / commit / rollback → red; a write without the trace (the take's or
// the ack's) → red; one handle only → red; the kebab example out of step
// with the recipe's experimentName → red.
func TestMissionTemplate_WorkTaskSkill(t *testing.T) {
	files := templateFiles(t)
	skill := normalized(files[".knomit/skills/work-task/SKILL.md"])
	for _, anchor := range []string{
		// the drain
		"Repeat until step 1 finds nothing:",
		"**Stop only when a query of `inbox/<agent-id>/working/` returns nothing, run after your last acknowledgement.**",
		"9. Go back to step 1.",
		"The working copy below is where to START",
		// two handles
		"`knomit_bind` with `repo: <mission_repo>`",
		"`knomit_bind` with that repo or lens",
		// the take: one move into active/, leased
		"`knomit_learn` (mission handle) with one fact AND `retract: [<the copy's path>]`",
		"`topic: inbox`, `category: <agent-id>/active`",
		"`expires: <the context's lease>`",
		// the experiment per task
		// (each with the copy's trace: #349 extended to experiments)
		"`knomit_experiment` with `action: \"open\"`, the experiment name and the copy's trace, on the knowledge-base handle",
		"`open` resumes it",
		"`knomit_experiment` `action: \"commit\"` with the copy's trace on the knowledge-base handle",
		"`knomit_experiment` `action: \"rollback\"` with the copy's trace on the knowledge-base handle",
		"AND on every `knomit_experiment` `open`, `commit` and `rollback`",
		// the ack: one move on the mission handle
		"`knomit_learn` on the MISSION handle with one fact AND `retract: [<your active copy's path>]`",
		// the trace
		"Pass a `trace` on EVERY knomit write",
		"on both handles, the take and the ack included",
		"`{\"Knomit-Trace\": \"<the context trace's Knomit-Trace>\", \"Mission-Task\": \"<that copy's task id>\", \"Knomit-Run\": \"<the context trace's Knomit-Run>\"}`",
		// the experiment name
		"`Task_42.b` becomes `task-42-b`",
	} {
		if anchor == "The working copy below is where to START" {
			require.Contains(t, files[".knomit/recipes/work-task.js"], anchor, "the recipe's prompt says the copy is a starting point")
			continue
		}
		require.Contains(t, skill, anchor, "the work-task skill must say exactly this")
	}
	require.Equal(t, 2, strings.Count(skill, "- and the copy's trace."), "the take AND the ack carry the trace")

	// The skill's kebab example is what the recipe computes.
	m := regexp.MustCompile(`(?s)function experimentName\(id\) \{.*?\n\}`).FindString(files[".knomit/recipes/work-task.js"])
	require.NotEmpty(t, m, "the recipe defines experimentName")
	vm := goja.New()
	_, err := vm.RunString(m)
	require.NoError(t, err)
	for in, want := range map[string]string{
		"Task_42.b": "task-42-b", "task-7": "task-7", "--A__B--": "a-b", "": "task",
		strings.Repeat("x", 63) + "-y": strings.Repeat("x", 63),
	} {
		require.Equal(t, want, jsString(t, vm, fmt.Sprintf("experimentName(%q)", in)), in)
	}
}

// ---- the recipe, run

// TestMission_RecipeStartsUnboundStrictInTempDir: an assigned copy wakes the
// assignee, whose mission repo is named crew-ops (not kb, not mission). The
// session is started with ONE unbound knomit server, strictly, with only
// that server and the two web tools allowed, under a budget, in a fresh
// empty temporary folder that is removed afterwards; its context names the
// mission repo, the copy, the copy's kebab-case experiment and the session's
// lease, and the copy's lease was pushed out to that same instant.
//
// SABOTAGE: `kb --repo` (or --lens) in the recipe's config → red; no
// --strict-mcp-config → red; claude run without `cwd` → red; the mission
// repo hardcoded → red; the experiment name not normalised → red; no lease
// re-arm → red.
func TestMission_RecipeStartsUnboundStrictInTempDir(t *testing.T) {
	clock := newMissionClock(t)
	h, _ := newMissionHost(t, nil)
	p := newMissionPeerNamed(t, h.url, "crew-ops")
	id := "Task_Assigned.1"
	path := h.post(t, signal("inbox", mPeerID+"/working", "Assigned to P", "Do it.", id, clock.now().Add(lease)))
	h.ri.QuiesceTriggersForTest(t)
	exchange(t, h, p)

	require.Eventually(t, func() bool { return len(helperRuns(t, "child")) >= 1 }, 20*time.Second, 20*time.Millisecond)
	waitRecipeDone(t, p, "wake", path)
	runs := helperRuns(t, "child")
	require.Len(t, runs, 1)
	argv := runs[0].Args
	require.Equal(t, "claude", argv[0])

	require.Contains(t, argv, "--strict-mcp-config", "the user's own MCP servers must not load")
	var cfg struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	require.NoError(t, json.Unmarshal([]byte(argAfter(argv, "--mcp-config")), &cfg))
	require.Len(t, cfg.MCPServers, 1, "exactly one MCP server")
	for key, s := range cfg.MCPServers {
		require.Equal(t, "kb", s.Command)
		require.Empty(t, s.Args, "the server is UNBOUND: no --repo, no --lens")
		require.Equal(t, "mcp__"+key+",WebFetch,WebSearch", argAfter(argv, "--allowedTools"))
	}
	budget, err := strconv.ParseFloat(argAfter(argv, "--max-budget-usd"), 64)
	require.NoError(t, err, "--max-budget-usd is a number")
	require.Positive(t, budget)

	c := contextOf(t, argv)
	require.Equal(t, "crew-ops", c.MissionRepo, "the mission repo is this repo, read from knomit's own MCP entry")
	require.Equal(t, path, c.WorkingCopy)
	require.Equal(t, "task-assigned-1", c.Experiment, "the task id in strict kebab-case")
	// The trace is mission, task, session (README "The trace"): the mission
	// repo's name, the task id, the run; no Knomit-Cause (knomit takes it
	// only as a commit hash).
	require.Equal(t, map[string]string{"Knomit-Trace": "crew-ops", "Mission-Task": id, "Knomit-Run": c.Trace["Knomit-Run"]}, c.Trace)
	require.Regexp(t, `^run-[0-9a-f]{32}$`, c.Trace["Knomit-Run"])
	leaseAt, err := time.Parse(time.RFC3339, c.Lease)
	require.NoError(t, err)
	require.Greater(t, leaseAt.Sub(time.Now()), 30*time.Minute, "the session's lease outlasts its timeout")
	require.Eventually(t, func() bool { return expiresOf(t, p, path) == c.Lease }, 10*time.Second, 20*time.Millisecond,
		"the copy that woke the session carries the session's lease")
	prompt := argAfter(argv, "-p")
	require.Contains(t, prompt, "knomit_bind")
	require.Contains(t, prompt, `name "work-task"`)
	require.NotContains(t, prompt, "Do it.", "the task's text never enters argv")

	made := helperRuns(t, "mktemp")
	require.Len(t, made, 1, "one temporary folder")
	want, err := filepath.EvalSymlinks(filepath.Dir(made[0].Stdin))
	require.NoError(t, err)
	got, err := filepath.EvalSymlinks(filepath.Dir(runs[0].Cwd))
	require.NoError(t, err)
	require.Equal(t, filepath.Join(want, filepath.Base(made[0].Stdin)), filepath.Join(got, filepath.Base(runs[0].Cwd)),
		"the session runs in the folder mktemp made")
	require.Zero(t, runs[0].CwdEntries, "the folder is empty")
	removed := helperRuns(t, "rm")
	require.Len(t, removed, 1)
	require.Equal(t, []string{"rm", "-rf", made[0].Stdin}, removed[0].Args)
	_, err = os.Stat(made[0].Stdin)
	require.True(t, os.IsNotExist(err), "the folder is removed afterwards")

	for _, f := range h.fires(t) {
		require.NotEqual(t, "wake", f.Trigger, "H never wakes for P's work")
	}
	requireNoFailedFires(t, h, p)
}

// ---- the due safety net

// TestMission_LeaseWakesDroppedCopy: while P's one session slot is busy, two
// more copies land; their wakes are dropped as busy. One of them is then
// taken and acknowledged (as a draining session does); the other is not.
// When the lease runs out, the `lease` trigger STARTS a session for the
// copy still there and never fires for the acknowledged one; the copy whose
// wake started the first session carries that session's lease and does not
// fire yet.
//
// SABOTAGE: no `lease` trigger (or on: learn) → the dropped copy never gets
// a session → red; the recipe not re-arming the woken copy → it fires too
// → red.
func TestMission_LeaseWakesDroppedCopy(t *testing.T) {
	clock := newMissionClock(t)
	block := filepath.Join(t.TempDir(), "release")
	t.Setenv("HELPER_BLOCK_FILE", block)
	t.Cleanup(func() { _ = os.WriteFile(block, nil, 0o600) })
	h, _ := newMissionHost(t, nil)
	p := newMissionPeer(t, h.url)

	first := p.post(t, signal("inbox", mPeerID+"/working", "First", "x", "task-first", clock.now().Add(lease)))
	require.Eventually(t, func() bool { return len(helperRuns(t, "child")) == 1 }, 20*time.Second, 20*time.Millisecond,
		"the first session is running")
	dropped := p.post(t, signal("inbox", mPeerID+"/working", "Dropped", "x", "task-dropped", clock.now().Add(lease)))
	acked := p.post(t, signal("inbox", mPeerID+"/working", "Acked", "x", "task-acked", clock.now().Add(lease)))
	p.ri.QuiesceTriggersForTest(t)
	require.Equal(t, 1, firesOf(t, p, "wake", first, store.TriggerOutcomeStarted))
	require.Equal(t, 1, firesOf(t, p, "wake", dropped, store.TriggerOutcomeBusy), "the wake was dropped: no queue")
	require.Equal(t, 1, firesOf(t, p, "wake", acked, store.TriggerOutcomeBusy))

	// A draining session takes one of them and acknowledges it.
	active, isErr, text := p.take(t, acked, mPeerID, clock.now().Add(time.Hour))
	require.False(t, isErr, text)
	p.ack(t, active, "task-acked")

	require.NoError(t, os.WriteFile(block, nil, 0o600))
	waitRecipeDone(t, p, "wake", first)

	clock.add(lease + time.Second)
	tick(t, p)
	require.Eventually(t, func() bool {
		return firesOf(t, p, "lease", dropped, store.TriggerOutcomeStarted) == 1
	}, 20*time.Second, 20*time.Millisecond, "the lease started a session for the dropped copy")
	require.Eventually(t, func() bool { _, ok := sessionFor(t, dropped); return ok }, 20*time.Second, 20*time.Millisecond)
	waitRecipeDone(t, p, "lease", dropped)
	for _, f := range p.fires(t) {
		if f.Trigger == "lease" {
			require.Equal(t, dropped, f.Path, "only the dropped copy's lease fired: %+v", f)
		}
	}
	requireNoFailedFires(t, p)
}

// TestMission_ActiveCopyLockAndLease: the take is a lock — the same move a
// second time is refused, so two sessions never work one copy — and a copy a
// session took and never acknowledged (it died) fires at ITS lease, and is
// worked again; once acknowledged it never fires.
//
// SABOTAGE: the `lease` trigger matching only working/ → the dead session's
// active copy never fires → red; a take that does not retract the queued
// copy → the second take is not refused → red.
func TestMission_ActiveCopyLockAndLease(t *testing.T) {
	clock := newMissionClock(t)
	h, _ := newMissionHost(t, nil)
	p := newMissionPeer(t, h.url)
	queued := p.post(t, signal("inbox", mPeerID+"/working", "Queued", "x", "task-locked", clock.now().Add(lease)))
	p.ri.QuiesceTriggersForTest(t)
	waitRecipeDone(t, p, "wake", queued)

	active, isErr, text := p.take(t, queued, mPeerID, clock.now().Add(time.Minute))
	require.False(t, isErr, text)
	_, isErr, _ = p.take(t, queued, mPeerID, clock.now().Add(time.Minute))
	require.True(t, isErr, "a second session's take of the same copy is refused")
	require.Equal(t, []string{active}, p.paths(t, p.branch, "kb/inbox/"), "one copy, the taker's active one")
	p.ri.QuiesceTriggersForTest(t)
	require.Zero(t, firesOf(t, p, "wake", active, store.TriggerOutcomeStarted), "a take does not wake a session")

	// The taker dies: its active copy's lease runs out.
	clock.add(time.Minute + time.Second)
	tick(t, p)
	require.Eventually(t, func() bool {
		return firesOf(t, p, "lease", active, store.TriggerOutcomeStarted) == 1
	}, 20*time.Second, 20*time.Millisecond, "the dead session's copy wakes a session")
	require.Eventually(t, func() bool { _, ok := sessionFor(t, active); return ok }, 20*time.Second, 20*time.Millisecond)
	waitRecipeDone(t, p, "lease", active)

	p.ack(t, active, "task-locked")
	clock.add(2 * time.Hour)
	tick(t, p)
	require.Equal(t, 1, firesOf(t, p, "lease", active, store.TriggerOutcomeStarted), "an acknowledged copy never fires again")
	requireNoFailedFires(t, p)
}

// ---- capacity

// TestMission_DecideRechecksCapacity: H ranks first for a task and claims it
// with room to spare; before its decide, its backlog fills (two assigned
// copies). At the decide H withdraws instead of taking, and P, next by rank,
// takes at its next decide. H never holds more than CAPACITY copies.
//
// SABOTAGE: decide without the re-check → H takes a third copy → red.
func TestMission_DecideRechecksCapacity(t *testing.T) {
	clock := newMissionClock(t)
	h, url := newMissionHost(t, nil)
	p := newMissionPeer(t, url)
	capacity, err := strconv.Atoi(jsVar(t, templateFiles(t)[".knomit/triggers/claims.js"], "CAPACITY"))
	require.NoError(t, err)

	id := taskIDWonBy(t, mHostID)
	taskPath := postTask(t, h, clock, id, "Write the migration notes.")
	h.ri.QuiesceTriggersForTest(t)
	exchange(t, h, p)
	require.Len(t, h.paths(t, h.branch, "kb/claims/"+id+"/"), 2, "both claimed while H had room")

	for i := 0; i < capacity; i++ {
		h.post(t, signal("inbox", mHostID+"/working", "Filler", "x", fmt.Sprintf("task-filler-%d", i), clock.now().Add(lease)))
	}
	h.ri.QuiesceTriggersForTest(t)

	for i := 0; i < 2; i++ {
		clock.add(window + time.Second)
		tick(t, h, p)
		exchange(t, h, p)
	}
	exchange(t, h, p)

	for _, at := range branchesOf(t, h, p) {
		all := at.n.paths(t, at.branch, "kb/")
		require.NotContains(t, all, taskPath, at.where)
		var mine []string
		for _, w := range workingCopies(all) {
			if strings.Split(w, "/")[2] == mHostID {
				mine = append(mine, w)
			}
		}
		require.Len(t, mine, capacity, "%s: H holds only its full backlog: %v", at.where, all)
		require.Empty(t, at.n.paths(t, at.branch, "kb/claims/"), at.where)
	}
	var taken []string
	for _, w := range createdWorkingCopies(singleParentChanges(t, historyOf(t, url))) {
		if !strings.Contains(w, "/"+mHostID+"/") {
			taken = append(taken, w)
		}
	}
	require.Len(t, taken, 1, "the task was taken once, by P")
	require.Equal(t, mPeerID, strings.Split(taken[0], "/")[2])
	requireNoFailedFires(t, h, p)
	requireNoRefusal(t, h)
}

// ---- review follow-ups (N1, N2, N3)

// TestMission_DupCheckSeesActiveCopy is the case dup-check was widened to
// `inbox/**` for: a double take where the higher-ranked taker (P) moves its
// copy into active/ BEFORE the other machine (H) ever sees its working/ copy.
// H then only ever sees a learn under active/, and only H's own dup-check can
// make H back off. After the exchanges one copy remains everywhere: P's
// active one.
//
// SABOTAGE: dup-check matching `inbox/*/working/**` again → H never fires on
// P's active copy and keeps its working copy → red.
func TestMission_DupCheckSeesActiveCopy(t *testing.T) {
	clock := newMissionClock(t)
	h, url := newMissionHost(t, nil)
	p := newMissionPeer(t, url)
	id := taskIDWonBy(t, mPeerID)
	postTask(t, h, clock, id, "Sweep the stale branches.")
	h.ri.QuiesceTriggersForTest(t)
	h.advance(t)
	p.sync(t) // P claims, but does not push: H decides on its own claim alone
	clock.add(window + time.Second)
	tick(t, h, p)
	hWC := workingCopies(h.paths(t, h.branch, "kb/"))
	pWC := workingCopies(p.paths(t, p.branch, "kb/"))
	require.Len(t, hWC, 1, "H took")
	require.Len(t, pWC, 1, "P took")
	require.Equal(t, mPeerID, strings.Split(pWC[0], "/")[2])

	// P's session takes its copy before anything reaches H.
	waitRecipeDone(t, p, "wake", pWC[0])
	active, isErr, text := p.take(t, pWC[0], mPeerID, clock.now().Add(time.Hour))
	require.False(t, isErr, text)
	p.ri.QuiesceTriggersForTest(t)

	for i := 0; i < 3; i++ {
		exchange(t, h, p)
	}
	for _, at := range branchesOf(t, h, p) {
		require.Equal(t, []string{active}, at.n.paths(t, at.branch, "kb/inbox/"),
			"%s: the lower-ranked holder backed off; P's active copy stays", at.where)
	}
	require.Positive(t, firesOf(t, h, "dup-check", active, store.TriggerOutcomeRan),
		"H's dup-check ran on P's active copy, the only copy of P's it ever saw")
	for _, f := range h.fires(t) {
		require.NotEqual(t, pWC[0], f.Path, "H never saw P's working copy: %+v", f)
	}
	requireNoFailedFires(t, h, p)
	requireNoRefusal(t, h)
}

// runRecipeStub runs the SHIPPED recipe in a plain goja runtime with stub
// globals: the woken copy is live or not, the queue holds a copy or not. It
// returns the recipe's result and the programs it touched, in order
// ("update" for the lease re-arm, then argv[0] of each exec).
func runRecipeStub(t *testing.T, live, queued bool) (map[string]any, []string) {
	t.Helper()
	vm := goja.New()
	_, err := vm.RunString(fmt.Sprintf(`
var calls = [];
var task_path = "kb/inbox/a-1/working/x.md";
var agent = {id: "a-1"};
var change = {commit: "0123456789abcdef0123456789abcdef01234567", trace: "task-x"};
var run = {id: "run-0123456789abcdef0123456789abcdef"};
var fact = {entities: ["task-x"]};
var mcp = {server: "knomit-repo-m", config: JSON.stringify({mcpServers: {"knomit-repo-m": {command: "kb", args: ["--repo", "m"]}}})};
var LIVE = %v, QUEUED = %v;
var knomit = {
  query: function (a) {
    if (a.path === task_path) { return {facts: LIVE ? [{file: task_path}] : []}; }
    if (a.path === "kb/inbox/a-1/working/") { return {facts: QUEUED ? [{file: "kb/inbox/a-1/working/y.md"}] : []}; }
    return {facts: []};
  },
  update: function (p, u) { calls.push("update"); if (!LIVE) { throw new Error("gone"); } },
  exec: function (argv, o) {
    calls.push(argv[0]);
    if (argv[0] === "mktemp") { return {exit: 0, stdout: "/tmp/session\n", stderr: ""}; }
    return {exit: 0, stdout: "", stderr: ""};
  }
};`, live, queued))
	require.NoError(t, err)
	v, err := vm.RunString(templateFiles(t)[".knomit/recipes/work-task.js"])
	require.NoError(t, err)
	res, ok := v.Export().(map[string]any)
	require.True(t, ok, "the recipe ends with its result object: %v", v)
	var calls []string
	require.NoError(t, vm.ExportTo(vm.Get("calls"), &calls))
	return res, calls
}

// TestMissionTemplate_RecipeStartsOnlyWhenQueued pins the recipe's one
// branch, both ways: no session when the woken copy is gone AND the queue is
// empty; a session whenever either holds work — including a gone copy with a
// queue behind it (a session still draining took the copy, but more work
// waits).
//
// SABOTAGE: skip claude whenever the woken copy is gone, queue or not → red;
// start claude even with nothing queued → red.
func TestMissionTemplate_RecipeStartsOnlyWhenQueued(t *testing.T) {
	res, calls := runRecipeStub(t, false, false)
	require.Equal(t, "nothing queued", res["message"])
	require.Empty(t, calls, "no update, no mktemp, no claude")

	for _, c := range []struct{ live, queued bool }{{false, true}, {true, false}, {true, true}} {
		res, calls := runRecipeStub(t, c.live, c.queued)
		require.Equal(t, "done", res["status"], "%+v", c)
		require.Equal(t, []string{"update", "mktemp", "claude", "rm"}, calls, "%+v: re-arm, folder, session, cleanup", c)
	}
}

// TestMission_BacklogCountsActiveCopies: a machine whose backlog is full of
// copies its sessions already took (all in active/, none in working/) neither
// bids for an offer nor claims a task.
//
// SABOTAGE: bid (or offer) counting working/ only → P bids (or claims) → red.
func TestMission_BacklogCountsActiveCopies(t *testing.T) {
	clock := newMissionClock(t)
	h, _ := newMissionHost(t, nil)
	p := newMissionPeer(t, h.url)
	capacity, err := strconv.Atoi(jsVar(t, templateFiles(t)[".knomit/triggers/awards.js"], "CAPACITY"))
	require.NoError(t, err)
	for i := 0; i < capacity; i++ {
		w := p.post(t, signal("inbox", mPeerID+"/working", "Busy", "x", fmt.Sprintf("task-busy-%d", i), clock.now().Add(lease)))
		p.ri.QuiesceTriggersForTest(t)
		waitRecipeDone(t, p, "wake", w)
		_, isErr, text := p.take(t, w, mPeerID, clock.now().Add(time.Hour))
		require.False(t, isErr, text)
	}
	require.Empty(t, p.paths(t, p.branch, "kb/inbox/"+mPeerID+"/working/"), "the queue is empty")
	require.Len(t, p.paths(t, p.branch, "kb/inbox/"+mPeerID+"/active/"), capacity, "the backlog is all active")

	p.post(t, signal("offers", mHostID+"/lane-a", "Offer", "Review it.", "task-offer-full", clock.now().Add(time.Hour)))
	postTask(t, p, clock, "task-claim-full", "Claim it.")
	p.ri.QuiesceTriggersForTest(t)
	require.Empty(t, p.paths(t, p.branch, "kb/bids/"), "a full backlog does not bid")
	require.Empty(t, p.paths(t, p.branch, "kb/claims/"), "a full backlog does not claim")
	requireNoFailedFires(t, p)
}
