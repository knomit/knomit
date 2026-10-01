package repos_test

// F08 PR D, the rest of the mission e2e (the harness is mission_e2e_test.go).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/mcp"
	"knomit/internal/platform/fileuri"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// ---- T-D0: the shipped template's settings, and that it loads as-is

// TestMissionTemplate_Settings pins what the template must SAY, which no
// behaviour test can: under `consensus: auto` an absent `conflicts` reads
// facts: merge / state: consensus anyway (F20's auto default), so dropping
// the block would turn nothing else red. The file must write it out.
// Every signal topic is learn_dedup: off (T-D4's static half: a dedup merge
// would fold a second claim into the first and drop its expires, so its
// timer would never arm). The claim window the e2e assumes is the shipped
// one. No template file names a branch: the consensus branch is whatever the
// repo's is.
//
// The `sync` object is written out with both keys realtime (F21 S2), and no
// trigger is `do: push`: realtime push already sends every commit.
//
// SABOTAGE: delete the `conflicts:` block → red on the explicit keys; delete
// `learn_dedup: off` under claims → red; delete the `sync:` block, or set
// push or pull to interval → red; re-add a `do: push` trigger → red.
func TestMissionTemplate_Settings(t *testing.T) {
	files := templateFiles(t)
	raw := []byte(files[".knomit/ontology.yaml"])

	o, err := fact.ParseNewOntology(raw)
	require.NoError(t, err, "the shipped ontology parses as a NEW ontology (bad values are fatal there)")

	var doc struct {
		Attributes map[string]any `yaml:"attributes"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &doc))
	require.Equal(t, "auto", doc.Attributes["consensus"], "the template sets consensus: auto")
	conflicts, ok := doc.Attributes["conflicts"].(map[string]any)
	require.True(t, ok, "the template writes the conflicts object out: %v", doc.Attributes)
	require.Equal(t, map[string]any{"facts": "merge", "state": "consensus"}, conflicts)

	cs, err := fact.ReadConsensus(raw)
	require.NoError(t, err)
	require.True(t, cs.Valid)
	require.Equal(t, fact.ConsensusAuto, cs.Mode)
	cf, err := fact.ReadConflicts(raw)
	require.NoError(t, err)
	require.True(t, cf.Valid)
	require.Equal(t, fact.ConflictsMerge, cf.Facts)
	require.Equal(t, fact.ConflictsConsensus, cf.State)

	// `sync` is written out, both keys realtime: the claim window below is
	// derived from it (TestMissionTemplate_TimingRule), and an absent or
	// interval key would put a claim's travel back at the 300 s round.
	syncObj, ok := doc.Attributes["sync"].(map[string]any)
	require.True(t, ok, "the template writes the sync object out: %v", doc.Attributes)
	require.Equal(t, map[string]any{"push": "realtime", "pull": "realtime"}, syncObj)
	sy, err := fact.ReadSync(raw)
	require.NoError(t, err)
	require.True(t, sy.Valid)
	require.True(t, sy.RealtimePush(), "sync.push is realtime")
	require.True(t, sy.RealtimePull(), "sync.pull is realtime")

	for _, topic := range []string{"tasks", "claims", "inbox", "acks", "offers", "bids", "awards"} {
		require.True(t, o.LearnDedupOff(topic), "%s must be learn_dedup: off", topic)
	}

	// Realtime push replaces every `do: push` trigger; one re-added would be
	// redundant, and the count the load tests expect would no longer hold.
	var trig struct {
		Topics map[string]struct {
			Triggers []struct {
				Name string `yaml:"name"`
				Do   string `yaml:"do"`
			} `yaml:"triggers"`
		} `yaml:"topics"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &trig))
	triggers := 0
	for topic, tp := range trig.Topics {
		for _, tr := range tp.Triggers {
			triggers++
			require.NotEqual(t, "push", tr.Do, "%s/%s is a do: push trigger; sync: {push: realtime} already sends every commit", topic, tr.Name)
		}
	}
	require.Equal(t, missionTriggers, triggers, "the template declares %d triggers", missionTriggers)

	m := regexp.MustCompile(`(?m)^var WINDOW_SECONDS = (\d+);`).FindStringSubmatch(files[".knomit/triggers/claims.js"])
	require.NotNil(t, m, "claims.js declares WINDOW_SECONDS")
	require.Equal(t, fmt.Sprint(int(window/time.Second)), m[1], "the e2e's window is the shipped one")

	branchWord := regexp.MustCompile(`\b(main|master|trunk|develop)\b`)
	for p, content := range files {
		require.Empty(t, branchWord.FindAllString(content, -1), "%s names a branch; say \"the consensus branch\"", p)
	}
}

// TestMissionTemplate_TimingRule enforces README "The timing rule" on the
// shipped WINDOW_SECONDS, from knomit's own constants rather than from the
// README's numbers:
//
//	floor = N (the default [git].realtime_pull_interval: a claimer sees the
//	          task up to one pull round after another)
//	      + the push countdown (the claim goes out)
//	      + the push countdown again (the host's merge wakes its own round,
//	          which fast-forwards the consensus branch)
//	      + N (one due tick: the decide rides a round, which fetches first)
//
// X must be at least 1.5 × floor (the round times themselves, tens of ms on a
// healthy origin, are the margin's to absorb), and 2X must exceed the
// winner's own due latency, 2N (one round after its expires, plus one retry
// if the script rate cap dropped its decide).
//
// SABOTAGE: WINDOW_SECONDS = 5 (and the e2e's window with it) → red on the
// floor.
func TestMissionTemplate_TimingRule(t *testing.T) {
	m := regexp.MustCompile(`(?m)^var WINDOW_SECONDS = (\d+);`).FindStringSubmatch(templateFiles(t)[".knomit/triggers/claims.js"])
	require.NotNil(t, m, "claims.js declares WINDOW_SECONDS")
	var secs int
	_, err := fmt.Sscan(m[1], &secs)
	require.NoError(t, err)
	x := time.Duration(secs) * time.Second

	n := config.DefaultRealtimePullInterval
	countdown := repos.PushWakeWindowForTest
	floor := n + countdown + countdown + n
	require.Equal(t, 8*time.Second, floor, "the README's arithmetic (3 + 1 + 1 + 3 s) follows knomit's constants")
	require.GreaterOrEqual(t, x, floor*3/2,
		"X = %s is below 1.5 × (pull %s + push countdown %s + host fast-forward %s + due tick %s)", x, n, countdown, countdown, n)
	require.Greater(t, 2*x, 2*n, "the dead-winner threshold 2X must exceed the winner's due latency")
}

// TestMissionTemplate_LoadsAsIs: on a real host, every trigger of the shipped
// template is active (every `script:` resolves, every `js:` compiles), and a
// fact that is not a signal is refused by rule name.
func TestMissionTemplate_LoadsAsIs(t *testing.T) {
	newMissionClock(t)
	h, _ := newMissionHost(t, nil)
	rep, err := h.ri.TriggerReport(context.Background(), 0)
	require.NoError(t, err)
	require.True(t, rep.Enabled, rep.Reason)
	require.Len(t, rep.Triggers, missionTriggers)
	for _, tr := range rep.Triggers {
		require.Equal(t, fact.TriggerActive, tr.State, "%s: %s", tr.Name, tr.Error)
	}
	f := signal("tasks", "lane-a", "Not a signal", "x", "task-x", time.Now().Add(time.Hour))
	f["kind"], f["type"] = "epistemic", "observation"
	_, isErr, text := h.call(t, "learn", map[string]any{"moment_name": "post", "facts": []any{f}})
	require.True(t, isErr)
	require.Contains(t, text, "signal")
}

// ---- T-D2

// TestMission_ExpiryScriptRetracts is T-D2: a task nobody takes before its
// expires is retracted by the inline `expire` script on both instances, and
// the claims made for it are withdrawn at their decide (the task is gone).
//
// SABOTAGE: `expire` matching a lane nothing is posted in (tasks/nothing/**)
// → the task survives its expires → "the task expired" red. (A match outside
// the topic, claims/**, makes the trigger invalid, which is the same red.)
func TestMission_ExpiryScriptRetracts(t *testing.T) {
	clock := newMissionClock(t)
	h, _ := newMissionHost(t, nil)
	p := newMissionPeer(t, h.url)
	id := "task-expiring"
	taskPath := h.post(t, signal("tasks", "lane-a", "Short-lived", "x", id, clock.now().Add(10*time.Second)))
	h.ri.QuiesceTriggersForTest(t)
	exchange(t, h, p)

	// Past the task's expires, before any claim is due: only `expire` acts.
	clock.add(11 * time.Second)
	tick(t, h, p)
	exchange(t, h, p)
	exchange(t, h, p)
	for _, at := range branchesOf(t, h, p) {
		require.Empty(t, at.n.paths(t, at.branch, "kb/tasks/"), "%s: the task expired", at.where)
	}

	clock.add(window)
	tick(t, h, p)
	exchange(t, h, p)
	exchange(t, h, p)
	for _, at := range branchesOf(t, h, p) {
		require.Empty(t, at.n.paths(t, at.branch, "kb/claims/"), "%s: its claims were withdrawn", at.where)
		require.Empty(t, workingCopies(at.n.paths(t, at.branch, "kb/")), "%s: nobody took it", at.where)
	}
	ran := false
	for _, f := range h.fires(t) {
		if f.Trigger == "expire" && f.Path == taskPath && f.Episode == "due" && f.Outcome == store.TriggerOutcomeRan {
			ran = true
		}
	}
	require.True(t, ran, "H's expire script ran on the task")
	requireNoRefusal(t, h)
}

// ---- T-D3

// TestMission_WinnerCrashReoffered is T-D3: the winner (H) crashes inside its
// take (its learn-with-retract fails), so its claim stays and it never takes.
// The loser re-arms each window; once the winner's claim is more than 2X past
// its expires it no longer ranks, the loser takes, and the SAME move retracts
// the dead winner's claim.
//
// SABOTAGE: the loser does not re-arm (the knomit.update in decide replaced
// by a no-op), or retracts its claim on losing → P never decides again →
// nobody takes → "the loser took the re-offered task" red.
func TestMission_WinnerCrashReoffered(t *testing.T) {
	clock := newMissionClock(t)
	h, _ := newMissionHost(t, nil)
	p := newMissionPeer(t, h.url)
	id := taskIDWonBy(t, mHostID)
	taskPath := postTask(t, h, clock, id, "Rotate the keys.")
	h.ri.QuiesceTriggersForTest(t)
	exchange(t, h, p)
	hostClaim := h.paths(t, h.branch, "kb/claims/"+id+"/"+mHostID+"/")
	require.Len(t, hostClaim, 1)

	h.tools.crashTakes.Store(true)
	for i := 0; i < 3; i++ {
		clock.add(window + time.Second)
		tick(t, h, p)
		exchange(t, h, p)
	}
	exchange(t, h, p)

	for _, at := range branchesOf(t, h, p) {
		all := at.n.paths(t, at.branch, "kb/")
		wc := workingCopies(all)
		require.Len(t, wc, 1, "%s: the loser took the re-offered task: %v", at.where, all)
		require.Equal(t, mPeerID, strings.Split(wc[0], "/")[2], at.where)
		require.NotContains(t, all, taskPath, at.where)
		require.Empty(t, at.n.paths(t, at.branch, "kb/claims/"), "%s: the dead winner's claim is gone too", at.where)
	}
	for _, cc := range singleParentChanges(t, historyOf(t, h.url)) {
		for path, ch := range cc.changes {
			if ch == "A" && len(workingCopies([]string{path})) == 1 {
				require.Equal(t, "D", cc.changes[hostClaim[0]], "the re-offer take retracts the dead claim in the same move: %v", cc.changes)
			}
		}
	}
	crashed := false
	for _, f := range h.fires(t) {
		if f.Trigger == "decide" && f.Outcome == store.TriggerOutcomeScriptError && strings.Contains(f.Error, "crashed") {
			crashed = true
		}
	}
	require.True(t, crashed, "the winner's decide failed as the crash says")
	requireNoFailedFires(t, p)
	requireNoRefusal(t, h)
}

// ---- T-D5

// TestMission_DuplicateBackstop is T-D5: a claim seen late. H decides before
// P's claim has reached it, so H takes; P, which ranks first and holds both
// claims, takes too. After the exchanges the dup-check backstop leaves
// exactly one working copy, the higher-ranked claimer's, everywhere.
//
// SABOTAGE: remove the dup-check trigger from the ontology → two working
// copies remain → red.
func TestMission_DuplicateBackstop(t *testing.T) {
	clock := newMissionClock(t)
	h, _ := newMissionHost(t, nil)
	p := newMissionPeer(t, h.url)
	id := taskIDWonBy(t, mPeerID)
	postTask(t, h, clock, id, "Tidy the changelog.")
	h.ri.QuiesceTriggersForTest(t)
	h.advance(t)
	p.sync(t) // P claims, but does not push: H never sees P's claim before deciding
	require.Len(t, p.paths(t, p.branch, "kb/claims/"+id+"/"), 2)

	clock.add(window + time.Second)
	tick(t, h, p)
	require.Len(t, workingCopies(h.paths(t, h.branch, "kb/")), 1, "H took (it saw only its own claim)")
	require.Len(t, workingCopies(p.paths(t, p.branch, "kb/")), 1, "P took (it ranks first)")

	for i := 0; i < 3; i++ {
		exchange(t, h, p)
	}
	for _, at := range branchesOf(t, h, p) {
		wc := workingCopies(at.n.paths(t, at.branch, "kb/"))
		require.Len(t, wc, 1, "%s: the backstop leaves one working copy", at.where)
		require.Equal(t, mPeerID, strings.Split(wc[0], "/")[2], "%s: the higher-ranked claimer keeps it", at.where)
	}
	require.Len(t, createdWorkingCopies(singleParentChanges(t, historyOf(t, h.url))), 2,
		"the double take did happen: the backstop, not the claim window, resolved it")
	requireNoFailedFires(t, h, p)
	requireNoRefusal(t, h)
}

// ---- T-D6

// TestMission_AssignedTaskNeverClaimed is T-D6: a task posted straight into
// P's working queue is never claimed, and wakes P only. The shipped recipe
// starts the (fake) session with the working copy's PATH and the instruction
// to call knomit_skill work-task; the task's text never reaches argv.
//
// SABOTAGE: the recipe's prompt built from the working copy's body (read
// with knomit.explain) → the sentinel reaches argv → red; `wake` matching
// inbox/** → H wakes too → red.
func TestMission_AssignedTaskNeverClaimed(t *testing.T) {
	clock := newMissionClock(t)
	h, _ := newMissionHost(t, nil)
	p := newMissionPeer(t, h.url)
	const sentinel = "SENTINEL-7f3a-do-not-echo"
	path := h.post(t, signal("inbox", mPeerID+"/working", "Assigned to P", "Please: "+sentinel, "task-assigned", time.Time{}))
	h.ri.QuiesceTriggersForTest(t)
	exchange(t, h, p)
	exchange(t, h, p)
	_ = clock

	for _, at := range branchesOf(t, h, p) {
		require.Empty(t, at.n.paths(t, at.branch, "kb/claims/"), "%s: an assigned task is never claimed", at.where)
		require.Equal(t, []string{path}, workingCopies(at.n.paths(t, at.branch, "kb/")), at.where)
	}
	require.Eventually(t, func() bool { return len(claudeRuns(t)) >= 1 }, 20*time.Second, 20*time.Millisecond, "P's wake started a session")
	time.Sleep(200 * time.Millisecond)
	runs := claudeRuns(t)
	require.Len(t, runs, 1, "exactly one session: the assignee's")
	argv := strings.Join(runs[0], " ")
	require.Equal(t, "claude", runs[0][0])
	require.Contains(t, argv, path, "the prompt names the working copy by path")
	require.Contains(t, argv, "knomit_skill")
	require.Contains(t, argv, `"work-task"`)
	require.NotContains(t, argv, sentinel, "the task's text never enters argv")
	for _, f := range h.fires(t) {
		require.NotEqual(t, "wake", f.Trigger, "H never wakes for P's work")
	}
	woke := false
	for _, f := range p.fires(t) {
		if f.Trigger == "wake" && f.Path == path && f.Outcome == store.TriggerOutcomeStarted {
			woke = true
		}
	}
	require.True(t, woke, "P's wake started the recipe")
	requireNoFailedFires(t, h, p)
}

// ---- T-D7

func missionRPC(t *testing.T, srv *mcpserver.MCPServer, ctx context.Context, method string, params any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(params)
	require.NoError(t, err)
	resp := srv.HandleMessage(ctx, json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":%s}`, method, b)))
	raw, err := json.Marshal(resp)
	require.NoError(t, err)
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &env), string(raw))
	require.Nil(t, env.Error, string(raw))
	return env.Result
}

// TestMission_SkillsServed is T-D7: both instances serve the template's
// skills from their consensus branch, as MCP prompts and through the
// knomit_skill tool the sample recipe names.
//
// SABOTAGE: rename .knomit/skills/work-task → red.
func TestMission_SkillsServed(t *testing.T) {
	newMissionClock(t)
	h, _ := newMissionHost(t, nil)
	p := newMissionPeer(t, h.url)
	for _, n := range []*missionNode{h, p} {
		srv := mcp.NewServer("kb", n.m, false, nil)
		ctx := repos.WithBranch(repos.WithRepoInstance(context.Background(), n.ri), n.branch)

		var prompts struct {
			Prompts []struct {
				Name string `json:"name"`
			} `json:"prompts"`
		}
		require.NoError(t, json.Unmarshal(missionRPC(t, srv, ctx, "prompts/list", map[string]any{}), &prompts))
		var names []string
		for _, pr := range prompts.Prompts {
			names = append(names, pr.Name)
		}
		require.Equal(t, []string{"post-task", "work-task"}, names, "%s: prompts/list", n.name)

		var res struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		}
		require.NoError(t, json.Unmarshal(missionRPC(t, srv, ctx, "tools/call",
			map[string]any{"name": "knomit_skill", "arguments": map[string]any{"name": "work-task"}}), &res))
		require.False(t, res.IsError, res.Content)
		var got struct {
			Body string `json:"body"`
		}
		require.NoError(t, json.Unmarshal([]byte(res.Content[0].Text), &got))
		require.Contains(t, got.Body, "retract: [<working copy path>]", "%s: the work-task body", n.name)
	}
}

// ---- T-D8 (after F20): two instances edit the same task

// TestMission_SameTaskEditedMerges replaces the RCA's T-D8 (whose premise, "a
// refused conflict stalls that peer", is false since F20): H edits a task's
// body while P edits its expires. P's sync meets the conflict and, with the
// template's `conflicts: {facts: merge}`, merges the fact field by field and
// records it (Knomit-Merge: <path> strategy=merge). Both edits land on every
// branch, H's merger refuses nothing, and three idle exchanges add no commit.
//
// SABOTAGE: the test's copy of the template with `facts: off` and `state:
// off` → H's merger refuses and no Knomit-Merge trailer names the task → red.
func TestMission_SameTaskEditedMerges(t *testing.T) {
	clock := newMissionClock(t)
	h, _ := newMissionHost(t, nil)
	p := newMissionPeer(t, h.url)
	id := "task-edited"
	taskPath := postTask(t, h, clock, id, "First version.")
	h.ri.QuiesceTriggersForTest(t)
	exchange(t, h, p)
	exchange(t, h, p)

	later := clock.now().Add(3 * time.Hour).UTC().Format(time.RFC3339)
	_, isErr, text := p.call(t, "update", map[string]any{"moment_name": "extend", "file": taskPath,
		"updates": map[string]any{"expires": later}})
	require.False(t, isErr, text)
	_, isErr, text = h.call(t, "update", map[string]any{"moment_name": "reword", "file": taskPath,
		"updates": map[string]any{"body": "Second version, from H."}})
	require.False(t, isErr, text)
	p.ri.QuiesceTriggersForTest(t)
	h.ri.QuiesceTriggersForTest(t)

	for i := 0; i < 2; i++ {
		exchange(t, h, p)
	}
	for _, at := range branchesOf(t, h, p) {
		content, err := at.n.svc(t).Facts().ReadFact(context.Background(), at.branch, taskPath, nil)
		require.NoError(t, err, at.where)
		f, err := fact.ParseFact(taskPath, content.Content)
		require.NoError(t, err)
		require.Contains(t, f.Body, "Second version, from H.", "%s: H's edit landed", at.where)
		require.Equal(t, later, f.Expires, "%s: P's edit landed", at.where)
	}
	merged := false
	for _, c := range historyOf(t, h.url) {
		for _, line := range strings.Split(c.Message, "\n") {
			if strings.HasPrefix(line, store.TrailerMerge+": "+taskPath+" ") && strings.Contains(line, "strategy=merge ") {
				merged = true
			}
		}
	}
	require.True(t, merged, "a Knomit-Merge trailer records the field merge of %s", taskPath)
	requireNoRefusal(t, h)

	before := len(historyOf(t, h.url))
	hTip, pTip := h.agentTip(t), p.agentTip(t)
	for i := 0; i < 3; i++ {
		exchange(t, h, p)
	}
	require.Equal(t, before, len(historyOf(t, h.url)), "three idle exchanges add no commit")
	require.Equal(t, hTip, h.agentTip(t))
	require.Equal(t, pTip, p.agentTip(t))
	requireNoFailedFires(t, h, p)
}

func (n *missionNode) agentTip(t *testing.T) string {
	t.Helper()
	tip, err := n.svc(t).Branches().HeadCommit(context.Background(), n.branch)
	require.NoError(t, err)
	return tip
}

// ---- T-D9: host-awarded, exactly once

// TestMission_HostAwardedExactlyOnce: P posts an offer for H (the awarder) to
// hand out; both instances bid; H's award script runs on BOTH bids and
// exactly one award is ever written, because each award is the move "write
// the award, delete the offer". The winner moves the award into its working
// queue; the losing bid is withdrawn at its expiry by the inline script.
//
// SABOTAGE: the award without {retract: [offerPath]} → the second bid gets a
// second award → "exactly one award" red; `award` matching bids/** (not only
// on the awarder) → P awards too → the same red.
func TestMission_HostAwardedExactlyOnce(t *testing.T) {
	clock := newMissionClock(t)
	h, _ := newMissionHost(t, nil)
	p := newMissionPeer(t, h.url)
	id := "task-offered"
	offer := p.post(t, signal("offers", mHostID+"/lane-a", "Offer "+id, "Review the design.", id, clock.now().Add(time.Hour)))
	p.ri.QuiesceTriggersForTest(t)
	require.Len(t, p.paths(t, p.branch, "kb/bids/"+mHostID+"/"+id+"/"+mPeerID+"/"), 1, "P bid on its own offer")
	for i := 0; i < 3; i++ {
		exchange(t, h, p)
	}

	ccs := singleParentChanges(t, historyOf(t, h.url))
	var awards []string
	var awardCommit commitChange
	for _, cc := range ccs {
		for path, ch := range cc.changes {
			if ch == "A" && strings.HasPrefix(path, "kb/awards/") {
				awards = append(awards, path)
				awardCommit = cc
			}
		}
	}
	require.Len(t, awards, 1, "exactly one award was ever written")
	require.Equal(t, "D", awardCommit.changes[offer], "the award deletes the offer in the same commit")
	require.Len(t, createdWorkingCopies(ccs), 1, "exactly one working copy was ever written")
	winner := strings.Split(awards[0], "/")[2]
	for _, at := range branchesOf(t, h, p) {
		all := at.n.paths(t, at.branch, "kb/")
		require.NotContains(t, all, offer, at.where)
		require.Empty(t, at.n.paths(t, at.branch, "kb/awards/"), "%s: the award moved into the working queue", at.where)
		wc := workingCopies(all)
		require.Len(t, wc, 1, at.where)
		require.Equal(t, winner, strings.Split(wc[0], "/")[2], at.where)
	}

	clock.add(11 * time.Minute) // past the bids' expires
	tick(t, h, p)
	exchange(t, h, p)
	exchange(t, h, p)
	for _, at := range branchesOf(t, h, p) {
		require.Empty(t, at.n.paths(t, at.branch, "kb/bids/"), "%s: the losing bid was withdrawn", at.where)
	}
	requireNoFailedFires(t, h, p)
	requireNoRefusal(t, h)
}

// ---- T-D1g: the GitHub-hosted variant, on a consensus branch named trunk

// gitOrigin is a bare repository standing in for GitHub: its consensus
// branch is advanced only by prMerge, a merge commit made with git itself.
type gitOrigin struct {
	bare, work, url, branch string
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
}

func newGitOrigin(t *testing.T, root, branch string, files map[string]string) *gitOrigin {
	t.Helper()
	bare := filepath.Join(root, "origin.git")
	git(t, "", "init", "--bare", "--initial-branch="+branch, bare)
	work := t.TempDir()
	git(t, "", "clone", bare, work)
	git(t, work, "checkout", "-B", branch)
	for p, content := range files {
		require.NoError(t, os.MkdirAll(filepath.Join(work, filepath.Dir(p)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(work, p), []byte(content), 0o644))
	}
	git(t, work, "add", "-A")
	git(t, work, "commit", "-m", "mission template")
	git(t, work, "push", "origin", branch)
	return &gitOrigin{bare: bare, work: work, url: fileuri.New(bare), branch: branch}
}

// prMerge merges a pushed agent branch the way a PR merge does, unless it
// brings no change.
func (o *gitOrigin) prMerge(t *testing.T, branch string) {
	t.Helper()
	git(t, o.work, "fetch", "origin")
	cmd := exec.Command("git", "diff", "--quiet", "origin/"+o.branch, "origin/"+branch)
	cmd.Dir = o.work
	if cmd.Run() == nil {
		return
	}
	git(t, o.work, "checkout", "-B", o.branch, "origin/"+o.branch)
	git(t, o.work, "merge", "--no-ff", "-m", "Merge pull request from "+branch, "origin/"+branch)
	git(t, o.work, "push", "origin", o.branch)
}

func newGitPeer(t *testing.T, root string, o *gitOrigin, name, agent, signer string) *missionNode {
	t.Helper()
	m, tools := newMissionManager(t, agent, signer, func(c *config.Config) { c.LocalOriginRoot = root })
	ri, err := m.Create(context.Background(), repos.CreateSpec{Name: "kb", Mode: "clone", Origin: &repos.OriginSpec{URL: o.url, Branch: o.branch}}, nil)
	require.NoError(t, err)
	n := &missionNode{name: name, m: m, ri: ri, branch: agent, tools: tools}
	require.Equal(t, o.branch, n.svc(t).UpstreamBranch(), "the consensus branch is the origin's")
	n.ri.QuiesceTriggersForTest(t)
	return n
}

// TestMission_GitHubVariantOnTrunk is T-D1 on the README's GitHub-hosted
// variant: the template with `consensus` removed (the forge owns the
// consensus branch) and `conflicts` kept, on an origin whose consensus
// branch is "trunk". Exactly one of two claimers takes, in one move, and
// only one working copy is ever created. Nothing in the template or the
// protocol names a branch, so a copy that assumed one would go red here.
func TestMission_GitHubVariantOnTrunk(t *testing.T) {
	clock := newMissionClock(t)
	root := t.TempDir()
	files := templateFiles(t)
	ont := files[".knomit/ontology.yaml"]
	require.Equal(t, 1, strings.Count(ont, "\n  consensus: auto\n"))
	files[".knomit/ontology.yaml"] = strings.Replace(ont, "\n  consensus: auto\n", "\n", 1)
	o := newGitOrigin(t, root, "trunk", files)
	a := newGitPeer(t, root, o, "A", mHostAgent, "mission-a")
	b := newGitPeer(t, root, o, "B", mPeerAgent, "mission-b")
	round := func() {
		for _, n := range []*missionNode{a, b} {
			n.sync(t)
			n.push(t)
			o.prMerge(t, n.branch)
		}
	}

	id := taskIDWonBy(t, mPeerID)
	taskPath := postTask(t, a, clock, id, "Port the docs.")
	a.ri.QuiesceTriggersForTest(t)
	round()
	round()
	require.Len(t, a.paths(t, a.branch, "kb/claims/"+id+"/"), 2, "A holds both claims")
	// Three windows: B takes at the first; A (the loser) re-arms at the
	// first and, because a round syncs A before B pushes, again at the
	// second; it sees the take and withdraws at the third.
	for i := 0; i < 3; i++ {
		clock.add(window + time.Second)
		tick(t, a, b)
		round()
	}
	round()

	for _, at := range []struct {
		where  string
		n      *missionNode
		branch string
	}{{"A", a, a.branch}, {"B", b, b.branch}, {"trunk", a, o.branch}} {
		all := at.n.paths(t, at.branch, "kb/")
		wc := workingCopies(all)
		require.Len(t, wc, 1, "%s: %v", at.where, all)
		require.Equal(t, mPeerID, strings.Split(wc[0], "/")[2], at.where)
		require.NotContains(t, all, taskPath, at.where)
		require.Empty(t, at.n.paths(t, at.branch, "kb/claims/"), at.where)
	}
	require.Len(t, createdWorkingCopies(singleParentChanges(t, historyOf(t, o.url))), 1, "only one working copy was ever created")
	requireNoFailedFires(t, a, b)
}
