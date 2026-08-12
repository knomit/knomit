package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"knomit/internal/store"
)

// TestTopicBrowsersSkipPrivateNamespace pins the same rule
// TestHandleTopics_HidesPrivateDirectory / TestHandleTopicFacts_HidesPrivateTopic
// / TestLensTopics_HidesPrivateDirectory already cover for a generic
// dot-directory, anchored on the concrete case this whole design exists to
// protect: job state living at .knomit/jobs/x.md (see internal/fact.PrivateRoot).
//
// All three sub-tests drive the real handler chain (router → handler →
// TopicLister stub) rather than asserting on fact.IsPrivatePath directly, so
// they fail if a handler ever forgets to call the guard, not just if the
// guard itself breaks.
func TestTopicBrowsersSkipPrivateNamespace(t *testing.T) {
	// handlers_topics.go:133 — handleTopicFacts, browsing directly INTO the
	// private topic (the same "direct navigation" case
	// TestHandleTopicFacts_HidesPrivateTopic pins for .drafts).
	t.Run("repo topic facts under .knomit", func(t *testing.T) {
		lister := &stubTopicLister{
			dirs: []store.DirEntry{{Name: "x.md", IsDir: false}},
		}
		s := &Server{
			Manager:      newTestManagerWithRepos(t, "alpha"),
			OntologyRoot: "kb",
			providers:    storeProviders{topicLister: lister},
		}
		r := s.NewAPIRouter()

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/repos/alpha/branches/main/topics/.knomit/jobs/facts", nil)
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
	})

	// handlers_topics.go:261 — the topic node handler, whose second guard
	// catches the case where the node PATH itself (not just an entry's name)
	// already carries the private segment, exactly the .knomit/jobs shape.
	t.Run("repo topic node under .knomit", func(t *testing.T) {
		lister := &stubTopicLister{
			dirs: []store.DirEntry{{Name: "jobs", IsDir: true}},
		}
		s := &Server{
			Manager:      newTestManagerWithRepos(t, "alpha"),
			OntologyRoot: "kb",
			providers:    storeProviders{topicLister: lister},
		}
		r := s.NewAPIRouter()

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/repos/alpha/branches/main/topics/.knomit", nil)
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
		stub := &lensTopicsStub{
			dirsByRepo: map[string][]store.DirEntry{
				"alpha": {{Name: "gotchas", IsDir: true}, {Name: ".knomit", IsDir: true}},
			},
		}
		s := &Server{Manager: m, OntologyRoot: "kb", providers: storeProviders{topicLister: stub}}
		r := s.NewAPIRouter()
		createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[]}`)

		body := decodeLensTopics(t, getLensFacts(t, r, "/lenses/eng/topics"))
		if len(body.Children) != 1 || body.Children[0].Name != "gotchas" {
			t.Fatalf("children: got %+v, want only \"gotchas\" — .knomit must not be listed", body.Children)
		}
	})
}
