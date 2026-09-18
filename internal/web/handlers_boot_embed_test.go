package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"knomit/internal/repos"
	"knomit/internal/web/hal"
)

// bootEmbedServer wires a server whose branch-root reader is a stub, so the
// embed tests do not depend on a live store.
func bootEmbedServer(t *testing.T, m *repos.Manager, readErr error) *Server {
	t.Helper()
	return &Server{
		Manager:           m,
		AgentBranch:       "agent/test",
		EmbeddingsEnabled: true,
		providers: storeProviders{
			branchRootReader: func(_ context.Context, _ *repos.RepoInstance, name string) (branchRootInfo, error) {
				if readErr != nil {
					return branchRootInfo{}, readErr
				}
				return branchRootInfo{Head: "7f3a8b2c", IndexCommit: "7f3a8b2c"}, nil
			},
		},
	}
}

func getJSON(t *testing.T, h http.Handler, path string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	var body map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("GET %s: body is not JSON: %v\n%s", path, err, rec.Body.String())
		}
	}
	return rec.Code, body
}

// embeddedBranchOf digs out _embedded.<key>, failing the test if the shape is
// wrong rather than returning a confusing nil.
func embeddedBranchOf(t *testing.T, body map[string]any, key string) (map[string]any, bool) {
	t.Helper()
	emb, ok := body["_embedded"]
	if !ok {
		return nil, false
	}
	embMap, ok := emb.(map[string]any)
	if !ok {
		t.Fatalf("_embedded is %T, want an object", emb)
	}
	v, ok := embMap[key]
	if !ok {
		return nil, false
	}
	vMap, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("_embedded.%s is %T, want an object", key, v)
	}
	return vMap, true
}

// Test 1. The embed must be the branch GET's body, not a second rendering of
// it. Two implementations kept in step by hand is the failure this shares a
// builder to avoid, so the assertion is a deep equality against the real
// branch handler for the same fixture.
func TestRepoGET_EmbedsTheReadBranchRootByteForByte(t *testing.T) {
	s := bootEmbedServer(t, newTestManagerWithRepos(t, "alpha"), nil)
	r := s.NewAPIRouter()

	code, repoBody := getJSON(t, r, "/repos/alpha")
	if code != http.StatusOK {
		t.Fatalf("repo GET status: got %d, want 200", code)
	}
	embedded, ok := embeddedBranchOf(t, repoBody, "branch")
	if !ok {
		t.Fatalf("repo GET has no _embedded.branch: %v", repoBody)
	}

	code, branchBody := getJSON(t, r, "/repos/alpha/branches/agent:test")
	if code != http.StatusOK {
		t.Fatalf("branch GET status: got %d, want 200", code)
	}

	if !reflect.DeepEqual(embedded, branchBody) {
		t.Errorf("_embedded.branch differs from the branch GET body.\nembedded: %#v\nbranch:   %#v", embedded, branchBody)
	}
	// The embed describes the READ branch, which is what repoView advertises.
	if got, want := embedded["name"], repoBody["read_branch"]; got != want {
		t.Errorf("embedded branch name: got %v, want the read_branch %v", got, want)
	}
}

// Test 4c. Proves the embedded hrefs came from hal.URLBuilder rather than
// being hand-built: a hand-built href is the way this silently diverges.
func TestRepoGET_EmbeddedLinksComeFromTheURLBuilder(t *testing.T) {
	s := bootEmbedServer(t, newTestManagerWithRepos(t, "alpha"), nil)
	r := s.NewAPIRouter()

	_, repoBody := getJSON(t, r, "/repos/alpha")
	embedded, ok := embeddedBranchOf(t, repoBody, "branch")
	if !ok {
		t.Fatal("repo GET has no _embedded.branch")
	}
	_, branchBody := getJSON(t, r, "/repos/alpha/branches/agent:test")

	embLinks, _ := embedded["_links"].(map[string]any)
	brLinks, _ := branchBody["_links"].(map[string]any)
	if embLinks == nil || brLinks == nil {
		t.Fatalf("missing _links: embedded=%v branch=%v", embedded["_links"], branchBody["_links"])
	}
	for _, rel := range []string{"self", "facts", "topics", "stats", "events", "repo"} {
		if embLinks[rel] == nil {
			t.Errorf("embedded _links.%s is missing", rel)
			continue
		}
		if !reflect.DeepEqual(embLinks[rel], brLinks[rel]) {
			t.Errorf("_links.%s: embedded %v, branch GET %v", rel, embLinks[rel], brLinks[rel])
		}
	}
}

// Test 2. A subscription has no agent branch; the content it serves comes
// from the followed upstream, so that is the root the repo GET must embed.
// See kb/conventions/repos/subscription/read-branch-for-content.
func TestRepoGET_SubscriptionEmbedsTheFollowedUpstreamRoot(t *testing.T) {
	m := repos.New(context.Background(), repos.Deps{})
	m.Set("sub", repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "sub", Subscribed: true, ReadBranch: "main",
	}))
	s := bootEmbedServer(t, m, nil)
	r := s.NewAPIRouter()

	code, repoBody := getJSON(t, r, "/repos/sub")
	if code != http.StatusOK {
		t.Fatalf("repo GET status: got %d, want 200", code)
	}
	if _, present := repoBody["agent_branch"]; present {
		t.Errorf("agent_branch must stay omitted for a subscription, got %v", repoBody["agent_branch"])
	}
	if got := repoBody["read_branch"]; got != "main" {
		t.Fatalf("read_branch: got %v, want main", got)
	}

	embedded, ok := embeddedBranchOf(t, repoBody, "branch")
	if !ok {
		t.Fatalf("subscription repo GET has no _embedded.branch: %v", repoBody)
	}
	if got := embedded["name"]; got != "main" {
		t.Errorf("embedded branch name: got %v, want main — the followed upstream, not the agent branch", got)
	}
}

// Test 3. The repo GET has to keep answering for a repo whose store is still
// opening. The embed is an optimisation; losing it must cost the client a
// round trip, never the response.
func TestRepoGET_OmitsEmbedWhenTheBranchRootIsUnreadable(t *testing.T) {
	s := bootEmbedServer(t, newTestManagerWithRepos(t, "alpha"), errors.New("store not open"))
	r := s.NewAPIRouter()

	code, repoBody := getJSON(t, r, "/repos/alpha")
	if code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 — an unreadable branch root must not fail the repo GET", code)
	}
	if _, ok := embeddedBranchOf(t, repoBody, "branch"); ok {
		t.Error("_embedded.branch is present although the branch root reader failed")
	}
	// The rest of the body is unchanged.
	for _, key := range []string{"name", "uid", "id", "read_branch", "_links"} {
		if _, ok := repoBody[key]; !ok {
			t.Errorf("repo body lost %q when the embed was omitted", key)
		}
	}
	if got := repoBody["name"]; got != "alpha" {
		t.Errorf("name: got %v, want alpha", got)
	}
}

// An unopened store is not an ERROR from defaultBranchRootReader — WithRead
// never runs its callback and a zero branchRootInfo comes back with a nil
// error. Embedding that would advertise a branch root with head "" as though
// it were known, which is worse than omitting it: the client would trust it
// and never ask.
func TestRepoGET_OmitsEmbedWhenNoHeadResolves(t *testing.T) {
	s := &Server{
		Manager:     newTestManagerWithRepos(t, "alpha"),
		AgentBranch: "agent/test",
		providers: storeProviders{
			branchRootReader: func(_ context.Context, _ *repos.RepoInstance, _ string) (branchRootInfo, error) {
				return branchRootInfo{}, nil // what an unopened store produces
			},
		},
	}
	code, repoBody := getJSON(t, s.NewAPIRouter(), "/repos/alpha")

	if code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", code)
	}
	if _, ok := embeddedBranchOf(t, repoBody, "branch"); ok {
		t.Error("_embedded.branch is present with no head resolved — the client would trust an empty root")
	}
}

// Test 4. The lens embed answers a WRITE question — where this lens writes —
// so it is the write member's AGENT branch, not its read branch.
func TestLensGET_EmbedsTheWriteMembersAgentBranchRoot(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	s := &Server{Manager: m, AgentBranch: "machine/test"}
	r := s.NewAPIRouter()

	if rec := postLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`); rec.Code != http.StatusCreated {
		t.Fatalf("seed create: %d body=%s", rec.Code, rec.Body.String())
	}

	code, body := getJSON(t, r, "/lenses/eng")
	if code != http.StatusOK {
		t.Fatalf("lens GET status: got %d, want 200", code)
	}
	embedded, ok := embeddedBranchOf(t, body, "write_branch")
	if !ok {
		t.Fatalf("lens GET has no _embedded.write_branch: %v", body)
	}

	want := m.Get("alpha").AgentBranch()
	if want == "" {
		t.Fatal("fixture: alpha has no agent branch")
	}
	if got := embedded["name"]; got != want {
		t.Errorf("embedded write_branch name: got %v, want the write member's agent branch %q", got, want)
	}
	// Same body the branch GET serves, by construction.
	_, branchBody := getJSON(t, r, "/repos/alpha/branches/"+hal.EncodeBranch(want))
	if !reflect.DeepEqual(embedded, branchBody) {
		t.Errorf("_embedded.write_branch differs from the branch GET body.\nembedded: %#v\nbranch:   %#v", embedded, branchBody)
	}
}

// Test 4b (named, not a fallthrough). A lens whose write member is a
// SUBSCRIPTION has no agent branch at all — subscribed is true only alongside
// an empty agentBranch (kb/invariants/repos/subscription/flag-and-branch-paired).
// The key must be ABSENT: an empty-string branch would read as "unknown" and
// would build a root for branch "".
func TestLensGET_SubscriptionWriteMemberHasNoWriteBranchKey(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	s := &Server{Manager: m, AgentBranch: "machine/test"}
	r := s.NewAPIRouter()

	if rec := postLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`); rec.Code != http.StatusCreated {
		t.Fatalf("seed create: %d body=%s", rec.Code, rec.Body.String())
	}

	// Swap the write member for a subscription carrying the SAME uid, so lens
	// membership still resolves but the instance has no agent branch.
	uid := m.Get("alpha").UID()
	if uid == "" {
		t.Fatal("fixture: alpha has no uid")
	}
	m.Set("alpha", repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "alpha", UID: uid, Subscribed: true, ReadBranch: "main",
	}))

	code, body := getJSON(t, r, "/lenses/eng")
	if code != http.StatusOK {
		t.Fatalf("lens GET status: got %d, want 200", code)
	}
	if emb, ok := body["_embedded"]; ok {
		if embMap, isMap := emb.(map[string]any); isMap {
			if v, present := embMap["write_branch"]; present {
				t.Errorf("_embedded.write_branch is present for a subscription write member: %v", v)
			}
		}
	}
	// The rest of the lens body is unchanged.
	if body["name"] != "eng" {
		t.Errorf("name: got %v, want eng", body["name"])
	}
	if _, ok := body["reads"]; !ok {
		t.Error("lens body lost reads")
	}
}
