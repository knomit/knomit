package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/repos"
	"knomit/internal/store"
)

// triggersRepo boots a real manager with one repo whose agent branch carries
// an ontology declaring one trigger on `tasks`.
func triggersRepo(t *testing.T, ontology string) (*repos.RepoInstance, http.Handler) {
	t.Helper()
	m, _ := newTestLensManager(t, "alpha")
	ri := m.Get("alpha")
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		_, err := svc.Facts().WriteFact(context.Background(), ri.AgentBranch(), repos.OntologyPath, ontology, "ontology", "updated")
		require.NoError(t, err)
	}))
	r := (&Server{Manager: m, AgentBranch: "machine/test", OntologyRoot: "kb"}).NewAPIRouter()
	return ri, r
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
	require.Eventually(t, func() bool {
		rec, p := getTriggers(t, r, url)
		if rec.Code != http.StatusOK {
			return false
		}
		page = p
		return len(p.Triggers) == 1 && p.Triggers[0].Stats.Fires == 1 && p.Triggers[0].Watermark == head && len(p.Fires) >= 2
	}, 20*time.Second, 20*time.Millisecond, "the fire never showed on the endpoint: %+v", page)

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
	require.Contains(t, rec.Body.String(), "/triggers{?log}")
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
