package repos_test

// F08 PR D: the mission template (examples/mission/) end to end, on two real
// instances. The template is copied from the SHIPPED directory (a walk of
// examples/mission/, never a duplicate), and every trigger script runs
// through the REAL MCP handlers (mcp.NewScriptTools), which is why this file
// is in package repos_test: internal/mcp imports internal/repos.
//
// Topology (knomit-hosted, as the template's ontology says): host H serves
// the repo over an in-process /git with an enrolled peer's push policy, has
// no origin and `consensus: auto`, so its merger merges every branch P
// pushes into H's agent branch. Peer P clones H. Neither runs a background
// loop: the test drives each round by hand, and one fake clock (the
// dispatcher's run clock, which is also the scripts' Date) moves time.
//
// Nothing here spells a branch name for the consensus branch: it is asked of
// the store (UpstreamBranch). The GitHub-variant test runs on "trunk".

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/mcp"
	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/testsupport/testsigner"
)

const (
	mHostAgent = "agent/host-11111111"
	mPeerAgent = "agent/peer-abcd1234"
	mHostID    = "host-11111111"
	mPeerID    = "peer-abcd1234"
	// mPeerFP is the pushing peer's full key fingerprint; its first 8 hex name
	// the only branch it may push (mPeerAgent).
	mPeerFP = "abcd1234" + "00000000000000000000000000000000000000000000000000000000"

	// window is the template's WINDOW_SECONDS (claims.js), read back from the
	// shipped file by TestMissionTemplate_Settings.
	window = 15 * time.Second

	// missionTriggers is how many triggers the shipped template declares.
	// None is `do: push`: the template's `sync: {push: realtime}` sends every
	// commit on the agent branch instead.
	missionTriggers = 11

	// lease is the template's LEASE_SECONDS (claims.js and awards.js): how
	// long a working copy may wait before the `lease` trigger wakes a
	// session for it. Read back from the shipped files by
	// TestMissionTemplate_Lease.
	lease = 300 * time.Second
)

// templateDir is the shipped template, relative to this package.
var templateDir = filepath.Join("..", "..", "examples", "mission")

// templateFiles is every file of the shipped template, keyed by its repo
// path (forward slashes).
func templateFiles(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	require.NoError(t, filepath.WalkDir(templateDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(templateDir, p)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = string(b)
		return nil
	}))
	require.Contains(t, out, ".knomit/ontology.yaml", "the template must ship its ontology")
	return out
}

// ---- the clock

type missionClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *missionClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *missionClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newMissionClock installs the fake clock AND a fake `claude` first on PATH:
// the template's `wake` trigger runs the shipped recipe, which execs claude,
// and no test may start a real session. The fake is this test binary
// (TestMain turns it into the recipe helper when KNOMIT_RECIPE_HELPER is
// set); it records its argv as JSON in the returned directory and exits 0.
func newMissionClock(t *testing.T) *missionClock {
	c := &missionClock{t: time.Now().UTC().Truncate(time.Second)}
	repos.SetTriggerClockForTest(t, c.now)
	fakeClaude(t)
	return c
}

// fakeClaude puts the test binary on PATH as `claude`, and as the two other
// tools the shipped recipe runs, `mktemp` and `rm` (the helper acts by the
// name it was started under), and returns the directory their reports land
// in. Every external program the recipe starts is then the test's own.
func fakeClaude(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	require.NoError(t, err)
	bin := t.TempDir()
	for _, name := range []string{"claude", "mktemp", "rm"} {
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		dst := filepath.Join(bin, name)
		// A hard link is cheap, but on Windows it names the RUNNING test
		// binary, which cannot be deleted, and t.TempDir's cleanup then fails
		// the test. There, and wherever linking fails, copy.
		if runtime.GOOS == "windows" || os.Link(self, dst) != nil {
			src, err := os.Open(self)
			require.NoError(t, err)
			out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
			require.NoError(t, err)
			_, err = io.Copy(out, src)
			require.NoError(t, err)
			require.NoError(t, out.Close())
			require.NoError(t, src.Close())
		}
	}
	reports := t.TempDir()
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("KNOMIT_RECIPE_HELPER", reports)
	return reports
}

// claudeRuns is every argv the fake claude was started with.
func claudeRuns(t *testing.T) [][]string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(os.Getenv("KNOMIT_RECIPE_HELPER"), "child-*.json"))
	require.NoError(t, err)
	var out [][]string
	for _, m := range matches {
		b, err := os.ReadFile(m)
		require.NoError(t, err)
		var rep struct {
			Args []string `json:"args"`
		}
		require.NoError(t, json.Unmarshal(b, &rep), m)
		out = append(out, rep.Args)
	}
	return out
}

// ---- script tools with a crash switch

// crashableTools is the real tool set; while crashTakes is set, every learn
// that retracts something (a take, a dup-check back-off) fails as if the
// machine died mid-decision.
type crashableTools struct {
	real       repos.ScriptTools
	crashTakes atomic.Bool
}

func (c *crashableTools) Call(ctx context.Context, tool string, args map[string]any) (string, bool, error) {
	if tool == "learn" && c.crashTakes.Load() {
		if _, ok := args["retract"]; ok {
			return "", false, fmt.Errorf("crashed")
		}
	}
	return c.real.Call(ctx, tool, args)
}

// ---- instances

type missionNode struct {
	name   string
	m      *repos.Manager
	ri     *repos.RepoInstance
	branch string
	tools  *crashableTools
	url    string // the host's /git URL (hosts only)
}

func newMissionManager(t *testing.T, agent, signer string, extra func(*config.Config)) (*repos.Manager, *crashableTools) {
	t.Helper()
	home := t.TempDir()
	cfg := config.Config{Home: home, OntologyRoot: "kb"}
	cfg.Git.LocalReconcileInterval = time.Minute
	if extra != nil {
		extra(&cfg)
	}
	tools := &crashableTools{real: mcp.NewScriptTools(nil)}
	m := repos.New(context.Background(), repos.Deps{
		Cfg:         cfg,
		AgentBranch: agent,
		KeyPath:     filepath.Join(home, "agent.key"),
		Signer:      testsigner.Named(signer),
		Machine:     repos.Options{Synchronous: true},
		ScriptTools: tools,
	})
	t.Cleanup(func() { _ = m.Close() })
	// What a booted server records once its listeners are bound: the shipped
	// recipe's exec waits for it. The fake claude never dials it.
	m.SetServerAddress("http://127.0.0.1:1")
	require.NoError(t, m.Start())
	return m, tools
}

func (n *missionNode) svc(t *testing.T) *store.Service {
	t.Helper()
	svc, release, err := n.ri.Acquire()
	require.NoError(t, err)
	release()
	return svc
}

// mutate lets a test edit the template before it is installed (the GitHub
// variant, a sabotage-shaped negative case).
type mutate func(files map[string]string)

// newMissionHost boots H with the shipped template on its agent branch and
// its consensus branch, and serves it.
func newMissionHost(t *testing.T, edit mutate) (*missionNode, string) {
	t.Helper()
	m, tools := newMissionManager(t, mHostAgent, "mission-host", nil)
	files := templateFiles(t)
	if edit != nil {
		edit(files)
	}
	// A repo's ontology is fixed when it is created (RepoInstance.Ontology):
	// the create carries the template's, and the rest of the template is
	// committed on top of it.
	ri, err := m.Create(context.Background(), repos.CreateSpec{Name: "kb", Mode: "custom", OntologyYAML: files[".knomit/ontology.yaml"]}, nil)
	require.NoError(t, err)
	h := &missionNode{name: "H", m: m, ri: ri, branch: mHostAgent, tools: tools}
	h.install(t, files)
	h.advance(t)
	h.ri.QuiesceTriggersForTest(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		svc, release, err := ri.Acquire()
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		defer release()
		ctx := store.WithPushPolicy(r.Context(), store.PushPolicy{Pusher: mPeerFP, OwnBranch: ri.AgentBranch()})
		svc.Handler().ServeHTTP(w, r.WithContext(ctx))
	}))
	t.Cleanup(srv.Close)
	h.url = srv.URL
	return h, srv.URL
}

// install commits every other template file on the agent branch. The
// scripts land after the ontology; a script trigger is invalid until its file
// exists and catches up once it does (its bookmark freezes meanwhile).
func (n *missionNode) install(t *testing.T, files map[string]string) {
	t.Helper()
	paths := make([]string, 0, len(files))
	for p := range files {
		if p != ".knomit/ontology.yaml" {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	for _, p := range paths {
		_, err := n.svc(t).Facts().WriteFact(context.Background(), n.branch, p, files[p], "template: "+p, "updated")
		require.NoError(t, err, p)
	}
}

func newMissionPeer(t *testing.T, url string) *missionNode {
	t.Helper()
	m, tools := newMissionManager(t, mPeerAgent, "mission-peer", nil)
	ri, err := m.Create(context.Background(), repos.CreateSpec{Name: "kb", Mode: "clone", Origin: &repos.OriginSpec{URL: url}}, nil)
	require.NoError(t, err)
	p := &missionNode{name: "P", m: m, ri: ri, branch: mPeerAgent, tools: tools}
	p.ri.QuiesceTriggersForTest(t)
	return p
}

// advance is one round of the host's local reconcile: the consensus branch
// follows the agent branch.
func (n *missionNode) advance(t *testing.T) {
	t.Helper()
	svc := n.svc(t)
	_, err := svc.AdvanceLocalUpstream(context.Background(), n.branch, svc.UpstreamBranch())
	require.NoError(t, err)
}

// sync is a peer's fetch-and-merge, then its triggers until quiet.
func (n *missionNode) sync(t *testing.T) {
	t.Helper()
	_, err := n.svc(t).Remote().Sync(context.Background(), n.branch, nil)
	require.NoError(t, err)
	n.ri.QuiesceTriggersForTest(t)
}

func (n *missionNode) push(t *testing.T) {
	t.Helper()
	_, err := n.svc(t).Remote().Push(context.Background(), n.branch, nil)
	require.NoError(t, err)
}

// settle runs the host's consensus merger, then its triggers until quiet.
func (n *missionNode) settle(t *testing.T) {
	t.Helper()
	n.ri.SettleConsensusForTest(t)
	n.ri.QuiesceTriggersForTest(t)
}

// exchange is one full round between H and P: H's consensus branch follows
// its agent branch; P fetches, merges and reacts; P pushes; H merges what P
// pushed and reacts; H's consensus branch follows again.
func exchange(t *testing.T, h, p *missionNode) {
	t.Helper()
	h.ri.QuiesceTriggersForTest(t)
	h.advance(t)
	p.sync(t)
	p.push(t)
	h.settle(t)
	h.advance(t)
}

// tick moves every instance's clock-driven work: one kick each (a due
// sweep at the fake clock), host first.
func tick(t *testing.T, nodes ...*missionNode) {
	t.Helper()
	for _, n := range nodes {
		n.ri.KickTriggersForTest(t)
	}
}

// ---- MCP calls, as a session on that instance makes them

func (n *missionNode) call(t *testing.T, tool string, args map[string]any) (map[string]any, bool, string) {
	t.Helper()
	ctx := repos.WithBinding(context.Background(), repos.NewBindingOfRepo(n.ri, n.branch))
	text, isErr, err := n.tools.real.Call(ctx, tool, args)
	require.NoError(t, err)
	var out map[string]any
	if !isErr {
		require.NoError(t, json.Unmarshal([]byte(text), &out), text)
	}
	return out, isErr, text
}

// signal is a template-shaped signal fact.
func signal(topic, category, title, body, id string, expires time.Time) map[string]any {
	f := map[string]any{
		"topic": topic, "category": category, "kind": "pragmatic", "type": "signal",
		"title": title, "body": body, "entities": []any{id}, "confidence": 1, "sources": 1,
	}
	if !expires.IsZero() {
		f["expires"] = expires.UTC().Format(time.RFC3339)
	}
	return f
}

// post learns one fact through the real knomit_learn and returns its path.
func (n *missionNode) post(t *testing.T, f map[string]any) string {
	t.Helper()
	out, isErr, text := n.call(t, "learn", map[string]any{"moment_name": "post", "facts": []any{f}})
	require.False(t, isErr, text)
	commits := out["commits"].([]any)
	require.Len(t, commits, 1)
	return commits[0].(map[string]any)["file"].(string)
}

// ---- reading state

// paths is every fact path under prefix on branch.
func (n *missionNode) paths(t *testing.T, branch, prefix string) []string {
	t.Helper()
	all, err := n.svc(t).Facts().ListAll(context.Background(), branch)
	require.NoError(t, err)
	var out []string
	for _, p := range all {
		if strings.HasPrefix(p, prefix) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func workingCopies(paths []string) []string {
	var out []string
	for _, p := range paths {
		if s := strings.Split(p, "/"); len(s) > 3 && s[1] == "inbox" && s[3] == "working" {
			out = append(out, p)
		}
	}
	return out
}

// branchesOf is every place a converged mission state must be the same: H's
// agent branch, H's consensus branch, P's agent branch.
func branchesOf(t *testing.T, h, p *missionNode) []struct {
	where  string
	n      *missionNode
	branch string
} {
	return []struct {
		where  string
		n      *missionNode
		branch string
	}{{"H agent", h, h.branch}, {"H consensus", h, h.svc(t).UpstreamBranch()}, {"P agent", p, p.branch}}
}

// historyOf is every commit reachable from any branch the host serves (the
// peer's pushed branch included), read through a fresh clone.
func historyOf(t *testing.T, url string) map[plumbing.Hash]*object.Commit {
	t.Helper()
	repo, err := gogit.Clone(memory.NewStorage(), nil, &gogit.CloneOptions{URL: url})
	require.NoError(t, err)
	out := map[plumbing.Hash]*object.Commit{}
	it, err := repo.CommitObjects()
	require.NoError(t, err)
	require.NoError(t, it.ForEach(func(c *object.Commit) error { out[c.Hash] = c; return nil }))
	return out
}

// commitChange is one non-merge commit's changes: path → "A" | "D" | "M".
type commitChange struct {
	hash    plumbing.Hash
	message string
	changes map[string]string
}

// singleParentChanges lists every non-merge commit with its changes against
// its parent. A fact is CREATED by exactly one such commit; merges only
// carry it.
func singleParentChanges(t *testing.T, all map[plumbing.Hash]*object.Commit) []commitChange {
	t.Helper()
	var out []commitChange
	for _, c := range all {
		if c.NumParents() != 1 {
			continue
		}
		parent, err := c.Parent(0)
		require.NoError(t, err)
		pt, err := parent.Tree()
		require.NoError(t, err)
		ct, err := c.Tree()
		require.NoError(t, err)
		diff, err := object.DiffTree(pt, ct)
		require.NoError(t, err)
		cc := commitChange{hash: c.Hash, message: c.Message, changes: map[string]string{}}
		for _, ch := range diff {
			switch {
			case ch.From.Name == "":
				cc.changes[ch.To.Name] = "A"
			case ch.To.Name == "":
				cc.changes[ch.From.Name] = "D"
			default:
				cc.changes[ch.To.Name] = "M"
			}
		}
		out = append(out, cc)
	}
	return out
}

// createdWorkingCopies is every working copy any commit ever created.
func createdWorkingCopies(ccs []commitChange) []string {
	var out []string
	for _, cc := range ccs {
		for p, ch := range cc.changes {
			if ch == "A" && len(workingCopies([]string{p})) == 1 {
				out = append(out, p)
			}
		}
	}
	sort.Strings(out)
	return out
}

// requireNoRefusal asserts H's consensus merger merged and never refused.
func requireNoRefusal(t *testing.T, h *missionNode) {
	t.Helper()
	c, ok := h.ri.ConsensusForTest()
	require.True(t, ok, "H must run a consensus merger (consensus: auto, no origin)")
	require.Zero(t, c.Refused, "zero refused consensus merges: %v", c.Warnings)
	require.Positive(t, c.Merges, "H merged what P pushed")
}

func (n *missionNode) fires(t *testing.T) []store.TriggerFire {
	t.Helper()
	rows, err := n.svc(t).Triggers().RecentTriggerFires(context.Background(), n.branch, 1000)
	require.NoError(t, err)
	return rows
}

// requireNoFailedFires asserts no trigger of the template failed on any node:
// a script that threw, timed out or was dropped, a recipe that errored.
func requireNoFailedFires(t *testing.T, nodes ...*missionNode) {
	t.Helper()
	for _, n := range nodes {
		for _, f := range n.fires(t) {
			switch f.Outcome {
			case store.TriggerOutcomeScriptError, store.TriggerOutcomeScriptTimeout, store.TriggerOutcomeRateLimited,
				store.TriggerOutcomeIfError, store.TriggerOutcomeRecipeError, store.TriggerOutcomeRecipeTimeout:
				t.Fatalf("%s: trigger %s on %s: %s: %s", n.name, f.Trigger, f.Path, f.Outcome, f.Error)
			}
		}
	}
}

// ---- T-D1

// postTask posts a generally available task on n.
func postTask(t *testing.T, n *missionNode, clock *missionClock, id, body string) string {
	t.Helper()
	return n.post(t, signal("tasks", "lane-a", "Task "+id, body, id, clock.now().Add(time.Hour)))
}

// TestMission_OneTaskTwoClaimersOneTakes is T-D1: one task posted on H; both
// instances claim it; after the claim window each decides at its own due;
// exactly one takes. Run for a task id H wins and one P wins (the rank is the
// shipped script's), so neither instance is special.
//
// Asserted: the take is ONE commit that adds the working copy and deletes the
// task and the taker's claim (the atomic move); only ONE working copy was ever
// created, in any commit on either instance (so the backstop cannot be hiding
// a double take); after the exchanges the task and every claim are gone and
// the same single working copy is on H's agent and consensus branches and on
// P; H's merger merged and refused nothing.
//
// SABOTAGE: decide picks itself (`var winner = agent.id;`), dup-check left
// intact → both take, the backstop cleans up so ONE copy remains, and "only
// ONE working copy was ever created" goes red. The take split into a learn
// and a separate retract → "the take deletes the task in the same commit"
// red. The take without its retract → "the task is gone" red.
func TestMission_OneTaskTwoClaimersOneTakes(t *testing.T) {
	for _, c := range []struct {
		name, winner string
	}{{"host wins", mHostID}, {"peer wins", mPeerID}} {
		t.Run(c.name, func(t *testing.T) {
			clock := newMissionClock(t)
			h, url := newMissionHost(t, nil)
			p := newMissionPeer(t, url)
			id := taskIDWonBy(t, c.winner)

			taskPath := postTask(t, h, clock, id, "Write the release notes.")
			h.ri.QuiesceTriggersForTest(t)
			require.Len(t, h.paths(t, h.branch, "kb/claims/"+id+"/"+mHostID+"/"), 1, "H claimed its own task")
			exchange(t, h, p)
			require.Len(t, p.paths(t, p.branch, "kb/claims/"+id+"/"+mPeerID+"/"), 1, "P claimed the task it fetched")
			require.Len(t, h.paths(t, h.branch, "kb/claims/"+id+"/"), 2, "H holds both claims before any decide")

			clock.add(window + time.Second)
			tick(t, h, p)
			exchange(t, h, p)
			clock.add(window + time.Second)
			tick(t, h, p)
			exchange(t, h, p)
			exchange(t, h, p)

			var working string
			for _, at := range branchesOf(t, h, p) {
				all := at.n.paths(t, at.branch, "kb/")
				wc := workingCopies(all)
				require.Len(t, wc, 1, "%s: exactly one working copy: %v", at.where, all)
				require.Equal(t, c.winner, strings.Split(wc[0], "/")[2], "%s: the winner by rank holds it", at.where)
				if working == "" {
					working = wc[0]
				}
				require.Equal(t, working, wc[0], "%s: the same working copy everywhere", at.where)
				require.NotContains(t, all, taskPath, "%s: the task is gone", at.where)
				require.Empty(t, at.n.paths(t, at.branch, "kb/claims/"), "%s: every claim is gone", at.where)
			}

			ccs := singleParentChanges(t, historyOf(t, url))
			require.Equal(t, []string{working}, createdWorkingCopies(ccs), "only ONE working copy was ever created")
			var take *commitChange
			for i := range ccs {
				if ccs[i].changes[working] == "A" {
					take = &ccs[i]
				}
			}
			require.NotNil(t, take)
			require.Equal(t, "D", take.changes[taskPath], "the take deletes the task in the same commit: %v", take.changes)
			var ownClaimDeleted bool
			for path, ch := range take.changes {
				if strings.HasPrefix(path, "kb/claims/"+id+"/"+c.winner+"/") && ch == "D" {
					ownClaimDeleted = true
				}
			}
			require.True(t, ownClaimDeleted, "the take deletes the taker's claim in the same commit: %v", take.changes)
			require.Len(t, take.changes, 3, "working copy + task + own claim, nothing else: %v", take.changes)

			requireNoRefusal(t, h)
			requireNoFailedFires(t, h, p)
			// The winner's wake started one session, for its working copy.
			require.Eventually(t, func() bool { return len(claudeRuns(t)) == 1 }, 20*time.Second, 20*time.Millisecond)
			require.Contains(t, strings.Join(claudeRuns(t)[0], " "), working)
			// #349: the prompt hands the session the task's trace — the task
			// id, which the claims script's take carried forward — to pass on
			// every write, inside its JSON context literal.
			trace := contextOf(t, claudeRuns(t)[0]).Trace
			require.Equal(t, id, trace["Knomit-Trace"], "the session's trace is the task id")
			require.Regexp(t, `^run-[0-9a-f]{32}$`, trace["Knomit-Run"])
			require.Regexp(t, `^[0-9a-f]{40}$`, trace["Knomit-Cause"])
		})
	}
}
