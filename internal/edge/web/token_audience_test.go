package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/corpus/repos/control"
	"knomit/internal/edge/oauth"
	"knomit/internal/foundation/auth"
	"knomit/internal/foundation/config"
)

// The MCP side of a token's audience (the mcp-token-audience audit, gaps G1,
// G2 and G4), end to end through the OAuth listener from a LOOPBACK peer
// (kb/invariants/oauth/router-never-runs-authmiddleware: a loopback caller is
// where a wrong edge would turn the token into anonymous).

const (
	lensEngMCP = "/api/v1/lenses/eng/mcp"
	betaMCP    = "/api/v1/repos/beta/branches/agent:test/mcp"
)

// newAudienceLensFixture is the OAuth fixture over real repos alpha, beta and
// gamma and the lens eng = {alpha (write), gamma}. beta is in no lens.
func newAudienceLensFixture(t *testing.T) (*oauthWebFixture, *repos.Manager) {
	t.Helper()
	f := newOAuthWebFixture(t, testIssuer)
	m, _ := newTestLensManager(t, "alpha", "beta", "gamma")
	_, err := m.CreateLens(context.Background(), control.Lens{
		Name: "eng", WriteUID: m.Get("alpha").UID(),
		Reads: []control.LensRead{{RepoUID: m.Get("gamma").UID()}},
	})
	require.NoError(t, err)
	f.s.Manager = m
	return f, m
}

// reposCatalogue calls knomit_repos at mount and returns the repo names, the
// lens names and the raw text.
func reposCatalogue(t *testing.T, h http.Handler, mount string) (reposNames, lensNames []string, raw string) {
	t.Helper()
	_, sid := rpcAt(t, h, mount, "", initBody)
	text, isErr := callToolAt(t, h, mount, sid, "knomit_repos", `{}`)
	require.False(t, isErr, text)
	var out struct {
		Repos  []struct{ Name string }  `json:"repos"`
		Lenses *[]struct{ Name string } `json:"lenses"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &out), text)
	for _, r := range out.Repos {
		reposNames = append(reposNames, r.Name)
	}
	require.NotNil(t, out.Lenses, "the lens registry answered, so `lenses` is present: %s", text)
	lensNames = []string{}
	for _, l := range *out.Lenses {
		lensNames = append(lensNames, l.Name)
	}
	sort.Strings(reposNames)
	return reposNames, lensNames, text
}

// G1: knomit_repos lists what the caller's credential may address, and no
// more. A repo token sees its repo; a lens token sees its lens and the lens's
// members (which it reaches through the lens, not by name); the unscoped
// endpoint's token and a local caller see everything, as before.
//
// Sabotage: make reposInReach / lensesInReach keep every row and the repo and
// lens tokens list beta (and the repo token eng).
func TestOAuthMCP_ReposListsOnlyWhatTheTokenReaches(t *testing.T) {
	f, m := newAudienceLensFixture(t)
	alphaMount := "/api/v1/repos/alpha/branches/" + strings.ReplaceAll(m.Get("alpha").AgentBranch(), "/", ":") + "/mcp"

	for _, tc := range []struct {
		name, resource, mount string
		repos, lenses         []string
	}{
		{"repo token", testIssuer + alphaMount, alphaMount, []string{"alpha"}, []string{}},
		{"lens token", testIssuer + lensEngMCP, lensEngMCP, []string{"alpha", "gamma"}, []string{"eng"}},
		{"unscoped token", testIssuer + "/api/v1/mcp", "/api/v1/mcp", []string{"alpha", "beta", "gamma"}, []string{"eng"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tok := f.mint(t, "laptop-"+strings.ReplaceAll(tc.name, " ", "-"), tc.resource, []string{"read"}, auth.Read)
			h := withAuthorization(f.s.OAuthHandler(), "Bearer "+tok)
			gotRepos, gotLenses, raw := reposCatalogue(t, h, tc.mount)
			require.Equal(t, tc.repos, gotRepos, raw)
			require.Equal(t, tc.lenses, gotLenses, raw)
			if len(tc.repos) < 3 {
				require.NotContains(t, raw, `"beta"`, "beta is outside this audience and must not appear at all")
			}
		})
	}

	// A local caller (the plain listener, loopback): everything.
	gotRepos, gotLenses, _ := reposCatalogue(t, f.s.NewAPIRouter(), "/mcp")
	require.Equal(t, []string{"alpha", "beta", "gamma"}, gotRepos)
	require.Equal(t, []string{"eng"}, gotLenses)
}

// G4: a lens token may add to its lens only repos it can already read — and
// it reaches no repo by name, so it can add none. A repo it does not reach
// is answered exactly like a uid no repo has. Re-sending the lens's own
// members, dropping one, or editing the description stays open to it, and a
// local caller may add any repo.
//
// Sabotage: drop the ReachesRepo check in checkLensMembers and the lens
// token's PATCH adding beta succeeds (200), after which eng's MCP endpoint
// would read beta.
func TestOAuthLensPatch_LensTokenCannotAddARepoItDoesNotReach(t *testing.T) {
	f, m := newAudienceLensFixture(t)
	tok := f.mint(t, "lens-writer", testIssuer+"/api/v1/lenses/eng", []string{"read", "write"}, auth.Read, auth.Write)
	h := withAuthorization(f.s.OAuthHandler(), "Bearer "+tok)
	patch := func(h http.Handler, path, body string) *httptest.ResponseRecorder {
		return serve(h, fromLoopback(newJSONRequest(http.MethodPatch, path, bytes.NewBufferString(body))))
	}
	uid := func(name string) string { return m.Get(name).UID() }
	readsOf := func() []string {
		l, ok, err := m.LensRegistry().Get("eng")
		require.NoError(t, err)
		require.True(t, ok)
		out := []string{}
		for _, rd := range l.Reads {
			out = append(out, m.GetByUID(rd.RepoUID).Name())
		}
		sort.Strings(out)
		return out
	}

	addBeta := patch(h, "/api/v1/lenses/eng", fmt.Sprintf(`{"reads":[{"uid":%q},{"uid":%q}]}`, uid("gamma"), uid("beta")))
	require.Equal(t, http.StatusBadRequest, addBeta.Code, addBeta.Body.String())
	unknown := patch(h, "/api/v1/lenses/eng", fmt.Sprintf(`{"reads":[{"uid":%q},{"uid":"no-such-uid"}]}`, uid("gamma")))
	require.Equal(t, http.StatusBadRequest, unknown.Code, unknown.Body.String())
	require.Equal(t,
		strings.ReplaceAll(unknown.Body.String(), "no-such-uid", "X"),
		strings.ReplaceAll(addBeta.Body.String(), uid("beta"), "X"),
		"an unreachable repo must read exactly like a nonexistent one")
	makeBetaWrite := patch(h, "/api/v1/lenses/eng", fmt.Sprintf(`{"write":{"uid":%q}}`, uid("beta")))
	require.Equal(t, http.StatusBadRequest, makeBetaWrite.Code, "the write repo is a member too: %s", makeBetaWrite.Body.String())
	require.NotContains(t, readsOf(), "beta")

	// What the lens already has stays editable by its own token.
	same := patch(h, "/api/v1/lenses/eng", fmt.Sprintf(`{"write":{"uid":%q},"reads":[{"uid":%q}],"description":"d"}`, uid("alpha"), uid("gamma")))
	require.Equal(t, http.StatusOK, same.Code, same.Body.String())
	drop := patch(h, "/api/v1/lenses/eng", `{"reads":[]}`)
	require.Equal(t, http.StatusOK, drop.Code, drop.Body.String())
	require.Equal(t, []string{"alpha"}, readsOf(), "gamma dropped, alpha is the write repo")

	// A local caller reaches every repo and may add beta.
	local := patch(f.s.NewAPIRouter(), "/lenses/eng", fmt.Sprintf(`{"reads":[{"uid":%q}]}`, uid("beta")))
	require.Equal(t, http.StatusOK, local.Code, local.Body.String())
	require.Equal(t, []string{"alpha", "beta"}, readsOf())
}

// G4, create: a token for /api/v1/lenses reaches every lens but no repo by
// name, so it cannot build a lens over any repo either.
func TestOAuthLensCreate_NeedsTheMembersInReach(t *testing.T) {
	f, m := newAudienceLensFixture(t)
	tok := f.mint(t, "lenses-writer", testIssuer+"/api/v1/lenses", []string{"read", "write"}, auth.Read, auth.Write)
	h := withAuthorization(f.s.OAuthHandler(), "Bearer "+tok)
	body := fmt.Sprintf(`{"name":"ops","write":{"uid":%q}}`, m.Get("beta").UID())
	rec := serve(h, fromLoopback(newJSONRequest(http.MethodPost, "/api/v1/lenses", bytes.NewBufferString(body))))
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "Unknown repo uid")

	wide := f.mint(t, "api-writer", testIssuer+"/api/v1", []string{"read", "write"}, auth.Read, auth.Write)
	rec = serve(withAuthorization(f.s.OAuthHandler(), "Bearer "+wide), fromLoopback(newJSONRequest(http.MethodPost, "/api/v1/lenses", bytes.NewBufferString(body))))
	require.Equal(t, http.StatusCreated, rec.Code, "a whole-API token reaches beta: %s", rec.Body.String())
}

// taskCall sends a raw JSON-RPC request and returns its result or its error.
func taskCall(t *testing.T, h http.Handler, mount, sid, method, params string) (map[string]any, map[string]any) {
	t.Helper()
	resp, _ := rpcAt(t, h, mount, sid, fmt.Sprintf(`{"jsonrpc":"2.0","id":31,"method":%q,"params":%s}`, method, params))
	res, _ := resp["result"].(map[string]any)
	errObj, _ := resp["error"].(map[string]any)
	return res, errObj
}

// startTask runs knomit_review as a task at mount and returns its id.
func startTask(t *testing.T, h http.Handler, mount, sid string) string {
	t.Helper()
	res, errObj := taskCall(t, h, mount, sid, "tools/call", `{"name":"knomit_review","arguments":{},"task":{"ttl":60000}}`)
	require.Nil(t, errObj)
	task, _ := res["task"].(map[string]any)
	id, _ := task["taskId"].(string)
	require.NotEmpty(t, id, "not a task-augmented dispatch: %v", res)
	return id
}

// listedTasks returns the task ids tasks/list shows at mount.
func listedTasks(t *testing.T, h http.Handler, mount, sid string) []string {
	t.Helper()
	res, errObj := taskCall(t, h, mount, sid, "tasks/list", `{}`)
	require.Nil(t, errObj, "tasks/list: %v", errObj)
	out := []string{}
	tasks, _ := res["tasks"].([]any)
	for _, tk := range tasks {
		if m, ok := tk.(map[string]any); ok {
			out = append(out, fmt.Sprint(m["taskId"]))
		}
	}
	return out
}

// G2: an MCP task belongs to the endpoint AND the principal that created it.
// mcp-go isolates tasks by MCP session id only, and knomit accepts any
// well-formed session id, so the session id alone is not an owner. A caller
// presenting the creator's session id — another principal on the same
// endpoint, or the same principal through another endpoint — gets "task not
// found" from tasks/get, tasks/result and tasks/cancel, byte for byte what a
// task id that never existed gets, and tasks/list leaves the task out. The
// owner keeps all four.
//
// Sabotage (each separately): drop the owner refusal in the request
// initialization hook (owner.go) — the three foreign calls answer the task;
// drop the AfterListTasks filter — the foreign lists show it.
func TestOAuthMCP_TasksAreOwnedByEndpointAndPrincipal(t *testing.T) {
	f := newOAuthWebFixture(t, testIssuer)
	f.s.Manager = newTestManagerWithRepos(t, "alpha", "beta")
	tokenH := func(subject, resource string) http.Handler {
		return withAuthorization(f.s.OAuthHandler(), "Bearer "+f.mint(t, subject, resource, []string{"read", "write"}, auth.Read, auth.Write))
	}
	ownerH := tokenH("owner", testIssuer+"/api/v1")
	_, sid := rpcAt(t, ownerH, betaMCP, "", initBody)
	id := startTask(t, ownerH, betaMCP, sid)
	params := fmt.Sprintf(`{"taskId":%q}`, id)

	_, ghost := taskCall(t, ownerH, betaMCP, sid, "tasks/get", `{"taskId":"00000000-0000-4000-8000-000000000000"}`)
	require.NotNil(t, ghost, "an unknown task id is an error")
	require.Equal(t, "task not found", ghost["message"])

	for _, tc := range []struct {
		name  string
		h     http.Handler
		mount string
	}{
		{"another principal, same endpoint", tokenH("other", testIssuer+"/api/v1"), betaMCP},
		{"same principal, another endpoint", ownerH, alphaMCP},
		{"another repo's token, its own endpoint", tokenH("alpha-only", testIssuer+alphaMCP), alphaMCP},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, method := range []string{"tasks/get", "tasks/result", "tasks/cancel"} {
				res, errObj := taskCall(t, tc.h, tc.mount, sid, method, params)
				require.Nil(t, res, "%s answered another owner's task: %v", method, res)
				require.Equal(t, ghost, errObj, "%s: must read exactly like a task that never existed", method)
			}
			require.NotContains(t, listedTasks(t, tc.h, tc.mount, sid), id)
		})
	}

	// The owner, on its own endpoint, still has it (and the foreign cancel
	// above did not reach it).
	res, errObj := taskCall(t, ownerH, betaMCP, sid, "tasks/get", params)
	require.Nil(t, errObj)
	require.Equal(t, id, res["taskId"])
	require.Contains(t, listedTasks(t, ownerH, betaMCP, sid), id)
	res, errObj = taskCall(t, ownerH, betaMCP, sid, "tasks/result", params)
	require.Nil(t, errObj)
	require.NotNil(t, res["content"])
}

// The lens reach: recorded at every principal-minting edge, from the token's
// audience on the OAuth listener, and copied into the MCP context with the
// endpoint.
func TestEveryAuthEdgeRecordsALensReach(t *testing.T) {
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out := ""
		if auth.ReachesLens(r.Context(), "eng") {
			out += "e"
		}
		if auth.ReachesLens(r.Context(), "ops") {
			out += "o"
		}
		_, _ = w.Write([]byte(out))
	})
	local := AuthMiddleware(config.AuthConfig{Require: false}, false)(echo)
	if rr := serve(local, fromLoopback(httptest.NewRequest("GET", "/x", nil))); rr.Body.String() != "eo" {
		t.Fatalf("a local caller reaches every lens: %q", rr.Body.String())
	}

	f := newOAuthWebFixture(t, testIssuer)
	bearer := BearerMiddleware(oauth.NewVerifier(testIssuer, f.store), testIssuer)(echo)
	for _, tc := range []struct{ resource, path, want string }{
		{testIssuer + lensEngMCP, lensEngMCP, "e"},
		{testIssuer + "/api/v1/lenses/eng", "/api/v1/lenses/eng", "e"},
		{testIssuer + "/api/v1/lenses/en", "/api/v1/lenses/en/mcp", ""}, // a "/" boundary
		{testIssuer + "/api/v1/lenses", "/api/v1/lenses", "eo"},
		{testIssuer + alphaMCP, alphaMCP, ""}, // a repo token reaches no lens
		{testIssuer + "/api/v1/mcp", "/api/v1/mcp", "eo"},
		{testIssuer + "/api/v1", lensEngMCP, "eo"},
	} {
		tok := f.mint(t, "laptop", tc.resource, []string{"read"}, auth.Read)
		req := httptest.NewRequest("GET", tc.path, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		if rr := serve(bearer, req); rr.Code != http.StatusOK || rr.Body.String() != tc.want {
			t.Fatalf("token for %s: code=%d reach=%q, want %q", tc.resource, rr.Code, rr.Body.String(), tc.want)
		}
	}

	r := httptest.NewRequest("POST", lensEngMCP, nil)
	r = r.WithContext(auth.WithLensReach(r.Context(), func(l string) bool { return l == "eng" }))
	ctx := mcpHTTPContext(context.Background(), r)
	if !auth.ReachesLens(ctx, "eng") || auth.ReachesLens(ctx, "ops") {
		t.Fatal("the request's lens reach must be copied, unchanged")
	}
}
