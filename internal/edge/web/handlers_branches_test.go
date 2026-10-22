package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"knomit/internal/edge/web/hal"
)

func TestHandleHALBranches_ReturnsCollection(t *testing.T) {
	// Wire a manager with one repo.
	m, _ := newRealStoreManager(t, "alpha")
	s := &Server{Manager: m}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/alpha/branches", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != hal.ContentType {
		t.Errorf("content-type: got %q, want %q", got, hal.ContentType)
	}

	var body struct {
		Count    int         `json:"count"`
		Links    hal.LinkMap `json:"_links"`
		Embedded struct {
			Branches []struct {
				Name  string      `json:"name"`
				Links hal.LinkMap `json:"_links"`
			} `json:"branches"`
		} `json:"_embedded"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Count != 2 {
		t.Errorf("count: %d", body.Count)
	}
	if len(body.Embedded.Branches) != 2 {
		t.Fatalf("embedded: %d", len(body.Embedded.Branches))
	}

	// The branch named "agent/test" must appear with its self link
	// URL-encoded to "agent:test".
	found := false
	for _, br := range body.Embedded.Branches {
		if br.Name == "agent/test" {
			found = true
			want := APIBase + "/repos/alpha/branches/agent:test"
			if got := br.Links["self"].Href; got != want {
				t.Errorf("self: got %q, want %q", got, want)
			}
		}
	}
	if !found {
		t.Error("agent/test not in collection")
	}
}

func TestHandleHALBranch_ReturnsStatusAndFullLinkMap(t *testing.T) {
	m, svcs := newRealStoreManager(t, "alpha")
	head, err := svcs["alpha"].HeadCommit(context.Background(), "agent/test")
	if err != nil || head == "" {
		t.Fatalf("fixture: head of agent/test = %q, %v", head, err)
	}
	s := &Server{
		Manager:           m,
		EmbeddingsEnabled: true,
		AgentBranch:       "agent/test",
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/alpha/branches/agent:test", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != hal.ContentType {
		t.Errorf("content-type: got %q, want %q", got, hal.ContentType)
	}

	var body struct {
		Name              string      `json:"name"`
		Head              string      `json:"head"`
		IndexCommit       string      `json:"index_commit"`
		EmbeddingsEnabled bool        `json:"embeddings_enabled"`
		IsAgentBranch     bool        `json:"is_agent_branch"`
		Links             hal.LinkMap `json:"_links"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if body.Name != "agent/test" {
		t.Errorf("name: %q (must be decoded)", body.Name)
	}
	if body.Head != head {
		t.Errorf("head: %q, want the store's head %q", body.Head, head)
	}
	if !body.EmbeddingsEnabled {
		t.Error("embeddings_enabled should be true")
	}
	if !body.IsAgentBranch {
		t.Error("is_agent_branch should be true when branch matches Server.AgentBranch")
	}

	// Full link set per design spec §3 branch-root example.
	wantRels := []string{
		"self", "facts", "topics", "commits", "search",
		"domains", "stats", "events", "synthesis-runs",
		"index-rebuilds", "mcp", "repo",
	}
	for _, rel := range wantRels {
		if _, ok := body.Links[rel]; !ok {
			t.Errorf("missing link %q", rel)
		}
	}
	// self must be branch-anchored, no commit.
	wantSelf := APIBase + "/repos/alpha/branches/agent:test"
	if got := body.Links["self"].Href; got != wantSelf {
		t.Errorf("self: got %q, want %q", got, wantSelf)
	}
}

func TestHandleHALBranch_UnknownRepoReturns404(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t)}
	r := s.NewAPIRouter()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/missing/branches/main", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status: %d", rec.Code)
	}
}

// TestIndexPercent pins the API's done/total → percent mapping: "ready" is
// always 100; "indexing"/"error" report done/total with a total==0 guard and
// a 100 clamp.
func TestIndexPercent(t *testing.T) {
	cases := []struct {
		state     string
		done, tot int
		want      int
	}{
		{"ready", 0, 0, 100},  // ready is always complete, even with no progress recorded
		{"ready", 3, 10, 100}, // ready overrides any stale progress
		{"indexing", 0, 0, 0}, // unknown total
		{"indexing", 0, 100, 0},
		{"indexing", 50, 100, 50},
		{"indexing", 568, 568, 100},
		{"indexing", 600, 568, 100}, // clamp: never exceed 100
		{"error", 25, 100, 25},      // error reports how far it got
	}
	for _, c := range cases {
		if got := indexPercent(c.state, c.done, c.tot); got != c.want {
			t.Errorf("indexPercent(%q, %d, %d) = %d, want %d", c.state, c.done, c.tot, got, c.want)
		}
	}
}
