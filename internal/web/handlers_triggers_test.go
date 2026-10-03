package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/repos"
	"knomit/internal/store"
)

// triggersRepo boots a real manager with one repo whose agent branch carries
// an ontology declaring one trigger on `tasks`, and waits (through the
// endpoint) until the dispatcher has processed the advance that declared it.
// Without the wait, the ontology commit and the test's first fact commit can
// coalesce into ONE advance under load, and a fact committed in the same
// advance as its trigger never fires — by design (first appearance).
func triggersRepo(t *testing.T, ontology string) (*repos.RepoInstance, http.Handler) {
	t.Helper()
	m, _ := newTestLensManager(t, "alpha")
	ri := m.Get("alpha")
	var head string
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		res, err := svc.Facts().WriteFact(context.Background(), ri.AgentBranch(), repos.OntologyPath, ontology, "ontology", "updated")
		require.NoError(t, err)
		head = res.CommitHash
	}))
	r := (&Server{Manager: m, AgentBranch: "machine/test", OntologyRoot: "kb"}).NewAPIRouter()
	waitTriggersHead(t, r, "/repos/alpha/branches/"+urlBranch(ri.AgentBranch())+"/triggers", head)
	return ri, r
}

// waitTriggersHead polls GET …/triggers until the report's head is the given
// commit, i.e. the dispatcher has read the ontology at that head.
func waitTriggersHead(t *testing.T, r http.Handler, url, head string) triggersPage {
	t.Helper()
	var page triggersPage
	var lastCode int
	var lastBody string
	require.Eventually(t, func() bool {
		rec, p := getTriggers(t, r, url)
		lastCode, lastBody = rec.Code, rec.Body.String()
		if rec.Code != http.StatusOK {
			return false
		}
		page = p
		return p.Head == head
	}, 20*time.Second, 20*time.Millisecond, "the dispatcher never reached %s (last status %d: %s)", head, lastCode, lastBody)
	return page
}

const oneTriggerOntology = "id: t\nname: T\ntopics:\n  tasks:\n    description: t\n    triggers:\n      - {name: all, on: [learn, update], do: emit}\n  other:\n    description: o\n"

type triggersPage struct {
	Enabled  bool                      `json:"enabled"`
	Reason   string                    `json:"reason"`
	Branch   string                    `json:"branch"`
	Head     string                    `json:"head"`
	Runs     repos.TriggerRunStats     `json:"runs"`
	Triggers []repos.TriggerView       `json:"triggers"`
	Fires    []store.TriggerFire       `json:"fires"`
	Links    map[string]map[string]any `json:"_links"`
}

func getTriggers(t *testing.T, r http.Handler, url string) (*httptest.ResponseRecorder, triggersPage) {
	t.Helper()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	var p triggersPage
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p), rec.Body.String())
	}
	return rec, p
}

// TriggerStatsExposed: GET …/triggers is the one statistics surface. After a
// matching write it shows the trigger active with its watermark at the head,
// its counts and durations, the run statistics, and (with ?log) the fire log
// with the fire and the run row. Sabotage: leave the stats out, or serve a
// second copy of the numbers elsewhere.
func TestREST_TriggersExposed(t *testing.T) {
	ri, r := triggersRepo(t, oneTriggerOntology)
	agent := ri.AgentBranch()
	url := "/repos/alpha/branches/" + urlBranch(agent) + "/triggers?log=5"

	head := seedOn(t, ri, agent, "kb/tasks/a/fire.md")
	var page triggersPage
	var lastCode int
	var lastBody string
	require.Eventually(t, func() bool {
		rec, p := getTriggers(t, r, url)
		lastCode, lastBody = rec.Code, rec.Body.String()
		if rec.Code != http.StatusOK {
			return false
		}
		page = p
		return len(p.Triggers) == 1 && p.Triggers[0].Stats.Fires == 1 && p.Triggers[0].Watermark == head && len(p.Fires) >= 2
	}, 20*time.Second, 20*time.Millisecond, "the fire never showed on the endpoint: %+v (last status %d: %s)", page, lastCode, lastBody)

	require.True(t, page.Enabled)
	require.Equal(t, agent, page.Branch)
	require.Equal(t, head, page.Head)
	tr := page.Triggers[0]
	require.Equal(t, "all", tr.Name)
	require.Equal(t, "tasks", tr.Node)
	require.Equal(t, "tasks/**", tr.Match, "the pattern after substitution (absent match = the node's subtree)")
	require.Equal(t, []string{"learn", "update"}, tr.On)
	require.Equal(t, "emit", tr.Do)
	require.Equal(t, "active", tr.State)
	require.Equal(t, int64(1), tr.Stats.Evaluations)
	require.Equal(t, int64(1), tr.Stats.Fires)
	require.Equal(t, int64(1), tr.Stats.Duration.Count)
	require.Equal(t, 1024, tr.Stats.Duration.P95Window)
	require.GreaterOrEqual(t, tr.Stats.Duration.MaxMS, tr.Stats.Duration.MinMS)
	require.GreaterOrEqual(t, page.Runs.Runs, int64(1))
	require.NotNil(t, page.Runs.Last)
	require.Equal(t, head, page.Runs.Last.RangeTo)
	require.Equal(t, 1, page.Runs.Last.Fires)

	kinds := map[string]int{}
	for _, f := range page.Fires {
		kinds[f.Outcome]++
		if f.Outcome == store.TriggerOutcomeEmitted {
			require.Equal(t, "kb/tasks/a/fire.md", f.Path)
			require.Equal(t, "learn", f.Episode)
			require.Equal(t, "local", f.Source)
			require.Equal(t, head, f.Commit)
		}
	}
	require.Equal(t, 1, kinds[store.TriggerOutcomeEmitted])
	require.GreaterOrEqual(t, kinds[store.TriggerOutcomeRun], 1)
	require.Contains(t, page.Links["self"]["href"], "/triggers")
	require.Contains(t, page.Links["events"]["href"], "/events")

	// ?log is bounded and validated.
	rec, _ := getTriggers(t, r, "/repos/alpha/branches/"+urlBranch(agent)+"/triggers?log=abc")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	rec, _ = getTriggers(t, r, "/repos/alpha/branches/"+urlBranch(agent)+"/triggers?log=501")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	rec, p := getTriggers(t, r, "/repos/alpha/branches/"+urlBranch(agent)+"/triggers")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Empty(t, p.Fires, "no log rows without ?log")

	// The branch root links to it.
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/repos/alpha/branches/"+urlBranch(agent), nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"triggers":{"href":"`)
	require.Contains(t, rec.Body.String(), "/triggers{?log,run}")
}

// A repo whose store has no dispatcher (a bare test instance) still answers,
// with enabled=false and a reason.
func TestREST_TriggersDisabledSaysWhy(t *testing.T) {
	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{Name: "sub", Subscribed: true, ReadBranch: "main"})
	m := newTestManagerWithRepos(t)
	m.Set("sub", ri)
	r := (&Server{Manager: m}).NewAPIRouter()
	rec, p := getTriggers(t, r, "/repos/sub/branches/main/triggers")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.False(t, p.Enabled)
	require.Contains(t, p.Reason, "subscription")
	require.Empty(t, p.Triggers)
}

// EmitOnlyOnItsBranchStream: the hub is per repo, so both streams receive the
// hub event; only the agent branch's stream forwards `event: trigger`. The
// stream for main never does. Sabotage: drop the branch filter in the switch.
func TestSSE_TriggerOnlyOnItsBranchStream(t *testing.T) {
	ri, r := triggersRepo(t, oneTriggerOntology)
	agent := ri.AgentBranch()

	open := func(branch string) (*streamRecorder, context.CancelFunc, chan struct{}) {
		rec := newStreamRecorder()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		req := httptest.NewRequest(http.MethodGet, "/repos/alpha/branches/"+urlBranch(branch)+"/events", nil).WithContext(ctx)
		go func() { r.ServeHTTP(rec, req); close(done) }()
		rec.waitFor(t, "initial status", func(b string) bool { return strings.Contains(b, "event: status") })
		return rec, cancel, done
	}
	agentRec, agentCancel, agentDone := open(agent)
	mainRec, mainCancel, mainDone := open("main")

	seedOn(t, ri, agent, "kb/tasks/a/fire.md")
	body := agentRec.waitFor(t, "the trigger event on the agent stream", func(b string) bool {
		return strings.Contains(b, "event: trigger")
	})
	require.Contains(t, body, `"trigger":"all"`)
	require.Contains(t, body, `"path":"kb/tasks/a/fire.md"`)
	require.Contains(t, body, `"episode":"learn"`)
	require.Contains(t, body, `"source":"local"`)
	require.NotContains(t, body, `"Branch"`, "the branch is not part of the payload; the stream IS the branch")

	time.Sleep(200 * time.Millisecond) // give a leaked event time to arrive
	require.NotContains(t, mainRec.body(), "event: trigger", "main's stream must not carry the agent branch's fire")

	agentCancel()
	mainCancel()
	<-agentDone
	<-mainDone
}

// TriggersByRun (F07 PR 5, D5): a `do: run` fire's run id finds exactly its
// two rows on GET …/triggers?run=<id> — `started`, then the recipe's result —
// both with that run_id, the same trace and recipe_source; ?run replaces the
// recent log; a malformed id is a 400. Since rehearsal finding F5 the answer
// is ONLY that run's rows (no trigger state) with two runs in the fixture, and
// the done row's text is in `message`, not `error`. Sabotage: serve ?run from
// the recent log (red: the other rows show), wrap the rows in the whole
// report again (red: `triggers` present), or skip the id check (red: no 400).
func TestREST_TriggersByRun(t *testing.T) {
	m, home := newTestLensManager(t, "alpha")
	require.NoError(t, os.MkdirAll(filepath.Join(home, "recipes"), 0o755))
	// Two back-to-back seeds can coalesce into ONE dispatcher pass, and a
	// concurrent:1 recipe (the default) would then drop the second fire as
	// `busy`, never retried (#409). Concurrency is not what this test checks
	// (TestRun_ConcurrencyCap does), so allow both; TestRun_RunIDCorrelates
	// does the same.
	require.NoError(t, os.WriteFile(filepath.Join(home, "recipes", "worker.js"),
		[]byte("// knomit: {\"concurrent\": 2}\n"+`({status: "done", message: "ran " + run.id});`), 0o600))
	ri := m.Get("alpha")
	var head string
	ontology := "id: t\nname: T\ntopics:\n  tasks:\n    description: t\n    triggers:\n" +
		"      - {name: work, on: learn, do: run, recipe: worker}\n      - {name: all, on: learn, do: emit}\n  other:\n    description: o\n"
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		res, err := svc.Facts().WriteFact(context.Background(), ri.AgentBranch(), repos.OntologyPath, ontology, "ontology", "updated")
		require.NoError(t, err)
		head = res.CommitHash
	}))
	r := (&Server{Manager: m, AgentBranch: "machine/test", OntologyRoot: "kb"}).NewAPIRouter()
	base := "/repos/alpha/branches/" + urlBranch(ri.AgentBranch()) + "/triggers"
	waitTriggersHead(t, r, base, head)
	// TWO runs, so "only this run's rows" is falsifiable against another run.
	seedOn(t, ri, ri.AgentBranch(), "kb/tasks/a/fire.md")
	seedOn(t, ri, ri.AgentBranch(), "kb/tasks/a/fire-2.md")

	var ids []string
	require.Eventually(t, func() bool {
		_, p := getTriggers(t, r, base+"?log=50")
		ids = ids[:0]
		done := 0
		for _, f := range p.Fires {
			if f.Trigger == "work" && f.Outcome == store.TriggerOutcomeStarted {
				ids = append(ids, f.RunID)
			}
			if f.Trigger == "work" && f.Outcome == store.TriggerOutcomeDone {
				done++
			}
		}
		return len(ids) == 2 && done == 2
	}, 20*time.Second, 20*time.Millisecond, "both runs never finished")
	id, other := ids[0], ids[1]
	require.Regexp(t, `^run-[0-9a-f]{32}$`, id)
	require.NotEqual(t, id, other)

	rec, page := getTriggers(t, r, base+"?log=50&run="+id)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, page.Fires, 2, "exactly this run's two rows: %+v", page.Fires)
	require.Equal(t, store.TriggerOutcomeStarted, page.Fires[0].Outcome)
	require.Equal(t, store.TriggerOutcomeDone, page.Fires[1].Outcome)
	require.Equal(t, "ran "+id, page.Fires[1].Message, "the recipe saw run.id; a done row's text is a message")
	require.Empty(t, page.Fires[1].Error, "F5: a done row carries no error")
	for _, f := range page.Fires {
		require.Equal(t, id, f.RunID, "never the other run's rows")
		require.Equal(t, "work", f.Trigger)
		require.Equal(t, page.Fires[0].Trace, f.Trace)
		require.Equal(t, store.RecipeSourceLocal, f.RecipeSource)
	}
	// ONLY the run's rows (F5): no declared triggers, statistics, run
	// summary, head or ontology blob — the whole trigger state is not sent.
	var raw map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	for _, k := range []string{"triggers", "runs", "head", "ontology_blob", "enabled"} {
		require.NotContains(t, raw, k, "?run= returns only the run's rows")
	}
	require.Equal(t, id, raw["run"])
	require.Equal(t, ri.AgentBranch(), raw["branch"])

	// The plain report still has the whole state.
	_, full := getTriggers(t, r, base+"?log=50")
	found := false
	for _, tr := range full.Triggers {
		if tr.Name == "work" {
			found = true
			require.Equal(t, "worker", tr.Recipe)
			require.Equal(t, int64(2), tr.Stats.Started)
		}
	}
	require.True(t, found)

	for _, bad := range []string{"x", "run-XYZ", "run-0123", "RUN-0123456789abcdef0123456789abcdef"} {
		rec, _ := getTriggers(t, r, base+"?run="+bad)
		require.Equal(t, http.StatusBadRequest, rec.Code, bad)
	}
	rec, p := getTriggers(t, r, base+"?run=run-00000000000000000000000000000000")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Empty(t, p.Fires, "an unknown run id has no rows")
}
