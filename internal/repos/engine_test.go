package repos

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/store"
)

// The engine tier, host half: the map, `model`, `experiment` and `incoming`,
// driven from real scripts on the real sandbox (fact.RunScriptResult) with
// the host a run builds (newEngineHost). The MCP entry and the loop script
// end to end are internal/mcp's (reason_test.go), where the real tools are.

// engineLogBuf is a goroutine-safe log sink.
type engineLogBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *engineLogBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *engineLogBuf) lines(t *testing.T) []map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(s.b.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(l), &m), l)
		out = append(out, m)
	}
	return out
}

// captureLog points the global logger at a buffer for one test.
func captureEngineLog(t *testing.T) *engineLogBuf {
	t.Helper()
	buf := &engineLogBuf{}
	prev := log.Logger
	log.Logger = zerolog.New(buf).Level(zerolog.DebugLevel)
	t.Cleanup(func() { log.Logger = prev })
	return buf
}

func newEngineTestRepo(t *testing.T) *RepoInstance {
	t.Helper()
	svc, err := store.Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, "agent/test"))
	return NewTestInstanceWithDeps(TestInstanceConfig{
		Name: "eng", UID: "eng-uid", AgentBranch: "agent/test", Svc: svc,
		Ontology: fact.CodeOntology(), OntologyRoot: "kb",
	})
}

// runProbe runs src with the engine tier of a fresh run on ri and returns the
// script's completion value.
func runProbe(t *testing.T, e *Engine, ri *RepoInstance, src string) (any, *engineHost, error) {
	t.Helper()
	prog, err := fact.CompileScript("probe", src)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel) // the host outlives the probe: a test may run a second script on it
	h := newEngineHost(e, ctx, NewBindingOfRepo(ri, ri.AgentBranch()), "reason-0000abcd", time.Now())
	out, err := fact.RunScriptResult(ctx, prog, "probe", map[string]any{"question": "q"}, h.functions(), time.Now().UTC())
	return out, h, err
}

// The engine tier is EXACTLY six names, and exec is nowhere: not on the host
// object and not as a global. Sabotage: add "exec" (or "emit") to
// engineHost.functions().
func TestEngine_HostMapIsExactlySixNoExec(t *testing.T) {
	ri := newEngineTestRepo(t)
	e := NewEngine(EngineConfig{})
	h := newEngineHost(e, context.Background(), NewBindingOfRepo(ri, ri.AgentBranch()), "reason-00000001", time.Now())
	var names []string
	for k := range h.functions() {
		names = append(names, k)
	}
	sort.Strings(names)
	require.Equal(t, []string{"experiment", "explain", "incoming", "learn", "model", "query"}, names)

	out, _, err := runProbe(t, e, ri, `({keys: Object.keys(knomit).sort(), exec: typeof knomit.exec, gexec: typeof exec,
		emit: typeof knomit.emit, run: typeof knomit.run, update: typeof knomit.update})`)
	require.NoError(t, err)
	m := out.(map[string]any)
	require.Equal(t, []any{"experiment", "explain", "incoming", "learn", "model", "query"}, m["keys"])
	for _, k := range []string{"exec", "gexec", "emit", "run", "update"} {
		require.Equal(t, "undefined", m[k], k)
	}
}

// fakeModel is an OpenAI-compatible chat-completions endpoint that answers
// with a fixed content and finish_reason, and records what it was sent.
type fakeModel struct {
	srv     *httptest.Server
	hits    atomic.Int64
	mu      sync.Mutex
	last    map[string]any
	content string
	finish  string
}

func newFakeModel(t *testing.T, content, finish string) *fakeModel {
	t.Helper()
	f := &fakeModel{content: content, finish: finish}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		require.Equal(t, "/v1/chat/completions", r.URL.Path)
		b, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(b, &req)
		f.mu.Lock()
		f.last = req
		content, finish := f.content, f.finish
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": content}, "finish_reason": finish}},
			"usage":   map[string]any{"prompt_tokens": 11, "completion_tokens": 7},
		})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeModel) engine() *Engine {
	return NewEngine(EngineConfig{Model: config.EngineConfig{ModelURL: f.srv.URL + "/v1", Model: "lfm-test"}})
}

const verdictSchemaJS = `{type: "object", additionalProperties: false, required: ["verdict"],
	properties: {verdict: {type: "string", enum: ["supports", "unrelated"]}}}`

// model: the schema goes to the endpoint as response_format json_schema, the
// configured model name is sent, inline reasoning is stripped, and the parsed
// output comes back; the call is logged with the run key and the judgment.
func TestEngine_Model_SchemaSentAndOutputReturned(t *testing.T) {
	buf := captureEngineLog(t)
	ri := newEngineTestRepo(t)
	fm := newFakeModel(t, "<think>weighing it</think>{\"verdict\": \"supports\"}", "stop")
	out, h, err := runProbe(t, fm.engine(), ri, `knomit.model({schema: `+verdictSchemaJS+`, system: "s", user: "u",
		log: {step: 3, state: "ASSESS", judgment: "support", posterior: 0.4}})`)
	require.NoError(t, err)
	m := out.(map[string]any)
	require.Equal(t, map[string]any{"verdict": "supports"}, m["output"])
	require.Equal(t, 1, h.modelCalls)

	fm.mu.Lock()
	req := fm.last
	fm.mu.Unlock()
	require.Equal(t, "lfm-test", req["model"])
	rf := req["response_format"].(map[string]any)
	require.Equal(t, "json_schema", rf["type"])
	sch := rf["json_schema"].(map[string]any)["schema"].(map[string]any)
	require.Equal(t, []any{"verdict"}, sch["required"])

	var line map[string]any
	for _, l := range buf.lines(t) {
		if l["message"] == "reason step" && l["judgment"] == "support" {
			line = l
		}
	}
	require.NotNil(t, line, "the model call has its own reason step line")
	require.Equal(t, "reason-0000abcd", line["reason_run"])
	require.Equal(t, "info", line["level"])
	require.Equal(t, float64(3), line["step"])
	require.Equal(t, "ASSESS", line["state"])
	require.Equal(t, 0.4, line["posterior"])
	require.Equal(t, `{"verdict":"supports"}`, line["verdict"])
}

// A reply outside the schema is REFUSED: thrown to the script, never
// returned, and logged at Warn with the run key. Sabotage: drop the
// resolved.Validate call (red: the script gets {"verdict":"maybe"}).
func TestEngine_Model_SchemaEnforcedByTheHost(t *testing.T) {
	buf := captureEngineLog(t)
	ri := newEngineTestRepo(t)
	fm := newFakeModel(t, `{"verdict": "maybe"}`, "stop")
	out, _, err := runProbe(t, fm.engine(), ri, `var r; try { knomit.model({schema: `+verdictSchemaJS+`, user: "u"}); r = "returned" }
		catch (e) { r = e.message }; r`)
	require.NoError(t, err)
	require.Contains(t, out, "does not match the schema")
	require.True(t, hasWarn(buf.lines(t), "reason-0000abcd", "does not match the schema"))
}

// finish_reason=length is a failed step: thrown, with a Warn line carrying
// the run key — never the partial reply. Sabotage: drop the length check
// (red: the truncated-but-valid JSON comes back as an answer).
func TestEngine_Model_TruncationIsThrownAndWarned(t *testing.T) {
	buf := captureEngineLog(t)
	ri := newEngineTestRepo(t)
	fm := newFakeModel(t, `{"verdict": "supports"}`, "length")
	out, _, err := runProbe(t, fm.engine(), ri, `var r; try { knomit.model({schema: `+verdictSchemaJS+`, user: "u",
		log: {step: 2, state: "ASSESS", judgment: "support"}}); r = "returned" } catch (e) { r = e.message }; r`)
	require.NoError(t, err)
	require.Contains(t, out, "truncated: finish_reason=length after 7 completion tokens")
	require.True(t, hasWarn(buf.lines(t), "reason-0000abcd", "truncated"))
}

// No schema, no call: the endpoint is never reached.
func TestEngine_Model_SchemaRequired(t *testing.T) {
	ri := newEngineTestRepo(t)
	fm := newFakeModel(t, `{}`, "stop")
	out, _, err := runProbe(t, fm.engine(), ri, `var r; try { knomit.model({user: "u"}); r = "returned" } catch (e) { r = e.message }; r`)
	require.NoError(t, err)
	require.Contains(t, out, "schema is required")
	require.Equal(t, int64(0), fm.hits.Load())
}

func hasWarn(lines []map[string]any, run, text string) bool {
	for _, l := range lines {
		if l["level"] == "warn" && l["reason_run"] == run && strings.Contains(l["error"].(string), text) {
			return true
		}
	}
	return false
}

// experiment from the script: open forks exp/<name> and moves the RUN onto it
// (a learn lands there, not on the agent branch); commit is refused; rollback
// of an experiment the run did not open is refused; rollback of its own
// deletes it and moves the run back. Sabotage: skip setBinding in
// experimentOpen (red: the fact lands on the agent branch).
func TestEngine_Experiment_OpenRollbackFromScript(t *testing.T) {
	ri := newEngineTestRepo(t)
	tools := &stubTools{ri: ri}
	e := NewEngine(EngineConfig{Tools: tools})
	agentBefore := headOn(t, ri, "agent/test")

	// someone else's experiment, which the run must not discard
	svc := testService(t, ri)
	_, err := svc.Experiments().OpenExperiment(context.Background(), "not-mine", "", "agent/test")
	require.NoError(t, err)

	out, h, err := runProbe(t, e, ri, `
		var o = knomit.experiment({action: "open", name: "reason-probe", log: {step: 1, state: "EMIT"}});
		var w = knomit.learn({topic: "architecture", category: "engine/probe", title: "Probe Fact", body: "b"});
		var c; try { knomit.experiment({action: "commit", name: "reason-probe"}); c = "committed" } catch (e) { c = e.message }
		var x; try { knomit.experiment({action: "rollback", name: "not-mine"}); x = "rolled" } catch (e) { x = e.message }
		var again; try { knomit.experiment({action: "open", name: "not-mine"}); again = "opened" } catch (e) { again = e.message }
		var l = knomit.experiment({action: "list"});
		({open: o, written: w.written_to, commit: c, foreign: x, again: again, list: l})`)
	require.NoError(t, err)
	m := out.(map[string]any)
	require.Equal(t, "exp/reason-probe", m["open"].(map[string]any)["branch"])
	require.Equal(t, "exp/reason-probe", m["written"], "the learn ran on the experiment")
	require.Contains(t, m["commit"], "commit is not available to the engine")
	require.Contains(t, m["foreign"], "rollback discards only an experiment this run opened")
	require.Contains(t, m["again"], "already inside")
	require.Equal(t, "reason-probe", h.experiment)
	require.Equal(t, []string{"kb/architecture/engine/probe/probe-fact.md"}, h.written)
	require.Equal(t, agentBefore, headOn(t, ri, "agent/test"), "the agent branch did not move")
	rows := m["list"].(map[string]any)["experiments"].([]any)
	require.Len(t, rows, 2)

	// rollback, from a second script on the same host
	prog, err := fact.CompileScript("probe2", `knomit.experiment({action: "rollback"})`)
	require.NoError(t, err)
	_, err = fact.RunScriptResult(context.Background(), prog, "probe2", nil, h.functions(), time.Now().UTC())
	require.NoError(t, err)
	_, exists, err := svc.Experiments().GetExperiment(context.Background(), "reason-probe")
	require.NoError(t, err)
	require.False(t, exists, "rollback deleted the run's experiment")
	_, exists, err = svc.Experiments().GetExperiment(context.Background(), "not-mine")
	require.NoError(t, err)
	require.True(t, exists, "and left the other one alone")
	require.Equal(t, "", h.experiment)
	require.Equal(t, "agent/test", h.cur.WriteBranch(), "the run is back on its world")
}

func headOn(t *testing.T, ri *RepoInstance, branch string) string {
	t.Helper()
	h, err := testService(t, ri).Branches().HeadCommit(context.Background(), branch)
	require.NoError(t, err)
	return h
}

// incoming returns exactly what the store's IncomingAtCommit returns for the
// head of the run's branch: the fact that cites the target, at the commit it
// was written. Sabotage: query OutgoingAtCommit instead (red: B is missing).
func TestEngine_Incoming_IsTheStoresIncomingAtCommit(t *testing.T) {
	ri := newEngineTestRepo(t)
	svc := testService(t, ri)
	ctx := context.Background()
	a := "kb/architecture/engine/a.md"
	b := "kb/architecture/engine/b.md"
	_, err := svc.Facts().WriteFact(ctx, "agent/test", a, factBody("Target A"), "learn: a", "learn")
	require.NoError(t, err)
	rb, err := svc.Facts().WriteFact(ctx, "agent/test", b,
		"---\ntype: observation\nconfidence: 0.8\nsources: 1\nrefs: ['"+a+"']\n---\n# Citer B\n\nbody\n", "learn: b", "learn")
	require.NoError(t, err)

	head := headOn(t, ri, "agent/test")
	want, err := svc.GraphStore().IncomingAtCommit(ctx, "agent/test", a, head)
	require.NoError(t, err)
	require.Len(t, want, 1, "precondition: the store sees B citing A")
	require.Equal(t, b, want[0].Path)

	out, _, err := runProbe(t, NewEngine(EngineConfig{}), ri, `knomit.incoming("`+a+`", {log: {step: 2, state: "WIDEN"}})`)
	require.NoError(t, err)
	m := out.(map[string]any)
	require.Equal(t, "agent/test", m["branch"])
	refs := m["refs"].([]any)
	require.Len(t, refs, 1)
	r := refs[0].(map[string]any)
	require.Equal(t, b, r["path"])
	require.Equal(t, rb.CommitHash, r["commit"])
	require.Equal(t, want[0].Commit, r["commit"])
	require.Equal(t, want[0].Title, r["title"])
}

// The engine's learn writes ONLY inside an experiment this run opened: a
// learn before experiment open is refused (Warn line with the run key) and
// the agent branch does not move; a learn carrying opts.retract is refused
// even inside the experiment, and the fact it names survives. Sabotage: drop
// either guard in engLearn (red: the first lands on agent/test, the second
// deletes the fact).
func TestEngine_Learn_OnlyInsideItsExperimentAndNeverRetracts(t *testing.T) {
	buf := captureEngineLog(t)
	ri := newEngineTestRepo(t)
	tools := &stubTools{ri: ri}
	e := NewEngine(EngineConfig{Tools: tools})
	victim := "kb/architecture/engine/victim.md"
	writeOn(t, ri, "agent/test", victim)
	agentBefore := headOn(t, ri, "agent/test")

	out, h, err := runProbe(t, e, ri, `
		var before; try { knomit.learn({topic: "architecture", category: "engine/probe", title: "Early", body: "b"}); before = "written" }
		catch (e) { before = e.message }
		knomit.experiment({action: "open", name: "reason-guard"});
		var rt; try { knomit.learn([], {retract: ["`+victim+`"]}); rt = "retracted" } catch (e) { rt = e.message }
		({before: before, retract: rt})`)
	require.NoError(t, err)
	m := out.(map[string]any)
	require.Contains(t, m["before"], "writes only inside an experiment this run opened")
	require.Contains(t, m["retract"], "opts.retract is refused")
	require.Equal(t, agentBefore, headOn(t, ri, "agent/test"), "the agent branch did not move")
	require.Empty(t, h.written)
	for _, c := range tools.recorded() {
		require.NotEqual(t, "learn", c.Tool, "no learn reached the tools: %v", c.Args)
	}
	exists, err := testService(t, ri).Facts().FactExists(context.Background(), "exp/reason-guard", victim)
	require.NoError(t, err)
	require.True(t, exists, "the fact named in retract survives on the experiment")
	require.True(t, hasWarn(buf.lines(t), "reason-0000abcd", "writes only inside an experiment"))
	require.True(t, hasWarn(buf.lines(t), "reason-0000abcd", "opts.retract is refused"))
}

// --- PR 3: the Go-side fixes ------------------------------------------------

// The budget is capped (fix-later 5): a run holds the single model slot for
// its whole budget, so a budget past MaxReasonBudget is clamped to it — the
// start line logs the budget the run actually has. Sabotage: drop the clamp
// in withDefaults (red: the start line says 100h).
func TestEngine_BudgetIsCapped(t *testing.T) {
	buf := captureEngineLog(t)
	ri := newEngineTestRepo(t)
	e := NewEngine(EngineConfig{Home: writeEngineScript(t, `({outcome: "gap", posterior: 0, chain: [], gaps: []})`)})
	res, err := e.RunWith(context.Background(), NewBindingOfRepo(ri, ri.AgentBranch()), "q", ReasonOptions{Budget: 100 * time.Hour})
	require.NoError(t, err)
	var start map[string]any
	for _, l := range buf.lines(t) {
		if l["message"] == "reason start" && l["reason_run"] == res.Run {
			start = l
		}
	}
	require.NotNil(t, start)
	require.Equal(t, float64(MaxReasonBudget.Milliseconds()), start["budget"], "zerolog logs a Dur in ms")
	require.Equal(t, MaxReasonBudget, ReasonOptions{Budget: 100 * time.Hour}.withDefaults().Budget)
	require.Equal(t, DefaultReasonBudget, ReasonOptions{}.withDefaults().Budget)
}

// writeEngineScript puts src at <home>/engine/reason.js and returns home.
func writeEngineScript(t *testing.T, src string) string {
	t.Helper()
	home := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(home, "engine"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(home, "engine", "reason.js"), []byte(src), 0o644))
	return home
}

// A run that never gets the model slot still logs `reason start` and
// `reason done` with its run key (fix-later 10), and returns interrupted.
// Sabotage: drop the done line on the slot-timeout path (red: no done line
// for the run).
func TestEngine_SlotTimeoutLogsStartAndDone(t *testing.T) {
	buf := captureEngineLog(t)
	ri := newEngineTestRepo(t)
	e := NewEngine(EngineConfig{})
	e.slot <- struct{}{} // another run holds the endpoint
	defer func() { <-e.slot }()
	res, err := e.Run(context.Background(), NewBindingOfRepo(ri, ri.AgentBranch()), "q", 50*time.Millisecond)
	require.NoError(t, err)
	require.Equal(t, "interrupted", res.Outcome)
	msgs := map[string]int{}
	for _, l := range buf.lines(t) {
		if l["reason_run"] == res.Run {
			msgs[l["message"].(string)]++
		}
	}
	require.Equal(t, 1, msgs["reason start"], "%v", msgs)
	require.Equal(t, 1, msgs["reason done"], "%v", msgs)
}

// A Go panic inside model or experiment is recovered by the host (fix-later
// 7): the script gets an Error, the run a Warn line; nothing crashes.
// Sabotage: delete either defer/recover (red: the test binary panics).
func TestEngine_ModelAndExperimentRecoverPanics(t *testing.T) {
	buf := captureEngineLog(t)
	ri := newEngineTestRepo(t)
	fm := newFakeModel(t, `{"verdict": "supports"}`, "stop")
	e := fm.engine()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h := newEngineHost(e, ctx, NewBindingOfRepo(ri, ri.AgentBranch()), "reason-0000beef", time.Now())
	h.e = nil     // model: modelCall dereferences the engine
	h.world = nil // experiment: list dereferences the world
	prog, err := fact.CompileScript("probe", `
		var m; try { knomit.model({schema: `+verdictSchemaJS+`, user: "u"}); m = "returned" } catch (e) { m = e.message }
		var x; try { knomit.experiment({action: "list"}); x = "returned" } catch (e) { x = e.message }
		({model: m, experiment: x})`)
	require.NoError(t, err)
	out, err := fact.RunScriptResult(ctx, prog, "probe", nil, h.functions(), time.Now().UTC())
	require.NoError(t, err)
	m := out.(map[string]any)
	require.Contains(t, m["model"], "knomit.model: panic:")
	require.Contains(t, m["experiment"], "knomit.experiment: panic:")
	require.True(t, hasWarn(buf.lines(t), "reason-0000beef", "knomit.model: panic"))
	require.True(t, hasWarn(buf.lines(t), "reason-0000beef", "knomit.experiment: panic"))
}

// Before reporting the answer as pending the asker's decision, the run
// checks its experiment still exists (fix-later R2): here another session
// rolls it back while the run is still inside its model call, and the
// result says it is gone instead of offering commit/rollback. Sabotage:
// drop the experimentExists check in RunWith (red: Next offers "commit").
func TestEngine_GoneExperimentIsNotPending(t *testing.T) {
	ri := newEngineTestRepo(t)
	svc := testService(t, ri)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// another session declines the experiment mid-run
		require.NoError(t, svc.Experiments().RollbackExperiment(context.Background(), "reason-r2"))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": `{"verdict": "supports"}`}, "finish_reason": "stop"}},
		})
	}))
	t.Cleanup(srv.Close)
	e := NewEngine(EngineConfig{
		Model: config.EngineConfig{ModelURL: srv.URL + "/v1"},
		Home: writeEngineScript(t, `knomit.experiment({action: "open", name: "reason-r2"});
			knomit.model({schema: `+verdictSchemaJS+`, user: "u"});
			({outcome: "gap", posterior: 0, chain: [], gaps: []})`),
	})
	res, err := e.Run(context.Background(), NewBindingOfRepo(ri, ri.AgentBranch()), "q", 30*time.Second)
	require.NoError(t, err)
	require.Equal(t, "reason-r2", res.Experiment)
	require.Contains(t, res.Next, "no longer exists")
	require.NotContains(t, res.Next, `action: "commit"`)
}
