package repos

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/platform/fileuri"
	"knomit/internal/store"
	"knomit/internal/testsupport/testsigner"
)

// ---- Fixture (F07 PR 3, `do: script`)
//
// The 1b fixture plus a STUB ScriptTools: it writes THROUGH the store with the
// ctx the host built — the binding names the branch, the trailers ride the ctx
// into the builders — so what these tests prove is the host's own contract
// (the ctx it hands a tool, the refusals, the guard, the cap, the outcomes).
// The handler half (validation, refs gate, CAS, the private path through the
// real tool) is internal/mcp's, and the real mcp.NewScriptTools wired into a
// Manager is proven end to end in internal/web.

type stubCall struct {
	Tool string
	Args map[string]any
}

type stubTools struct {
	ri *RepoInstance
	mu sync.Mutex
	// calls is every Call the host made, in order.
	calls []stubCall
	// hook, when set, runs first and may take the call over (handled=true).
	hook func(ctx context.Context, tool string, args map[string]any) (text string, isErr bool, err error, handled bool)
}

func (s *stubTools) Call(ctx context.Context, tool string, args map[string]any) (string, bool, error) {
	s.mu.Lock()
	s.calls = append(s.calls, stubCall{Tool: tool, Args: args})
	hook := s.hook
	s.mu.Unlock()
	if hook != nil {
		if text, isErr, err, handled := hook(ctx, tool, args); handled {
			return text, isErr, err
		}
	}
	b, err := RequireBinding(ctx)
	if err != nil {
		return "", false, err
	}
	branch := b.WriteBranch()
	svc, release, err := s.ri.Acquire()
	if err != nil {
		return "knowledge base is unavailable", true, nil
	}
	defer release()
	moment, _ := args["moment_name"].(string)
	switch tool {
	case "query":
		out, _ := json.Marshal(map[string]any{"facts": []any{}, "echo": args})
		return string(out), false, nil
	case "explain":
		out, _ := json.Marshal(map[string]any{"file": args["file"], "commit": args["commit"]})
		return string(out), false, nil
	case "learn":
		facts, _ := args["facts"].([]any)
		var commits []map[string]any
		for _, f := range facts {
			m := f.(map[string]any)
			title, _ := m["title"].(string)
			path := fmt.Sprintf("kb/%s/%s/%s.md", m["topic"], m["category"], strings.ToLower(strings.ReplaceAll(title, " ", "-")))
			body := factBody(title)
			if exp, ok := m["expires"].(string); ok {
				body = datedBody(title, exp)
			}
			r, err := svc.Facts().WriteFact(ctx, branch, path, body, "learn: "+moment, "learn")
			if err != nil {
				return err.Error(), true, nil
			}
			commits = append(commits, map[string]any{"file": path, "hash": r.CommitHash})
		}
		out, _ := json.Marshal(map[string]any{"commits": commits, "written_to": branch})
		return string(out), false, nil
	case "update":
		file, _ := args["file"].(string)
		updates, _ := args["updates"].(map[string]any)
		title, _ := updates["title"].(string)
		if title == "" {
			title = file + " v2"
		}
		body := factBody(title)
		if exp, ok := updates["expires"].(string); ok {
			body = datedBody(title, exp)
		}
		r, err := svc.Facts().WriteFact(ctx, branch, file, body, "update: "+moment, "update")
		if err != nil {
			return err.Error(), true, nil
		}
		out, _ := json.Marshal(map[string]any{"file": file, "commit": r.CommitHash})
		return string(out), false, nil
	case "retract":
		file, _ := args["file"].(string)
		h, err := svc.Facts().DeleteFact(ctx, branch, file, "retract("+moment+"): "+file)
		if err != nil {
			return err.Error(), true, nil
		}
		out, _ := json.Marshal(map[string]any{"file": file, "commit": h})
		return string(out), false, nil
	}
	return "", false, fmt.Errorf("stub: unknown tool %s", tool)
}

func (s *stubTools) recorded() []stubCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]stubCall(nil), s.calls...)
}

// newScriptRepo boots a repo whose dispatcher has the stub tools and the given
// per-minute cap (0 = the built-in default), with the triggers installed.
func newScriptRepo(t *testing.T, rate int, entries ...string) (*Manager, *RepoInstance, *stubTools) {
	t.Helper()
	home := t.TempDir()
	cfg := config.Config{Home: home, OntologyRoot: "kb"}
	cfg.Triggers.ScriptRatePerMinute = rate
	tools := &stubTools{}
	m := New(context.Background(), Deps{Cfg: cfg, AgentBranch: trigAgent,
		KeyPath: filepath.Join(home, "agent.key"), DisableBackgroundSync: true, ScriptTools: tools})
	t.Cleanup(func() { _ = m.Close() })
	ri := bootRepo(t, m)
	tools.ri = ri
	setOntology(t, ri, triggerOntology("", entries...))
	return m, ri, tools
}

// scriptTrig renders a `do: script` entry.
func scriptTrig(name, on, match, script string) string {
	e := fmt.Sprintf("      - name: %s\n        on: %s\n        do: script\n        script: %s\n", name, on, script)
	if match != "" {
		e += fmt.Sprintf("        match: %q\n", match)
	}
	return e
}

// putScript commits .knomit/triggers/<name>.js on the agent branch (repo
// content, like an operator's commit) and waits for the run that reloads it.
func putScript(t *testing.T, ri *RepoInstance, name, src string) string {
	t.Helper()
	r, err := testService(t, ri).Facts().WriteFact(context.Background(), trigAgent, fact.TriggerScriptPath(name), src, "script: "+name, "updated")
	require.NoError(t, err)
	waitTriggerHead(t, ri, r.CommitHash)
	return r.CommitHash
}

// settle waits until the dispatcher has processed and flushed the CURRENT head
// and the head has stopped moving (a script's writes are new heads), and
// returns it.
func settle(t *testing.T, ri *RepoInstance) string {
	t.Helper()
	var last string
	stable := 0
	require.Eventually(t, func() bool {
		h := head(t, ri)
		done, _ := ri.triggers.runSequence()
		if done == h && ri.triggers.flushed() && h == last {
			stable++
		} else {
			stable = 0
		}
		last = h
		return stable >= 3
	}, 20*time.Second, 20*time.Millisecond, "the dispatcher never settled")
	return last
}

// commitMessage reads a commit's message; commitSigner its verified signer.
func commitMessage(t *testing.T, ri *RepoInstance, h string) string {
	t.Helper()
	info, err := testService(t, ri).Triggers().CommitInfo(context.Background(), plumbing.NewHash(h))
	require.NoError(t, err)
	return info.Message
}

func trailersOf(t *testing.T, ri *RepoInstance, h string) store.Trailers {
	t.Helper()
	msg := commitMessage(t, ri, h)
	return store.Trailers{
		Trace: store.TrailerValue(msg, store.TrailerTrace), Cause: store.TrailerValue(msg, store.TrailerCause),
		Trigger: store.TrailerValue(msg, store.TrailerTrigger),
	}
}

// commitsAfter lists the commits from head back to (excluding) stop, oldest
// first, following first parents.
func commitsAfter(t *testing.T, ri *RepoInstance, stop string) []string {
	t.Helper()
	var out []string
	h := head(t, ri)
	for h != stop {
		out = append([]string{h}, out...)
		info, err := testService(t, ri).Triggers().CommitInfo(context.Background(), plumbing.NewHash(h))
		require.NoError(t, err)
		require.NotEmpty(t, info.Parents, "walked past the root looking for %s", stop)
		h = info.Parents[0].String()
	}
	return out
}

// payloads collects the TriggerEvent payloads of one trigger seen on the hub
// since the subscription.
type payloadSink struct {
	mu   sync.Mutex
	got  []json.RawMessage
	stop context.CancelFunc
}

func subscribePayloads(t *testing.T, ri *RepoInstance, trigger string) *payloadSink {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	events, _ := ri.TaskHub().Subscribe(ctx)
	s := &payloadSink{stop: cancel}
	t.Cleanup(cancel)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case e := <-events:
				if ev, ok := e.(TriggerEvent); ok && ev.Trigger == trigger && ev.Payload != nil {
					s.mu.Lock()
					s.got = append(s.got, ev.Payload)
					s.mu.Unlock()
				}
			}
		}
	}()
	return s
}

func (s *payloadSink) wait(t *testing.T, n int) []map[string]any {
	t.Helper()
	require.Eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.got) >= n
	}, 10*time.Second, 10*time.Millisecond, "%d payload(s) never arrived", n)
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, p := range s.got {
		var m map[string]any
		require.NoError(t, json.Unmarshal(p, &m))
		out = append(out, m)
	}
	return out
}

func outcomesOf(rows []store.TriggerFire) []string {
	out := []string{}
	for _, r := range rows {
		out = append(out, r.Outcome)
	}
	return out
}

// ---- T1, T12, T13: the host object

// HostFunctions: every function reaches the tool set with the arguments the
// MCP tool takes (moment_name defaults to trigger:<name>), the results come
// back as the tool's JSON, emit carries the payload on the branch stream,
// push answers kicked (F07 PR 4, T6), run with no recipe anywhere answers
// {bound: false} (F07 PR 5, T1) and logs nothing, and the fire is `ran`.
// Sabotage: route a function to the wrong tool; drop the moment_name default;
// keep push's stub.
func TestScript_HostFunctions(t *testing.T) {
	_, ri, tools := newScriptRepo(t, 0, scriptTrig("t1", "learn", "tasks/in/**", "host"))
	putScript(t, ri, "host", `
var q = knomit.query({text: "hello", limit: 3});
var e = knomit.explain("kb/tasks/in/a.md", {commit: "abc"});
var l = knomit.learn({topic: "tasks", category: "out", title: "Learned"});
var u = knomit.update("kb/tasks/out/learned.md", {updates: {title: "Learned v2"}, moment_name: "my-moment"});
var r = knomit.retract("kb/tasks/out/learned.md");
knomit.emit({q: q, e: e, l: l, u: u, r: r, push: knomit.push(), run: knomit.run("x", {a: 1}), who: change.author.id, trace: change.trace});
`)
	sink := subscribePayloads(t, ri, "t1")
	c0 := writeOn(t, ri, trigAgent, "kb/tasks/in/a.md")
	settle(t, ri)

	p := sink.wait(t, 1)[0]
	require.Equal(t, "hello", p["q"].(map[string]any)["echo"].(map[string]any)["text"])
	require.Equal(t, "kb/tasks/in/a.md", p["e"].(map[string]any)["file"])
	require.Equal(t, "abc", p["e"].(map[string]any)["commit"])
	require.Equal(t, trigAgent, p["l"].(map[string]any)["written_to"])
	require.Equal(t, "kb/tasks/out/learned.md", p["u"].(map[string]any)["file"])
	require.Equal(t, "kb/tasks/out/learned.md", p["r"].(map[string]any)["file"])
	require.Equal(t, map[string]any{"ok": true, "status": "kicked"}, p["push"])
	require.Equal(t, map[string]any{"bound": false}, p["run"], "no recipe on main or on this machine: unbound")
	require.Equal(t, c0, p["trace"], "a plain firing commit is the root of its own story")

	calls := tools.recorded()
	names := []string{}
	for _, c := range calls {
		names = append(names, c.Tool)
	}
	require.Equal(t, []string{"query", "explain", "learn", "update", "retract"}, names)
	require.Equal(t, float64(3), calls[0].Args["limit"], "numbers arrive as JSON numbers")
	require.Equal(t, "trigger:t1", calls[2].Args["moment_name"], "learn's moment_name defaults to trigger:<name>")
	require.Equal(t, "my-moment", calls[3].Args["moment_name"], "an explicit moment_name is kept")
	require.Equal(t, map[string]any{"title": "Learned v2"}, calls[3].Args["updates"])
	require.Equal(t, "trigger:t1", calls[4].Args["moment_name"])

	rows := firesOf(t, ri, "t1")
	require.Equal(t, []string{store.TriggerOutcomeRan}, outcomesOf(rows))
	require.Equal(t, int64(1), ri.triggers.stats.view("t1").Fires)
	// The script made three commits (learn, update, retract) after c0.
	require.Len(t, commitsAfter(t, ri, c0), 3)
}

// NoIOPrimitives: the sandbox exposes nothing but ECMAScript and the eight
// host functions; `Date.now()` is pinned to the run's instant (no clock);
// `knomit` is frozen. Sabotage: vm.Set("require", …) → red; leave Date on the
// wall clock → red; add a ninth function → red.
//
// The clock is proved by pinning the run's clock to a past instant and reading
// it back, NOT by spinning between two reads: a spin long enough to cross a
// wall-clock millisecond on a fast machine overran the 5 s script budget under
// -race (goja ~14× slower), so the fire timed out before emit.
func TestScript_NoIOPrimitives(t *testing.T) {
	pinned := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	setHooks(t, triggerHooks{now: func() time.Time { return pinned }})
	_, ri, _ := newScriptRepo(t, 0, scriptTrig("t1", "learn", "tasks/in/**", "probe"))
	putScript(t, ri, "probe", `
var d1 = Date.now(); var d2 = new Date().getTime();
var reassigned = false; try { knomit.query = null; reassigned = true; } catch (e) {}
knomit.emit({
  names: Object.getOwnPropertyNames(knomit).sort(),
  typeofs: [typeof require, typeof process, typeof fetch, typeof setTimeout, typeof XMLHttpRequest],
  d1: d1, d2: d2, frozen: Object.isFrozen(knomit), reassigned: reassigned,
  globals: Object.keys(globalThis), utc: new Date().toISOString().slice(-1)
});`)
	sink := subscribePayloads(t, ri, "t1")
	writeOn(t, ri, trigAgent, "kb/tasks/in/a.md")
	settle(t, ri)
	p := sink.wait(t, 1)[0]
	require.Equal(t, []any{"emit", "explain", "learn", "push", "query", "retract", "run", "update"}, p["names"])
	require.Equal(t, []any{"undefined", "undefined", "undefined", "undefined", "undefined"}, p["typeofs"])
	require.Equal(t, float64(pinned.UnixMilli()), p["d1"], "Date.now() is the run's pinned clock, not the wall clock")
	require.Equal(t, float64(pinned.UnixMilli()), p["d2"], "new Date() is the run's pinned clock, not the wall clock")
	require.Equal(t, true, p["frozen"])
	require.Equal(t, false, p["reassigned"], "the host object cannot be rewritten")
	for _, bound := range []string{"fact", "agent", "change", "knomit"} {
		require.NotContains(t, p["globals"], bound, "the bound globals are not enumerable (a script's own vars are)")
	}
	require.Equal(t, "Z", p["utc"])
}

// ---- T3, T4: identity and trailers

// WritesAsThisMachine: a peer's commit (another key, merged from main) fires
// the script; the script's commit is authored by THIS agent branch and signed
// by THIS store's key, never the peer's. Sabotage: author from change.author.id.
func TestScript_WritesAsThisMachine(t *testing.T) {
	_, ri, _ := newScriptRepo(t, 0, scriptTrig("t1", "learn", "tasks/in/**", "learn"))
	putScript(t, ri, "learn", `knomit.learn({topic: "tasks", category: "out", title: "from " + change.author.id + " " + change.source});`)
	svc := testService(t, ri)
	ctx := context.Background()
	before := settle(t, ri)

	svc.SetSigner(testsigner.Named("peer"))
	peer := writeOn(t, ri, "main", "kb/tasks/in/peer.md")
	svc.SetSigner(nil)
	require.NoError(t, svc.Branches().MergeBranch(ctx, "main", trigAgent, store.StrategyRemoteWins))
	settle(t, ri)

	commits := commitsAfter(t, ri, before)
	scriptCommit := commits[len(commits)-1]
	require.Contains(t, commitMessage(t, ri, scriptCommit), "Knomit-Trigger: t1")
	info, err := svc.Triggers().CommitInfo(ctx, plumbing.NewHash(scriptCommit))
	require.NoError(t, err)
	require.Equal(t, "test", info.AuthorName, "the author is derived from the agent branch")
	require.Contains(t, info.AuthorEmail, "@agents.knomit.io")
	own, err := svc.Triggers().CommitSignerOf(ctx, plumbing.NewHash(scriptCommit))
	require.NoError(t, err)
	require.Equal(t, svc.SignerFingerprint(), own.Fingerprint, "signed by this store's key")
	peerSig, err := svc.Triggers().CommitSignerOf(ctx, plumbing.NewHash(peer))
	require.NoError(t, err)
	require.NotEqual(t, peerSig.Fingerprint, own.Fingerprint, "never the firing author's key")
	rows := firesOf(t, ri, "t1")
	require.Len(t, rows, 1)
	require.Equal(t, "merged", rows[0].Source)
	require.Equal(t, peer, rows[0].Commit)
	require.Equal(t, peer, trailersOf(t, ri, scriptCommit).Cause)
}

// TrailersStamped: the script's commit ends with the three-line paragraph
// (trace = the derived trace, cause = the firing commit, trigger = the name),
// TrailerValue reads it, the signature verifies over it. Sabotage: omit
// Knomit-Cause; set the trailers per call instead of once per fire.
func TestScript_TrailersStamped(t *testing.T) {
	_, ri, _ := newScriptRepo(t, 0, scriptTrig("t1", "learn", "tasks/in/**", "learn"))
	putScript(t, ri, "learn", `knomit.learn({topic: "tasks", category: "out", title: "one"}); knomit.learn({topic: "tasks", category: "out", title: "two"});`)
	before := settle(t, ri)
	c0 := writeOn(t, ri, trigAgent, "kb/tasks/in/a.md")
	settle(t, ri)
	commits := commitsAfter(t, ri, before)
	require.Equal(t, c0, commits[0])
	require.Len(t, commits, 3, "the firing commit and two script writes")
	for _, h := range commits[1:] {
		msg := commitMessage(t, ri, h)
		require.True(t, strings.HasSuffix(msg, "\n\nKnomit-Trace: "+c0+"\nKnomit-Cause: "+c0+"\nKnomit-Trigger: t1\n"), "%q", msg)
		require.Equal(t, store.Trailers{Trace: c0, Cause: c0, Trigger: "t1"}, trailersOf(t, ri, h))
		sig, err := testService(t, ri).Triggers().CommitSignerOf(context.Background(), plumbing.NewHash(h))
		require.NoError(t, err)
		require.Equal(t, testService(t, ri).SignerFingerprint(), sig.Fingerprint)
	}
	require.Equal(t, store.Trailers{}, trailersOf(t, ri, c0), "the firing commit itself carries none")
}

// ---- T5, T6, T9: the trace

// TraceDerivation: the toucher's Knomit-Trace is copied forward; else a task
// (a signal with one entity) makes its id the trace; else the firing commit.
// Sabotage: derive from the head instead of the toucher; ignore the entity.
func TestScript_TraceDerivation(t *testing.T) {
	_, ri, _ := newScriptRepo(t, 0, scriptTrig("t1", "learn", "tasks/in/**", "learn"))
	putScript(t, ri, "learn", `knomit.learn({topic: "tasks", category: "out", title: "for " + change.path});`)
	svc := testService(t, ri)

	before := settle(t, ri)
	traced := writeMsg(t, ri, trigAgent, "kb/tasks/in/traced.md", "learn: traced\n\nKnomit-Trace: story-x\n")
	settle(t, ri)
	require.Equal(t, store.Trailers{Trace: "story-x", Cause: traced, Trigger: "t1"}, trailersOf(t, ri, head(t, ri)))

	before = settle(t, ri)
	signal := "---\nkind: pragmatic\ntype: signal\nconfidence: 0.8\nsources: 1\nentities: [task-7f3a9c]\n---\n# A task\n\ndo it\n"
	r, err := svc.Facts().WriteFact(context.Background(), trigAgent, "kb/tasks/in/task.md", signal, "learn: task", "learn")
	require.NoError(t, err)
	settle(t, ri)
	require.Equal(t, store.Trailers{Trace: "task-7f3a9c", Cause: r.CommitHash, Trigger: "t1"}, trailersOf(t, ri, head(t, ri)))
	_ = before

	plain := writeOn(t, ri, trigAgent, "kb/tasks/in/plain.md")
	settle(t, ri)
	require.Equal(t, store.Trailers{Trace: plain, Cause: plain, Trigger: "t1"}, trailersOf(t, ri, head(t, ri)))

	byPath := map[string]string{}
	for _, f := range firesOf(t, ri, "t1") {
		byPath[f.Path] = f.Trace
	}
	require.Equal(t, map[string]string{
		"kb/tasks/in/traced.md": "story-x", "kb/tasks/in/task.md": "task-7f3a9c", "kb/tasks/in/plain.md": plain,
	}, byPath, "change.trace and the fire row carry the derived value")
}

// ChainAcrossTriggers: T1 (on a) writes b; T2 (on b) writes c: c carries
// trace = a's story, cause = b's commit, trigger = t2. A merge commit and an
// ordinary write carry no paragraph. Sabotage: copy cause forward; stamp in
// storeCommit.
func TestScript_ChainAcrossTriggers(t *testing.T) {
	_, ri, _ := newScriptRepo(t, 0,
		scriptTrig("t1", "learn", "tasks/a/**", "to-b"),
		scriptTrig("t2", "learn", "tasks/b/**", "to-c"))
	putScript(t, ri, "to-b", `knomit.learn({topic: "tasks", category: "b", title: "b"});`)
	putScript(t, ri, "to-c", `knomit.learn({topic: "tasks", category: "c", title: "c"});`)
	before := settle(t, ri)
	c0 := writeOn(t, ri, trigAgent, "kb/tasks/a/x.md")
	settle(t, ri)
	commits := commitsAfter(t, ri, before)
	require.Len(t, commits, 3, "a, then b by t1, then c by t2")
	c1, c2 := commits[1], commits[2]
	require.Equal(t, store.Trailers{Trace: c0, Cause: c0, Trigger: "t1"}, trailersOf(t, ri, c1))
	require.Equal(t, store.Trailers{Trace: c0, Cause: c1, Trigger: "t2"}, trailersOf(t, ri, c2))

	// An ordinary write, and a merge commit bringing a peer's write through
	// main: neither is stamped.
	svc := testService(t, ri)
	plain := writeOn(t, ri, trigAgent, "kb/other/plain.md")
	require.Equal(t, store.Trailers{}, trailersOf(t, ri, plain))
	svc.SetSigner(testsigner.Named("peer"))
	writeOn(t, ri, "main", "kb/other/peer.md")
	svc.SetSigner(nil)
	require.NoError(t, svc.Branches().MergeBranch(context.Background(), "main", trigAgent, store.StrategyRemoteWins))
	merge := settle(t, ri)
	info, err := svc.Triggers().CommitInfo(context.Background(), plumbing.NewHash(merge))
	require.NoError(t, err)
	require.Len(t, info.Parents, 2, "fixture: the head is the merge commit")
	require.NotContains(t, info.Message, "Knomit-")
}

// TraceReadableWithGitLog (documentary): knomit's git objects live in SQLite,
// so the story is read on the ORIGIN (or any clone) after a push:
// `git log --all --grep='^Knomit-Trace: <id>$'` lists exactly the script
// commits of the story, and git's own trailer parser reads Knomit-Cause.
func TestScript_TraceReadableWithGitLog(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	_, ri, _ := newScriptRepo(t, 0,
		scriptTrig("t1", "learn", "tasks/a/**", "to-b"),
		scriptTrig("t2", "learn", "tasks/b/**", "to-c"))
	putScript(t, ri, "to-b", `knomit.learn({topic: "tasks", category: "b", title: "b"});`)
	putScript(t, ri, "to-c", `knomit.learn({topic: "tasks", category: "c", title: "c"});`)
	before := settle(t, ri)
	c0 := writeOn(t, ri, trigAgent, "kb/tasks/a/x.md")
	settle(t, ri)
	commits := commitsAfter(t, ri, before)
	require.Len(t, commits, 3)
	writeOn(t, ri, trigAgent, "kb/other/unrelated.md")
	settle(t, ri)

	bare := filepath.Join(t.TempDir(), "origin.git")
	require.NoError(t, os.MkdirAll(bare, 0o755))
	runGit(t, "", "init", "--bare", "--initial-branch=main", bare)
	svc := testService(t, ri)
	require.NoError(t, svc.ConfigureRemote(fileuri.New(bare), "main", trigAgent))
	_, err := svc.Remote().Push(context.Background(), trigAgent, nil)
	require.NoError(t, err)

	out := gitOut(t, bare, "log", "--all", "--grep=^Knomit-Trace: "+c0+"$", "--format=%H")
	require.ElementsMatch(t, commits[1:], strings.Fields(out), "exactly the story's script commits; the root is found by its own hash")
	require.Equal(t, commits[1], strings.TrimSpace(gitOut(t, bare, "log", "-1", "--format=%(trailers:key=Knomit-Cause,valueonly)", commits[2])))
	require.Equal(t, "t2", strings.TrimSpace(gitOut(t, bare, "log", "-1", "--format=%(trailers:key=Knomit-Trigger,valueonly)", commits[2])))
}

// ---- T7: the loop guard

// LoopGuard: a script's own write never re-fires its own trigger on that path
// (self-caused: counted, no row, the bookmark moves); a human's later edit
// fires; another trigger on the same path fires on the script's write.
// Sabotage: compare against the `if` result; skip by path only; drop the
// trailer read.
func TestScript_LoopGuard(t *testing.T) {
	_, ri, _ := newScriptRepo(t, 0,
		scriptTrig("t", "[learn, update]", "tasks/loop/**", "self"),
		trig("watcher", "[learn, update]", "tasks/loop/**", ""))
	putScript(t, ri, "self", `if (change.episode === 'learn') { knomit.update(change.path, {updates: {title: 'by-script'}}); }`)
	before := settle(t, ri)
	writeOn(t, ri, trigAgent, "kb/tasks/loop/p.md")
	last := settle(t, ri)
	commits := commitsAfter(t, ri, before)
	require.Len(t, commits, 2, "the human write and ONE script update")
	require.Equal(t, "t", trailersOf(t, ri, commits[1]).Trigger)
	require.Equal(t, []string{store.TriggerOutcomeRan}, outcomesOf(firesOf(t, ri, "t")), "exactly one run: the update it made is self-caused")
	st := ri.triggers.stats.view("t")
	require.Equal(t, int64(1), st.SelfCaused)
	require.Equal(t, int64(1), st.Evaluations, "a self-caused fire is not an evaluation")
	require.Equal(t, last, watermarks(t, ri)["t"], "the bookmark moved past the script's own write")
	require.Len(t, firesOf(t, ri, "watcher"), 2, "another trigger on the same path fires on the script's write too")

	// A human's edit of the same path fires again (no trailer on it).
	updateOn(t, ri, trigAgent, "kb/tasks/loop/p.md")
	settle(t, ri)
	require.Len(t, firesOf(t, ri, "t"), 2)
	require.Equal(t, int64(1), ri.triggers.stats.view("t").SelfCaused)
}

// LoopGuardNeverOnDue [M3]: a due fire is caused by time, not by the write:
// a script that re-arms its OWN due trigger (moving `expires`) fires again at
// the new instant; only the update episode it makes is self-caused.
// Sabotage: apply the guard in sweepDue → the second due fire is skipped.
func TestScript_LoopGuardNeverOnDue(t *testing.T) {
	clock := newFakeClock()
	dueHooks(t, clock, triggerHooks{})
	_, ri, _ := newScriptRepo(t, 0, scriptTrig("t", "[update, due]", "tasks/due/**", "rearm"))
	putScript(t, ri, "rearm", `if (change.episode === 'due') { knomit.update(change.path, {updates: {title: 'rearmed', expires: new Date(Date.now() + 1000).toISOString()}}); }`)
	settle(t, ri)
	past := clock.now().Add(-time.Hour).Format(time.RFC3339)
	r, err := testService(t, ri).Facts().WriteFact(context.Background(), trigAgent, "kb/tasks/due/p.md", datedBody("p", past), "learn: p", "learn")
	require.NoError(t, err)
	settle(t, ri)
	dueRows := func() int {
		n := 0
		for _, f := range firesOf(t, ri, "t") {
			if f.Episode == "due" {
				n++
			}
		}
		return n
	}
	require.Equal(t, 1, dueRows(), "the overdue fact fired once; the script re-armed it one second ahead")
	require.Equal(t, int64(1), ri.triggers.stats.view("t").SelfCaused, "the script's update is the one self-caused episode")
	_ = r

	clock.add(2 * time.Second)
	ri.triggers.triggerKick()
	require.Eventually(t, func() bool { return dueRows() == 2 }, 10*time.Second, 20*time.Millisecond,
		"the re-armed fact fires again at its new instant: a due fire is never self-caused")
	settle(t, ri)
	for _, f := range firesOf(t, ri, "t") {
		require.Equal(t, store.TriggerOutcomeRan, f.Outcome)
	}
}

// LoopGuardOnRetract: a script's own retract is self-caused in a linear
// advance (the deleting commit is the toucher and carries the trailer); in a
// NONLINEAR advance a retract's toucher is the head fallback, which carries no
// trailer, so the guard cannot see who deleted it and the retract fires
// (at-least-once; stated in the proposal).
func TestScript_LoopGuardOnRetract(t *testing.T) {
	_, ri, _ := newScriptRepo(t, 0, scriptTrig("t", "[learn, retract]", "tasks/rt/**", "zap"))
	putScript(t, ri, "zap", `if (change.episode === 'learn' && fact.title === 'zap') { knomit.retract(change.path); }`)
	svc := testService(t, ri)
	base := settle(t, ri)

	r, err := svc.Facts().WriteFact(context.Background(), trigAgent, "kb/tasks/rt/p.md", factBody("zap"), "learn: p", "learn")
	require.NoError(t, err)
	settle(t, ri)
	commits := commitsAfter(t, ri, base)
	require.Len(t, commits, 2, "the write and the script's retract")
	require.Equal(t, "t", trailersOf(t, ri, commits[1]).Trigger)
	require.Equal(t, []string{store.TriggerOutcomeRan}, outcomesOf(firesOf(t, ri, "t")))
	require.Equal(t, int64(1), ri.triggers.stats.view("t").SelfCaused, "the linear retract is self-caused")
	_ = r

	// Nonlinear: the branch is rewound to base and a new commit lands there
	// (p is present at the watermark, absent at the new head, and the head is
	// not a descendant of the watermark).
	keep := writeOn(t, ri, trigAgent, "kb/tasks/rt/keep.md") // title "keep": the script does nothing
	settle(t, ri)
	require.NoError(t, svc.TestingSetRef("refs/heads/"+trigAgent, base))
	other := writeOn(t, ri, trigAgent, "kb/tasks/rt/other.md")
	settle(t, ri)
	var retracts []store.TriggerFire
	for _, f := range firesOf(t, ri, "t") {
		if f.Episode == "retract" {
			retracts = append(retracts, f)
		}
	}
	require.Len(t, retracts, 1, "%+v", firesOf(t, ri, "t"))
	require.Equal(t, "kb/tasks/rt/keep.md", retracts[0].Path)
	require.True(t, retracts[0].Nonlinear)
	require.Equal(t, other, retracts[0].Commit, "the head fallback: no toucher, no trailer, so the guard cannot apply")
	_ = keep
}

// ---- T8: the rate cap

// RateCap: script runs per trigger per minute; fires beyond it are DROPPED
// (a rate-limited row each, the bookmark moves past them), one WARN when the
// cap first bites, and the next fire after the window runs. Sabotage: never
// trim the window; count writes instead of runs; log per skip.
func TestScript_RateCap(t *testing.T) {
	logs := captureLogs(t, zerolog.WarnLevel)
	clock := newFakeClock()
	dueHooks(t, clock, triggerHooks{})
	_, ri, _ := newScriptRepo(t, 3, scriptTrig("t", "learn", "tasks/in/**", "noop"))
	putScript(t, ri, "noop", `knomit.emit({ok: true});`)
	settle(t, ri)

	// A merge bringing 10 matching paths at once: ONE commit, one advance.
	// One commit is the realistic shape of a merged burst.
	files := map[string]string{}
	for i := 0; i < 10; i++ {
		p := fmt.Sprintf("kb/tasks/in/b%02d.md", i)
		files[p] = factBody(p)
	}
	last, _, err := testService(t, ri).Facts().BatchWriteFacts(context.Background(), trigAgent, files, nil, "learn: burst", "learn")
	require.NoError(t, err)
	waitTriggerHead(t, ri, last)
	rows := firesOf(t, ri, "t")
	require.Len(t, rows, 10)
	counts := map[string]int{}
	for _, r := range rows {
		counts[r.Outcome]++
	}
	require.Equal(t, map[string]int{store.TriggerOutcomeRan: 3, store.TriggerOutcomeRateLimited: 7}, counts)
	require.Equal(t, int64(7), ri.triggers.stats.view("t").RateLimited)
	require.Equal(t, 1, strings.Count(logs.String(), "rate cap reached"), "%s", logs.String())
	require.Equal(t, last, watermarks(t, ri)["t"], "dropped fires are not retried: the bookmark moved past them")

	// Still inside the minute: dropped.
	writeOn(t, ri, trigAgent, "kb/tasks/in/still.md")
	settle(t, ri)
	require.Equal(t, int64(8), ri.triggers.stats.view("t").RateLimited)
	require.Equal(t, 1, strings.Count(logs.String(), "rate cap reached"), "one WARN per cap episode")

	// After the window: runs.
	clock.add(61 * time.Second)
	writeOn(t, ri, trigAgent, "kb/tasks/in/later.md")
	settle(t, ri)
	require.Equal(t, int64(4), ri.triggers.stats.view("t").Fires)
	require.Equal(t, int64(8), ri.triggers.stats.view("t").RateLimited)
}

// ---- T10, T11: errors and time

// RuntimeErrorIsOutcome: a throw is `script-error` with the message, counted,
// logged once per (trigger, blob, message); the run continues and the
// bookmark advances. [M2] A Go panic inside a tool is thrown to the script as
// an error naming the tool: the fire is script-error, the run completes once
// and the watermark advances — never safeRun's re-run loop. Sabotage:
// propagate the error; log every fire; drop the host recover.
func TestScript_RuntimeErrorIsOutcome(t *testing.T) {
	logs := captureLogs(t, zerolog.WarnLevel)
	_, ri, tools := newScriptRepo(t, 0,
		scriptTrig("throws", "learn", "tasks/in/**", "throws"),
		scriptTrig("panics", "learn", "tasks/in/**", "panics"),
		trig("all", "learn", "tasks/in/**", ""))
	putScript(t, ri, "throws", `throw new Error("boom");`)
	putScript(t, ri, "panics", `try { knomit.learn({topic: "tasks", category: "out", title: "x"}); } catch (e) { knomit.emit({caught: e.message, isError: e instanceof Error}); throw e; }`)
	tools.hook = func(ctx context.Context, tool string, args map[string]any) (string, bool, error, bool) {
		if tool == "learn" {
			panic("stub exploded")
		}
		return "", false, nil, false
	}
	sink := subscribePayloads(t, ri, "panics")
	settle(t, ri)

	var last string
	for i := 0; i < 5; i++ {
		last = writeOn(t, ri, trigAgent, fmt.Sprintf("kb/tasks/in/%d.md", i))
		settle(t, ri)
	}
	th := firesOf(t, ri, "throws")
	require.Len(t, th, 5)
	for _, f := range th {
		require.Equal(t, store.TriggerOutcomeScriptError, f.Outcome)
		require.Contains(t, f.Error, "boom")
	}
	require.Equal(t, int64(5), ri.triggers.stats.view("throws").ScriptError)
	warnsOf := func(trigger string) int {
		n := 0
		for _, line := range strings.Split(logs.String(), "\n") {
			if strings.Contains(line, "trigger script failed") && strings.Contains(line, `"trigger":"`+trigger+`"`) {
				n++
			}
		}
		return n
	}
	require.Equal(t, 1, warnsOf("throws"), "one WARN per (trigger, blob, message) for 5 fires: %s", logs.String())
	require.Equal(t, 1, warnsOf("panics"))
	require.Equal(t, last, watermarks(t, ri)["throws"])
	require.Len(t, firesOf(t, ri, "all"), 5, "siblings still fire")

	pa := firesOf(t, ri, "panics")
	require.Len(t, pa, 5)
	require.Equal(t, store.TriggerOutcomeScriptError, pa[0].Outcome)
	require.Contains(t, pa[0].Error, "knomit.learn: panic: stub exploded")
	p := sink.wait(t, 1)[0]
	require.Equal(t, true, p["isError"], "a host failure is a real JavaScript Error")
	require.Contains(t, p["caught"], "stub exploded")
	require.Equal(t, last, watermarks(t, ri)["panics"], "the watermark advanced: the range did not re-fire")
	_, seq := ri.triggers.runSequence()
	time.Sleep(200 * time.Millisecond)
	_, seq2 := ri.triggers.runSequence()
	require.Equal(t, seq, seq2, "no re-run loop after a host panic")
}

// TimeoutIsOutcome: a busy loop is interrupted at the budget (script-timeout);
// a host call that finishes within the budget is not cut; [M1] a host call
// that BLOCKS returns the budget's DeadlineExceeded to the script and the
// fire is script-timeout within the budget plus the interrupt latency — the
// host ctx carries the deadline, not the handler's 60 s. Sabotage: pass the
// bare run ctx to the host.
func TestScript_TimeoutIsOutcome(t *testing.T) {
	_, ri, tools := newScriptRepo(t, 0,
		scriptTrig("loops", "learn", "tasks/in/**", "loops"),
		scriptTrig("slowcall", "learn", "tasks/in/**", "slowcall"),
		scriptTrig("blocks", "learn", "tasks/in/**", "blocks"))
	setHooks(t, triggerHooks{scriptBudget: 300 * time.Millisecond})
	putScript(t, ri, "loops", `while (true) {}`)
	putScript(t, ri, "slowcall", `knomit.query({slow: true}); knomit.emit({done: true});`)
	putScript(t, ri, "blocks", `knomit.query({block: true}); knomit.emit({after: true});`)
	var blockedErr error
	var blockedAt time.Duration
	tools.hook = func(ctx context.Context, tool string, args map[string]any) (string, bool, error, bool) {
		switch {
		case args["slow"] == true:
			time.Sleep(100 * time.Millisecond) // ignores ctx: completes inside the budget
			return `{"facts":[]}`, false, nil, true
		case args["block"] == true:
			t0 := time.Now()
			<-ctx.Done() // a stuck store: returns only when the ctx ends
			blockedErr, blockedAt = ctx.Err(), time.Since(t0)
			return "", false, ctx.Err(), true
		}
		return "", false, nil, false
	}
	sinkSlow := subscribePayloads(t, ri, "slowcall")
	sinkBlock := subscribePayloads(t, ri, "blocks")
	settle(t, ri)

	started := time.Now()
	writeOn(t, ri, trigAgent, "kb/tasks/in/a.md")
	settle(t, ri)
	require.Less(t, time.Since(started), 3*time.Second, "three fires, two of them bounded by a 300 ms budget")

	lo := firesOf(t, ri, "loops")
	require.Len(t, lo, 1)
	require.Equal(t, store.TriggerOutcomeScriptTimeout, lo[0].Outcome)
	require.Contains(t, lo[0].Error, "exceeded")
	require.Equal(t, int64(1), ri.triggers.stats.view("loops").ScriptTimeout)

	sc := firesOf(t, ri, "slowcall")
	require.Equal(t, []string{store.TriggerOutcomeRan}, outcomesOf(sc), "a 100 ms host call under a 300 ms budget completes")
	require.Equal(t, true, sinkSlow.wait(t, 1)[0]["done"])
	require.Less(t, ri.triggers.stats.view("slowcall").Duration.MaxMS, 50.0, "host time is not the script's own time")

	bl := firesOf(t, ri, "blocks")
	require.Len(t, bl, 1)
	require.Equal(t, store.TriggerOutcomeScriptTimeout, bl[0].Outcome)
	require.ErrorIs(t, blockedErr, context.DeadlineExceeded, "the host ctx carried the budget as its deadline, not the handler's 60 s")
	require.Less(t, blockedAt, 1500*time.Millisecond, "released at the budget (300 ms), not the handler's own timeout")
	sinkBlock.mu.Lock()
	require.Empty(t, sinkBlock.got, "the budget interrupts the script at the very next instruction; the emit after the call never runs")
	sinkBlock.mu.Unlock()
}

// ---- T14, T15, T16: the script at the head

// ReloadOnHeadBlob: a changed script runs on the next fire (no restart), the
// same blob is not recompiled, and the script change itself fires nothing.
// Sabotage: key the cache by name only.
func TestScript_ReloadOnHeadBlob(t *testing.T) {
	_, ri, _ := newScriptRepo(t, 0, scriptTrig("t", "learn", "tasks/in/**", "ver"))
	sink := subscribePayloads(t, ri, "t")
	putScript(t, ri, "ver", `knomit.emit({v: 1});`)
	writeOn(t, ri, trigAgent, "kb/tasks/in/a.md")
	settle(t, ri)
	putScript(t, ri, "ver", `knomit.emit({v: 2});`)
	writeOn(t, ri, trigAgent, "kb/tasks/in/b.md")
	settle(t, ri)
	ps := sink.wait(t, 2)
	require.Equal(t, float64(1), ps[0]["v"])
	require.Equal(t, float64(2), ps[1]["v"])
	ri.triggers.mu.Lock()
	compiles := ri.triggers.sc.compiles
	ri.triggers.mu.Unlock()
	require.Equal(t, 2, compiles)

	putScript(t, ri, "ver", `knomit.emit({v: 2});`) // the same blob again
	writeOn(t, ri, trigAgent, "kb/tasks/in/c.md")
	settle(t, ri)
	ri.triggers.mu.Lock()
	compiles = ri.triggers.sc.compiles
	ri.triggers.mu.Unlock()
	require.Equal(t, 2, compiles, "the same blob is not recompiled")
	for _, f := range fires(t, ri) {
		require.NotContains(t, f.Path, ".knomit/", "a script change is never an episode")
	}
}

// CompileErrorIsInvalid / MissingScript: a script that does not compile, or
// is absent at the head, makes the trigger `invalid` with the error on the
// endpoint, logs ONE error per (trigger, blob), records every fire as
// script-error, keeps the bookmark moving, and clears once fixed. Sabotage:
// log per fire; freeze the watermark; treat missing as unsupported.
func TestScript_CompileErrorAndMissingAreInvalid(t *testing.T) {
	logs := captureLogs(t, zerolog.ErrorLevel)
	_, ri, _ := newScriptRepo(t, 0,
		scriptTrig("broken", "learn", "tasks/in/**", "broken"),
		scriptTrig("missing", "learn", "tasks/in/**", "nope"))
	putScript(t, ri, "broken", `this is not javascript ((`)
	rep, err := ri.TriggerReport(context.Background(), 0)
	require.NoError(t, err)
	states := map[string]TriggerView{}
	for _, v := range rep.Triggers {
		states[v.Name] = v
	}
	require.Equal(t, "invalid", states["broken"].State)
	require.Contains(t, states["broken"].Error, "does not compile")
	require.Equal(t, "broken", states["broken"].Script)
	require.Equal(t, "invalid", states["missing"].State)
	require.Contains(t, states["missing"].Error, "not found")

	var last string
	for i := 0; i < 5; i++ {
		last = writeOn(t, ri, trigAgent, fmt.Sprintf("kb/tasks/in/%d.md", i))
	}
	settle(t, ri)
	for _, name := range []string{"broken", "missing"} {
		rows := firesOf(t, ri, name)
		require.Len(t, rows, 5, name)
		for _, f := range rows {
			require.Equal(t, store.TriggerOutcomeScriptError, f.Outcome)
		}
		require.Equal(t, last, watermarks(t, ri)[name], "%s: the bookmark advances", name)
		require.Equal(t, int64(5), ri.triggers.stats.view(name).ScriptError)
	}
	// One ERROR per (trigger, blob) across the 5 fires: the compile error
	// once, the missing file once (plus "broken" being absent before it was
	// written — a different blob, its own line).
	require.Equal(t, 1, strings.Count(logs.String(), "does not compile"), "%s", logs.String())
	require.Equal(t, 1, strings.Count(logs.String(), "nope.js not found"), "%s", logs.String())

	putScript(t, ri, "broken", `knomit.emit({fixed: true});`)
	putScript(t, ri, "nope", `knomit.emit({fixed: true});`)
	rep, err = ri.TriggerReport(context.Background(), 0)
	require.NoError(t, err)
	for _, v := range rep.Triggers {
		require.Equal(t, "active", v.State, v.Name)
		require.Empty(t, v.Error)
	}
	writeOn(t, ri, trigAgent, "kb/tasks/in/after.md")
	settle(t, ri)
	require.Equal(t, store.TriggerOutcomeRan, firesOf(t, ri, "broken")[0].Outcome)
	require.Equal(t, store.TriggerOutcomeRan, firesOf(t, ri, "missing")[0].Outcome)
}

// ---- T17, T18: sequencing

// WriteIsNextAdvance: one fire, one script write, then a SECOND run whose
// range is exactly (the firing commit → the script's commit) and fires a
// sibling trigger; never a run inside a run.
func TestScript_WriteIsNextAdvance(t *testing.T) {
	_, ri, _ := newScriptRepo(t, 0,
		scriptTrig("t1", "learn", "tasks/in/**", "to-out"),
		trig("sibling", "learn", "tasks/out/**", ""))
	putScript(t, ri, "to-out", `knomit.learn({topic: "tasks", category: "out", title: "o"});`)
	before := settle(t, ri)
	_, seq0 := ri.triggers.runSequence()
	c0 := writeOn(t, ri, trigAgent, "kb/tasks/in/a.md")
	settle(t, ri)
	commits := commitsAfter(t, ri, before)
	require.Len(t, commits, 2)
	c1 := commits[1]
	_, seq1 := ri.triggers.runSequence()
	require.Equal(t, seq0+2, seq1, "the fire's run, then the run for the script's commit")
	runs := runRows(t, ri)
	require.GreaterOrEqual(t, len(runs), 2)
	require.Equal(t, c0, runs[0].RangeFrom, "the second run's range starts at the firing commit")
	require.Equal(t, c1, runs[0].RangeTo)
	require.Equal(t, []string{"kb/tasks/out/o.md"}, pathsOf(firesOf(t, ri, "sibling")))
}

// NoStoreHeldDuringScript [M1]: SwapStore completes while a script is in a
// long JavaScript loop (phase B holds no Acquire; the swap drains only a host
// call's), and stop() interrupts the loop within the interrupt latency, not
// the budget. Sabotage: hold Acquire across the script; arm the interrupt
// from the timer only.
func TestScript_NoStoreHeldDuringScript(t *testing.T) {
	m, ri, _ := newScriptRepo(t, 0, scriptTrig("t", "learn", "tasks/in/**", "spin"))
	putScript(t, ri, "spin", `while (true) {}`)
	settle(t, ri)
	setHooks(t, triggerHooks{scriptBudget: 5 * time.Second})
	writeOn(t, ri, trigAgent, "kb/tasks/in/a.md")
	time.Sleep(300 * time.Millisecond) // the run is in phase B, spinning

	require.NoError(t, ri.WithRead(func(svc *store.Service) { require.NoError(t, svc.Checkpoint()) }))
	tmp := filepath.Join(t.TempDir(), "copy.db")
	copyDB(t, m.RepoPath(ri.UID()), tmp)
	started := time.Now()
	require.NoError(t, m.SwapStore(ri, tmp))
	require.Less(t, time.Since(started), time.Second, "SwapStore must not wait behind a running script")

	started = time.Now()
	ri.triggers.stop()
	require.Less(t, time.Since(started), 500*time.Millisecond, "cancel interrupts the script within the interrupt latency, not the 5 s budget")
}

// ---- T19: the private-path refusal

// PrivatePathRefused: learn with a `path`, update and retract of a `.knomit/`
// path each throw BEFORE any tool runs; nothing is committed. (The same calls
// through the MCP tool succeed: internal/mcp/script_tools_test.go.)
// Sabotage: drop the host check.
func TestScript_PrivatePathRefused(t *testing.T) {
	_, ri, tools := newScriptRepo(t, 0, scriptTrig("t", "learn", "tasks/in/**", "priv"))
	putScript(t, ri, "priv", `
var out = {};
try { knomit.learn({path: ".knomit/jobs/x.md", title: "x"}); } catch (e) { out.learn = e.message; }
try { knomit.update(".knomit/jobs/x.md", {updates: {title: "y"}}); } catch (e) { out.update = e.message; }
try { knomit.update(".knomit/jobs/x", {updates: {title: "y"}}); } catch (e) { out.updateNoExt = e.message; }
try { knomit.retract(".knomit/jobs/x.md"); } catch (e) { out.retract = e.message; }
knomit.emit(out);`)
	sink := subscribePayloads(t, ri, "t")
	before := settle(t, ri)
	c0 := writeOn(t, ri, trigAgent, "kb/tasks/in/a.md")
	settle(t, ri)
	p := sink.wait(t, 1)[0]
	for _, k := range []string{"learn", "update", "updateNoExt", "retract"} {
		require.Contains(t, p[k], "a script may not write under .knomit/", k)
	}
	require.Empty(t, tools.recorded(), "refused before any tool ran")
	require.Equal(t, []string{c0}, commitsAfter(t, ri, before), "nothing was committed")
	require.Equal(t, []string{store.TriggerOutcomeRan}, outcomesOf(firesOf(t, ri, "t")))
}

// ---- F08 PR A, M2: knomit.learn(facts, {retract}) — the F04 move from a
// script.

// LearnRetractForwarded [T-A8, host half]: opts.retract reaches the learn
// tool as `retract`, verbatim, beside the defaulted moment_name; `[]` with a
// retract is forwarded as a batch retraction. The real one-commit result is
// TestScript_MoveThroughRealTools (internal/web). Sabotage: forward only
// moment_name → no `retract` argument → red.
func TestScript_LearnRetractForwarded(t *testing.T) {
	_, ri, tools := newScriptRepo(t, 0, scriptTrig("t", "learn", "tasks/in/**", "take"))
	tools.hook = func(ctx context.Context, tool string, args map[string]any) (string, bool, error, bool) {
		return `{"commits":[]}`, false, nil, true
	}
	putScript(t, ri, "take", `
knomit.learn({topic: "tasks", category: "working", title: "w"}, {retract: [change.path]});
knomit.learn([], {retract: [change.path, "kb/tasks/in/other.md"], moment_name: "drop"});`)
	writeOn(t, ri, trigAgent, "kb/tasks/in/a.md")
	settle(t, ri)
	calls := tools.recorded()
	require.Len(t, calls, 2)
	require.Equal(t, "learn", calls[0].Tool)
	require.Equal(t, []any{"kb/tasks/in/a.md"}, calls[0].Args["retract"])
	require.Equal(t, "trigger:t", calls[0].Args["moment_name"])
	require.Equal(t, []any{}, calls[1].Args["facts"])
	require.Equal(t, []any{"kb/tasks/in/a.md", "kb/tasks/in/other.md"}, calls[1].Args["retract"])
	require.Equal(t, "drop", calls[1].Args["moment_name"])
	require.Equal(t, []string{store.TriggerOutcomeRan}, outcomesOf(firesOf(t, ri, "t")))
}

// LearnRetractPrivateRefused [T-A7, script half]: a retract path under a
// private segment — .knomit/ job state (which the MCP tool itself accepts)
// or a .drafts fact — throws in the HOST before any tool runs; so does a
// retract that is not a list of strings. Sabotage: skip the private check on
// opts.retract → the tool is called → red.
func TestScript_LearnRetractPrivateRefused(t *testing.T) {
	_, ri, tools := newScriptRepo(t, 0, scriptTrig("t", "learn", "tasks/in/**", "priv"))
	putScript(t, ri, "priv", `
var out = {};
try { knomit.learn({topic: "tasks", category: "w", title: "x"}, {retract: [".knomit/jobs/x.md"]}); } catch (e) { out.job = e.message; }
try { knomit.learn([], {retract: ["kb/.drafts/x.md"]}); } catch (e) { out.drafts = e.message; }
try { knomit.learn([], {retract: "kb/tasks/in/a.md"}); } catch (e) { out.notList = e.message; }
try { knomit.learn([], {retract: [5]}); } catch (e) { out.notString = e.message; }
knomit.emit(out);`)
	sink := subscribePayloads(t, ri, "t")
	before := settle(t, ri)
	c0 := writeOn(t, ri, trigAgent, "kb/tasks/in/a.md")
	settle(t, ri)
	p := sink.wait(t, 1)[0]
	require.Contains(t, p["job"], "a script may not write under .knomit/")
	require.Contains(t, p["drafts"], "a script may not write under .knomit/")
	require.Contains(t, p["notList"], "must be a list")
	require.Contains(t, p["notString"], "non-empty string")
	require.Empty(t, tools.recorded(), "refused before any tool ran")
	require.Equal(t, []string{c0}, commitsAfter(t, ri, before), "nothing was committed")
}
