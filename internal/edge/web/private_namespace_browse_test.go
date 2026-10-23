package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/engine/store"
)

// TestTopicBrowsersSkipPrivateNamespace pins the same rule
// TestHandleTopics_HidesPrivateDirectory / TestHandleTopicFacts_HidesPrivateTopic
// / TestLensTopics_HidesPrivateDirectory already cover for a generic
// dot-directory, anchored on the concrete case this whole design exists to
// protect: job state living at .knomit/jobs/x.md (see internal/domain/fact.PrivateRoot).
//
// All three sub-tests drive the real handler chain (router → handler → real
// store) rather than asserting on fact.IsPrivatePath directly, so they fail if
// a handler ever forgets to call the guard, not just if the guard itself
// breaks. Each first checks that the store DOES list the private entry, so the
// guard, not an empty tree, is what hides it.
func TestTopicBrowsersSkipPrivateNamespace(t *testing.T) {
	// handlers_topics.go:133 — handleTopicFacts, browsing directly INTO the
	// private topic (the same "direct navigation" case
	// TestHandleTopicFacts_HidesPrivateTopic pins for .drafts).
	t.Run("repo topic facts under .knomit", func(t *testing.T) {
		m, svcs := newRealStoreManager(t, "alpha")
		writeFactsInOneCommit(t, m.Get("alpha"), plainFact("kb/.knomit/jobs/x.md", "Job state", nil))
		entries, err := svcs["alpha"].Facts().ListDir(t.Context(), "agent/test", "kb/.knomit/jobs")
		require.NoError(t, err)
		require.Len(t, entries, 1, "fixture: the store must list the job file")
		seen := observeListDir(t, map[string]*store.Service{"alpha": svcs["alpha"]})
		s := &Server{Manager: m, OntologyRoot: "kb"}
		r := s.NewAPIRouter()

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/repos/alpha/branches/agent:test/topics/.knomit/jobs/facts", nil)
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status: %d, body=%s", rec.Code, rec.Body.String())
		}

		var body struct {
			Count int `json:"count"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if body.Count != 0 {
			t.Fatalf("count: %d, want 0 — job state under .knomit/ must not be listed", body.Count)
		}
		require.Equal(t, []string{"kb/.knomit/jobs"}, seen.of("alpha"))
	})

	// handlers_topics.go:261 — the topic node handler, whose second guard
	// catches the case where the node PATH itself (not just an entry's name)
	// already carries the private segment, exactly the .knomit/jobs shape.
	t.Run("repo topic node under .knomit", func(t *testing.T) {
		m, svcs := newRealStoreManager(t, "alpha")
		writeFactsInOneCommit(t, m.Get("alpha"), plainFact("kb/.knomit/jobs/x.md", "Job state", nil))
		entries, err := svcs["alpha"].Facts().ListDir(t.Context(), "agent/test", "kb/.knomit")
		require.NoError(t, err)
		require.Len(t, entries, 1, "fixture: the store must list the jobs directory")
		seen := observeListDir(t, map[string]*store.Service{"alpha": svcs["alpha"]})
		s := &Server{Manager: m, OntologyRoot: "kb"}
		r := s.NewAPIRouter()

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/repos/alpha/branches/agent:test/topics/.knomit", nil)
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status: %d, body=%s", rec.Code, rec.Body.String())
		}

		var body struct {
			Count int `json:"count"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if body.Count != 0 {
			t.Fatalf("count: %d, want 0 — .knomit/jobs must not be listed as a topic child", body.Count)
		}
		require.Equal(t, []string{"kb/.knomit"}, seen.of("alpha"))
	})

	// handlers_lenses_topics.go:113 — the lens union tree is a reader too, and
	// must agree with the plain repo topicHandler above.
	//
	// The request stays at the topics ROOT rather than drilling into
	// /topics/.knomit: one level down, federate.ReadTargetsFor's
	// ontology-aware topic skip (decision-17) would treat ".knomit" as a
	// topic name, find no mount whose ontology declares it, and drop every
	// mount from the fan-out before ListDir ever runs — which would make this
	// sub-test pass for the wrong reason (no mount queried) rather than
	// because the private-path guard fired. At the root the topic filter is
	// inert, so ".knomit" only disappears if the guard (line 113) removes it
	// from the listing itself, matching TestLensTopics_HidesPrivateDirectory's
	// existing pattern for ".drafts".
	t.Run("lens topic browser under .knomit", func(t *testing.T) {
		m, _ := newTestLensManager(t, "alpha")
		s := &Server{Manager: m, OntologyRoot: "kb"}
		r := s.NewAPIRouter()
		createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[]}`)
		alpha := m.Get("alpha")
		writeFactsInOneCommit(t, alpha,
			plainFact("kb/gotchas/g.md", "G", nil),
			plainFact("kb/.knomit/jobs/x.md", "Job state", nil),
		)
		stores := lensMountStores(t, m, "alpha")
		entries, err := stores["alpha"].Facts().ListDir(t.Context(), alpha.AgentBranch(), "kb")
		require.NoError(t, err)
		require.Len(t, entries, 2, "fixture: the store must list .knomit at the root")

		body := decodeLensTopics(t, getLensFacts(t, r, "/lenses/eng/topics"))
		if len(body.Children) != 1 || body.Children[0].Name != "gotchas" {
			t.Fatalf("children: got %+v, want only \"gotchas\" — .knomit must not be listed", body.Children)
		}
	})
}
