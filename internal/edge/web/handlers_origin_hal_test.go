package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"

	"knomit/internal/corpus/repos/control"
	"knomit/internal/domain/fact"
	"knomit/internal/edge/web/hal"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/remotesync"
	"knomit/internal/engine/store/storetest"
	"knomit/internal/foundation/config"
	"knomit/internal/platform/fileuri"
	"knomit/internal/testsupport/testsigner"
)

// injectOrigin gives a real store the origin its Manager would inject from
// control.db at mount (svc.Remote().SetOrigin), so GET /origin and GET /repos
// read real store state: the injected connection plus the store's own status
// row.
func injectOrigin(svc *store.Service, url, branch, authMethod string) {
	svc.Remote().SetOrigin(&remotesync.Origin{URL: url, Branch: branch, AuthMethod: authMethod, AuthToken: "tok-" + branch})
}

// originProblem decodes an application/problem+json body.
func originProblem(t *testing.T, rec *httptest.ResponseRecorder) hal.Problem {
	t.Helper()
	require.Equal(t, "application/problem+json", rec.Header().Get("Content-Type"), "body=%s", rec.Body.String())
	var p hal.Problem
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
	return p
}

// GET /repos/{repo}/origin answers the REQUESTED repo's origin, every field
// read from its store: two repos with distinct origins, the requested one on
// an upstream branch that is neither "main" nor the agent branch, with a sync
// error recorded and no push yet.
func TestHandleHALGetOrigin_ReturnsOriginData(t *testing.T) {
	m, svcs := newRealStoreManager(t, "alpha", "beta")
	injectOrigin(svcs["alpha"], "https://x.test/alpha.git", "main", "none")
	injectOrigin(svcs["beta"], "https://x.test/beta.git", "trunk", "token")
	require.NoError(t, svcs["beta"].Remote().RecordSyncError("origin", "fetch refused"))
	require.NotEqual(t, "trunk", m.Get("beta").AgentBranch(), "precondition: upstream is not the agent branch")
	s := &Server{Manager: m}

	rec := httptest.NewRecorder()
	s.NewAPIRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/repos/beta/origin", nil))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	require.Equal(t, hal.ContentType, rec.Header().Get("Content-Type"))

	var body struct {
		Name           string      `json:"name"`
		URL            string      `json:"url"`
		Branch         string      `json:"branch"`
		AuthMethod     string      `json:"auth_method"`
		LastStatus     *string     `json:"last_status"`
		LastError      *string     `json:"last_error"`
		LastPushStatus *string     `json:"last_push_status"`
		Links          hal.LinkMap `json:"_links"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "origin", body.Name)
	require.Equal(t, "https://x.test/beta.git", body.URL)
	require.Equal(t, "trunk", body.Branch)
	require.Equal(t, "token", body.AuthMethod)
	require.NotNil(t, body.LastStatus)
	require.Equal(t, "error", *body.LastStatus)
	require.NotNil(t, body.LastError)
	require.Equal(t, "fetch refused", *body.LastError)
	require.Nil(t, body.LastPushStatus, "no push has run: null, not a value")
	require.Equal(t, "/api/v1/repos/beta/origin", body.Links["self"].Href)
	require.Equal(t, "/api/v1/repos/beta", body.Links["repo"].Href)
	require.NotContains(t, rec.Body.String(), "tok-trunk", "the view never carries the credential")
}

// The origin view carries both circuit breakers (F21 S1), always present:
// closed with no failures and a null open_until before the loop's first
// round. Sabotage: drop either field from originView → red.
func TestHandleHALGetOrigin_CarriesBreakers(t *testing.T) {
	m, svcs := newRealStoreManager(t, "alpha")
	injectOrigin(svcs["alpha"], "https://x.test/kb.git", "main", "none")
	s := &Server{Manager: m}
	rec := httptest.NewRecorder()
	s.NewAPIRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/repos/alpha/origin", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"fetch_breaker", "push_breaker"} {
		raw, ok := body[k]
		if !ok {
			t.Fatalf("missing %s in %s", k, rec.Body.String())
		}
		var b map[string]any
		if err := json.Unmarshal(raw, &b); err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		if b["state"] != "closed" || b["consecutive_failures"] != float64(0) {
			t.Errorf("%s: got %v, want closed with 0 failures", k, b)
		}
		if v, ok := b["open_until"]; !ok || v != nil {
			t.Errorf("%s: open_until must be present and null while closed, got %v", k, b)
		}
	}
}

// A repo with no injected origin answers 204 with no body, even when another
// repo has one.
func TestHandleHALGetOrigin_NoOrigin_Returns204(t *testing.T) {
	m, svcs := newRealStoreManager(t, "alpha", "beta")
	injectOrigin(svcs["beta"], "https://x.test/beta.git", "main", "none")
	s := &Server{Manager: m}

	rec := httptest.NewRecorder()
	s.NewAPIRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/repos/alpha/origin", nil))
	require.Equal(t, http.StatusNoContent, rec.Code, "body=%s", rec.Body.String())
	require.Empty(t, rec.Body.String())
}

// A repo whose store cannot be acquired is the one store-unavailable 503, not a
// 204: "no origin" must not be read off a store nobody could ask. (It was a 500
// "Failed to get origin" carrying the error text before the 2026-10-24 ruling.)
func TestHandleHALGetOrigin_StorelessRepoIs503(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t, "alpha")} // no store behind alpha
	rec := httptest.NewRecorder()
	s.NewAPIRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/repos/alpha/origin", nil))
	requireUnavailableProblem(t, rec, "/repos/alpha/origin")
}

func TestHandleHALGetOrigin_UnknownRepo_Returns404(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t, "alpha")}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/missing/origin", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: got %q, want application/problem+json", got)
	}
}

func TestHandleHALSetOrigin_Returns200(t *testing.T) {
	originsRoot := t.TempDir()
	s, _, ri := newControlDBTestServer(t, originsRoot)
	r := s.NewAPIRouter()
	url := seedBareRemoteForTest(t, filepath.Join(originsRoot, "upstream.git"))

	rec := putOrigin(t, r, "alpha", `{"url":"`+url+`","branch":"main","auth_method":"none"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != hal.ContentType {
		t.Errorf("content-type: got %q, want %q", got, hal.ContentType)
	}
	// The reply comes after the Sync stage re-entered on the new origin.
	if got := ri.Status().Sync.Origin; got != url {
		t.Errorf("Sync follows %q, want %q", got, url)
	}
}

// putOrigin sends PUT /repos/{repo}/origin with a JSON body.
func putOrigin(t *testing.T, r http.Handler, repo, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := fromLoopback(httptest.NewRequest(http.MethodPut, "/repos/"+repo+"/origin", strings.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	return rec
}

// TestHandleHALSetOrigin_LocalOriginGate pins that PUT /origin rejects a local
// filesystem origin when local origins are disabled (no LocalOriginRoot). The
// gate lives in the handler (which holds the real Manager) and runs before
// the stored origin is read: alpha here has no store at all, and the answer is
// still the policy's 400, not the 503 a store read would give. Regression for
// an earlier fail-open design where enforcement lived in a store-access layer
// that could be constructed without it.
func TestHandleHALSetOrigin_LocalOriginGate(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t, "alpha")} // Deps{} → no LocalOriginRoot
	r := s.NewAPIRouter()

	rec := putOrigin(t, r, "alpha", `{"url":"/etc/passwd","auth_method":"none"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	require.Equal(t, "Origin not allowed", originProblem(t, rec).Title)
}

// The stored origin is read before the attach (an empty field keeps what is
// stored); a repo whose store cannot be acquired is the one store-unavailable
// 503 (it was 503 "No store available" with the error text as detail).
func TestHandleHALSetOrigin_StorelessRepoIs503(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t, "alpha")} // no store behind alpha
	rec := putOrigin(t, s.NewAPIRouter(), "alpha", `{"url":"https://x.test/kb.git","auth_method":"none"}`)
	requireUnavailableProblem(t, rec, "/repos/alpha/origin")
}

// A PUT that names neither url, auth_method nor token keeps all three from
// the stored origin getOrigin read: the attach goes to the stored url with the
// stored credential, and the record is unchanged.
func TestHandleHALSetOrigin_PartialUpdateKeepsTheStoredOrigin(t *testing.T) {
	originsRoot := t.TempDir()
	s, m, ri := newControlDBTestServer(t, originsRoot)
	r := s.NewAPIRouter()
	url := seedBareRemoteForTest(t, filepath.Join(originsRoot, "upstream.git"))

	rec := putOrigin(t, r, "alpha", `{"url":"`+url+`","branch":"main","auth_method":"token","token":"tok-stored"}`)
	require.Equal(t, http.StatusOK, rec.Code, "first PUT: body=%s", rec.Body.String())

	rec = putOrigin(t, r, "alpha", `{"branch":"main"}`)
	require.Equal(t, http.StatusOK, rec.Code, "partial PUT: body=%s", rec.Body.String())
	o, err := m.Origins().Get(ri.UID())
	require.NoError(t, err)
	require.NotNil(t, o)
	require.Equal(t, url, o.URL)
	require.Equal(t, "token", o.AuthMethod)
	require.Equal(t, "tok-stored", o.AuthToken)
}

// PUT attaches on the branch it was asked for: the remote has both main and
// trunk, and trunk (neither main nor the agent branch) is what is recorded.
func TestHandleHALSetOrigin_AttachesOnTheRequestedBranch(t *testing.T) {
	originsRoot := t.TempDir()
	s, m, ri := newControlDBTestServer(t, originsRoot)
	bare := filepath.Join(originsRoot, "upstream.git")
	url := seedBareRemoteForTest(t, bare)
	runGitForTest(t, bare, "branch", "trunk", "main")
	require.NotEqual(t, "trunk", ri.AgentBranch(), "precondition")

	rec := putOrigin(t, s.NewAPIRouter(), "alpha", `{"url":"`+url+`","branch":"trunk","auth_method":"none"}`)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	o, err := m.Origins().Get(ri.UID())
	require.NoError(t, err)
	require.NotNil(t, o)
	require.Equal(t, url, o.URL)
	require.Equal(t, "trunk", o.Branch)
}

// A stored "cert" auth method is knomit's own decision, not an input: a
// partial PUT does not carry it into the attach (which would refuse it as an
// input) but lets the attach decide again, and the stored url is still reused.
func TestHandleHALSetOrigin_PartialUpdateDoesNotCarryACert(t *testing.T) {
	originsRoot := t.TempDir()
	s, m, ri := newControlDBTestServer(t, originsRoot)
	url := seedBareRemoteForTest(t, filepath.Join(originsRoot, "upstream.git"))
	// The origin as the Manager injects it from a record whose auth was decided
	// as cert.
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		svc.Remote().SetOrigin(&remotesync.Origin{URL: url, Branch: "main", AuthMethod: "cert"})
	}))

	rec := putOrigin(t, s.NewAPIRouter(), "alpha", `{"branch":"main"}`)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	o, err := m.Origins().Get(ri.UID())
	require.NoError(t, err)
	require.NotNil(t, o)
	require.Equal(t, url, o.URL, "the stored url is reused")
	require.NotEqual(t, "cert", o.AuthMethod, "a file:// origin is not decided as cert")
}

func TestHandleHALSetOrigin_UnknownRepo_Returns404(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t, "alpha")}
	r := s.NewAPIRouter()

	body := `{"url":"https://github.com/example/repo.git"}`
	rec := httptest.NewRecorder()
	req := fromLoopback(httptest.NewRequest(http.MethodPut, "/repos/missing/origin", strings.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rec.Code)
	}
}

// A failed durable write answers 500 and changes nothing: the attach's apply
// writes the git remote first, then the record, and puts the git remote back
// when the record cannot be written (TestSetOrigin_FailedPersistRestoresTheGitRemote
// pins the restore).
func TestHandleHALSetOrigin_FailedPersistIs500(t *testing.T) {
	originsRoot := t.TempDir()
	s, m, ri := newControlDBTestServerOpt(t, originsRoot, false) // no key → a token cannot be stored
	r := s.NewAPIRouter()
	url := seedBareRemoteForTest(t, filepath.Join(originsRoot, "upstream.git"))

	rec := putOrigin(t, r, "alpha", `{"url":"`+url+`","branch":"main","auth_method":"token","token":"tok"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	if o, err := m.Origins().Get(ri.UID()); err != nil || o != nil {
		t.Fatalf("nothing may be persisted: %+v %v", o, err)
	}
}

// TestHandleHALSetOrigin_UnreachableIs502AndPersistsNothing pins the
// fail-fast attach: the AttachOrigin guard probes the remote (bounded by the
// network timeout) BEFORE anything is stopped or written, so an unreachable
// remote, a refused credential or a timeout is an immediate 502 "origin was
// not attached" — with NOTHING persisted and the running sync loop untouched.
// It used to persist the row and only then fail the reconcile ("origin was
// saved but the initial reconcile failed").
func TestHandleHALSetOrigin_UnreachableIs502AndPersistsNothing(t *testing.T) {
	originsRoot := t.TempDir()
	s, m, ri := newControlDBTestServer(t, originsRoot)
	r := s.NewAPIRouter()
	syncGen := ri.Status()

	nowhere := fileuri.New(filepath.Join(originsRoot, "nowhere.git"))
	rec := putOrigin(t, r, "alpha", `{"url":"`+nowhere+`","branch":"main","auth_method":"none"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status: got %d, want 502; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: got %q, want application/problem+json", got)
	}
	if !strings.Contains(rec.Body.String(), "origin was not attached") {
		t.Errorf("body should say the origin was not attached; got %s", rec.Body.String())
	}
	if o, err := m.Origins().Get(ri.UID()); err != nil || o != nil {
		t.Fatalf("nothing may be persisted: %+v %v", o, err)
	}
	if after := ri.Status(); after.Sync != syncGen.Sync {
		t.Fatalf("the running sync loop must not have been touched: %+v → %+v", syncGen.Sync, after.Sync)
	}
}

// TestHandleHALSetOrigin_DuringIndexingIs409WithACancelLink: Attach is
// exclusive with indexing — refused before any guard or network read, with a
// problem body that links the way out.
func TestHandleHALSetOrigin_DuringIndexingIs409WithACancelLink(t *testing.T) {
	r, m, release := heldIndexServer(t)
	defer release()
	ri := m.Get("alpha")

	rec := putOrigin(t, r, "alpha", `{"url":"https://unreachable.invalid/kb.git","branch":"main","auth_method":"none"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	var problem struct {
		Title  string `json:"title"`
		Detail string `json:"detail"`
		Links  map[string]struct {
			Href   string `json:"href"`
			Method string `json:"method"`
		} `json:"_links"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &problem))
	require.Equal(t, "Repo is indexing", problem.Title)
	require.Equal(t, "wait for it to finish or cancel indexing", problem.Detail)
	require.Equal(t, "/api/v1/repos/alpha/index:cancel", problem.Links["cancel-index"].Href)
	require.Equal(t, http.MethodPost, problem.Links["cancel-index"].Method)
	if o, err := m.Origins().Get(ri.UID()); err != nil || o != nil {
		t.Fatalf("nothing may be persisted: %+v %v", o, err)
	}
}

// patchUpstream sends PATCH /repos/{repo}/origin/upstream with a JSON body.
func patchUpstream(t *testing.T, r http.Handler, repo, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := fromLoopback(httptest.NewRequest(http.MethodPatch, "/repos/"+repo+"/origin/upstream", strings.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	return rec
}

// attachedUpstreamServer is newControlDBTestServer with alpha attached to a
// real local bare remote whose upstream is "main".
func attachedUpstreamServer(t *testing.T) (http.Handler, *repos.Manager, *repos.RepoInstance) {
	t.Helper()
	originsRoot := t.TempDir()
	s, m, ri := newControlDBTestServer(t, originsRoot)
	url := seedBareRemoteForTest(t, filepath.Join(originsRoot, "upstream.git"))
	_, err := m.Send(context.Background(), ri, repos.AttachOrigin(repos.OriginSpec{URL: url, Branch: "main", AuthMethod: "none"}))
	require.NoError(t, err)
	return s.NewAPIRouter(), m, ri
}

// upstreamState is the stored upstream branch (control.db) and the live fetch
// refspecs of ri.
func upstreamState(t *testing.T, m *repos.Manager, ri *repos.RepoInstance) (string, []string) {
	t.Helper()
	o, err := m.Origins().Get(ri.UID())
	require.NoError(t, err)
	require.NotNil(t, o)
	var refspecs []string
	require.NoError(t, ri.WithRead(func(svc *store.Service) { refspecs = storetest.FetchRefspecs(svc, "origin") }))
	return o.Branch, refspecs
}

// PATCH answers with the branch it was ASKED for, which here is neither the
// stored upstream ("main") nor the agent branch, and that branch is what was
// written. A second PATCH to another branch tells the echo from a constant.
func TestHandleHALSetOriginUpstream_UpdatesBranch(t *testing.T) {
	r, m, ri := attachedUpstreamServer(t)
	require.NotEqual(t, "develop", ri.AgentBranch(), "precondition: request branch is not the agent branch")
	if b, _ := upstreamState(t, m, ri); b != "main" {
		t.Fatalf("precondition: stored upstream %q, want main", b)
	}

	rec := patchUpstream(t, r, "alpha", `{"branch":"develop"}`)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var body struct {
		Status string      `json:"status"`
		Branch string      `json:"branch"`
		Links  hal.LinkMap `json:"_links"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "ok", body.Status)
	require.Equal(t, "develop", body.Branch)
	require.Equal(t, "/api/v1/repos/alpha/origin", body.Links["self"].Href)
	require.Equal(t, "/api/v1/repos/alpha", body.Links["repo"].Href)

	b, _ := upstreamState(t, m, ri)
	require.Equal(t, "develop", b)

	rec = patchUpstream(t, r, "alpha", `{"branch":"release-2"}`)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "release-2", body.Branch)
	b, _ = upstreamState(t, m, ri)
	require.Equal(t, "release-2", b)
}

// Changing the upstream keeps the credential: the injected origin (what the
// reconcile loop authenticates with) still carries the stored token and auth
// method after PATCH, with the new branch.
func TestHandleHALSetOriginUpstream_KeepsTheCredential(t *testing.T) {
	originsRoot := t.TempDir()
	s, m, ri := newControlDBTestServer(t, originsRoot)
	url := seedBareRemoteForTest(t, filepath.Join(originsRoot, "upstream.git"))
	_, err := m.Send(context.Background(), ri, repos.AttachOrigin(repos.OriginSpec{
		URL: url, Branch: "main", AuthMethod: "token", AuthToken: "tok-kept",
	}))
	require.NoError(t, err)

	rec := patchUpstream(t, s.NewAPIRouter(), "alpha", `{"branch":"develop"}`)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var remote *remotesync.Remote
	require.NoError(t, ri.WithRead(func(svc *store.Service) { remote, err = svc.Remote().GetRemote("origin") }))
	require.NoError(t, err)
	require.NotNil(t, remote)
	require.Equal(t, "develop", remote.Branch)
	require.Equal(t, url, remote.URL)
	require.Equal(t, "token", remote.AuthMethod)
	require.Equal(t, "tok-kept", remote.AuthToken)
	o, err := m.Origins().Get(ri.UID())
	require.NoError(t, err)
	require.Equal(t, "tok-kept", o.AuthToken)
}

func TestHandleHALSetOriginUpstream_EmptyBranch_Returns400(t *testing.T) {
	r, m, ri := attachedUpstreamServer(t)

	rec := patchUpstream(t, r, "alpha", `{"branch":""}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	require.Equal(t, "Branch required", originProblem(t, rec).Title)
	b, _ := upstreamState(t, m, ri)
	require.Equal(t, "main", b, "a refused PATCH writes nothing")
}

// TestHandleHALSetOriginUpstream_InvalidBranch_Returns400 pins that a branch
// name with characters illegal in a git ref is rejected with 400 and never
// reaches setOriginUpstream (it would otherwise be woven into the fetch
// refspec): the stored upstream and the refspecs are unchanged.
func TestHandleHALSetOriginUpstream_InvalidBranch_Returns400(t *testing.T) {
	r, m, ri := attachedUpstreamServer(t)
	_, before := upstreamState(t, m, ri)
	for _, bad := range []string{"has space", "ends/", "-leading", "a..b", "ctrl\tname", "co:lon"} {
		body, _ := json.Marshal(map[string]string{"branch": bad})
		rec := patchUpstream(t, r, "alpha", string(body))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("branch %q: status got %d, want 400; body=%s", bad, rec.Code, rec.Body.String())
			continue
		}
		require.Equal(t, "Invalid branch name", originProblem(t, rec).Title, "branch %q", bad)
	}
	b, after := upstreamState(t, m, ri)
	require.Equal(t, "main", b, "no invalid branch may be stored")
	require.ElementsMatch(t, before, after, "no invalid branch may reach the refspec")
}

// A repo whose store cannot be acquired, on a running manager, answers the one
// store-unavailable 503 (it was 500 "No store available") and the other repo
// is untouched.
func TestHandleHALSetOriginUpstream_StorelessRepoIs503(t *testing.T) {
	r, m, ri := attachedUpstreamServer(t)
	m.Set("ghost", repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{Name: "ghost", UID: "ghost-uid", AgentBranch: "agent/test"}))

	rec := patchUpstream(t, r, "ghost", `{"branch":"develop"}`)
	requireUnavailableProblem(t, rec, "/repos/ghost/origin/upstream")
	b, _ := upstreamState(t, m, ri)
	require.Equal(t, "main", b, "the failed PATCH on ghost must not touch alpha")
}

// Any other failure is 500 "Failed to set upstream branch" carrying the error:
// here a manager that was never started has no control.db tenant.
func TestHandleHALSetOriginUpstream_StoppedManagerIs500(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t, "alpha")} // never started: Origins() is nil
	rec := patchUpstream(t, s.NewAPIRouter(), "alpha", `{"branch":"develop"}`)
	require.Equal(t, http.StatusInternalServerError, rec.Code, "body=%s", rec.Body.String())
	p := originProblem(t, rec)
	require.Equal(t, "Failed to set upstream branch", p.Title)
	require.Equal(t, repos.ErrManagerStopped.Error(), p.Detail)
	require.Equal(t, "/repos/alpha/origin/upstream", p.Instance)
}

func TestHandleHALDeleteOrigin_Returns204(t *testing.T) {
	s, _, _ := newControlDBTestServer(t, t.TempDir())
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := fromLoopback(httptest.NewRequest(http.MethodDelete, "/repos/alpha/origin", nil))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleHALDeleteOrigin_UnknownRepo_Returns404(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t, "alpha")}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := fromLoopback(httptest.NewRequest(http.MethodDelete, "/repos/missing/origin", nil))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rec.Code)
	}
}

// runGitForTest runs a git command in dir, failing the test on error. Used to
// build a real bare remote the attach guard can probe and the Sync stage's
// reconcile can fetch from without touching the network.
func runGitForTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// seedBareRemoteForTest builds a bare git repo with one commit on "main"
// under bare and returns a file:// URL pointing at it — a local stand-in for
// a real remote so PUT /origin's probe and reconcile succeed without any
// network access.
func seedBareRemoteForTest(t *testing.T, bare string) string {
	t.Helper()
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatalf("mkdir bare: %v", err)
	}
	runGitForTest(t, "", "init", "--bare", "--initial-branch=main", bare)
	work := t.TempDir()
	runGitForTest(t, "", "clone", bare, work)
	if err := os.WriteFile(filepath.Join(work, "seed.txt"), []byte("seed"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	runGitForTest(t, work, "add", "seed.txt")
	runGitForTest(t, work, "commit", "-m", "seed")
	runGitForTest(t, work, "push", "origin", "main")
	runGitForTest(t, bare, "symbolic-ref", "HEAD", "refs/heads/main")
	return fileuri.New(bare)
}

// newControlDBTestServer boots a REAL Manager — control.db opened via
// Start(), a repo mounted via the real Create path — so PUT/DELETE send real
// lifecycle events and the writes actually land in control.db (and not the
// repo's own remotes row).
//
// A real agent key is written so credential encryption is available —
// otherwise Origins.Set would refuse to store the test's token. originsRoot
// becomes LocalOriginRoot so a file:// origin under it clears the
// local-origin policy gate that PUT /origin enforces at the write edge.
func newControlDBTestServer(t *testing.T, originsRoot string) (*Server, *repos.Manager, *repos.RepoInstance) {
	t.Helper()
	return newControlDBTestServerOpt(t, originsRoot, true)
}

// newControlDBTestServerOpt is newControlDBTestServer with the agent key made
// optional. withKey=false leaves Deps.KeyPath pointing at a file that does not
// exist, which is how Manager.Start ends up with crypt == nil — the state in
// which Origins.Set refuses any non-empty credential rather than writing a
// secret in the clear. That refusal is the only reachable way to fail the
// DURABLE half of an origin write after its git half has already succeeded,
// which is what TestSetOrigin_FailedPersistRestoresTheGitRemote needs.
func newControlDBTestServerOpt(t *testing.T, originsRoot string, withKey bool) (*Server, *repos.Manager, *repos.RepoInstance) {
	t.Helper()
	home := t.TempDir()
	keyPath := filepath.Join(home, "agent.key")
	if withKey {
		if err := os.WriteFile(keyPath, []byte("agent-key-material-for-hkdf"), 0o600); err != nil {
			t.Fatalf("write key: %v", err)
		}
	}

	m := repos.New(context.Background(), repos.Deps{
		Cfg:     config.Config{Home: home, OntologyRoot: "kb", LocalOriginRoot: originsRoot},
		AgentID: testsigner.FixtureAgentID("agent/test"), AgentBranch: "agent/test",
		KeyPath: keyPath,
		Machine: repos.Options{Synchronous: true},
	})
	t.Cleanup(func() { _ = m.Close() })
	if err := m.Start(); err != nil {
		t.Fatalf("start manager: %v", err)
	}

	ri, err := m.Create(context.Background(), repos.CreateSpec{Name: "alpha", Mode: "preset"}, nil)
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}

	s := &Server{Manager: m}
	return s, m, ri
}

// TestPutOrigin_PersistsToControlDB pins the Task 10 contract: PUT /origin
// writes connection identity (url/branch/auth) to control.db via
// mgr.Origins(), not to the repo's own remotes row. That is what makes the
// connection survive losing the repo's .db file — control.db outlives it.
func TestPutOrigin_PersistsToControlDB(t *testing.T) {
	originsRoot := t.TempDir()
	s, m, ri := newControlDBTestServer(t, originsRoot)
	r := s.NewAPIRouter()

	wantURL := seedBareRemoteForTest(t, filepath.Join(originsRoot, "upstream.git"))
	body := `{"url":"` + wantURL + `","branch":"main","auth_method":"token","token":"tok-secret"}`
	rec := httptest.NewRecorder()
	req := fromLoopback(httptest.NewRequest(http.MethodPut, "/repos/alpha/origin", strings.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code < 200 || rec.Code >= 300 {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}

	// 1. control.db (mgr.Origins()) holds the full record.
	origin, err := m.Origins().Get(ri.UID())
	if err != nil {
		t.Fatalf("Origins().Get: %v", err)
	}
	if origin == nil {
		t.Fatal("expected an origin in control.db, got nil")
	}
	if origin.URL != wantURL {
		t.Errorf("origin URL: got %q, want %q", origin.URL, wantURL)
	}
	if origin.Branch != "main" {
		t.Errorf("origin branch: got %q, want %q", origin.Branch, "main")
	}
	if origin.AuthToken != "tok-secret" {
		t.Errorf("origin auth token: got %q, want %q", origin.AuthToken, "tok-secret")
	}

	// 2. The repo's own remotes table CANNOT carry connection identity —
	// migration 000017 dropped the columns, so control.db is not merely the
	// preferred source of truth, it is the only possible one. Asserting on the
	// schema rather than on a row's contents is what makes that irreversible:
	// a re-added column would fail here even before anything wrote to it.
	var cols map[string]bool
	var scanErr error
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			scanErr = errors.New("nil svc")
			return
		}
		rows, err := storetest.RawDB(svc).Query(`SELECT name FROM pragma_table_info('remotes')`)
		if err != nil {
			scanErr = err
			return
		}
		defer rows.Close()
		cols = map[string]bool{}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				scanErr = err
				return
			}
			cols[name] = true
		}
		scanErr = rows.Err()
	})
	if scanErr != nil {
		t.Fatalf("read remotes schema: %v", scanErr)
	}
	if len(cols) == 0 {
		t.Fatal("precondition: the remotes table must exist")
	}
	for _, gone := range []string{"url", "branch", "auth_method", "auth_token"} {
		if cols[gone] {
			t.Errorf("remotes.%s must not exist — connection identity belongs to control.db", gone)
		}
	}
}

// TestPatchOriginUpstream_NoOriginIsRejected pins the guard that
// SetOriginUpstream inherited from the deleted store.SetUpstreamBranch:
// changing the upstream on a repo that has no origin is an error, not a silent
// no-op that would wire up a git remote with an empty URL.
func TestPatchOriginUpstream_NoOriginIsRejected(t *testing.T) {
	s, _, _ := newControlDBTestServer(t, t.TempDir())
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := fromLoopback(httptest.NewRequest(http.MethodPatch, "/repos/alpha/origin/upstream",
		strings.NewReader(`{"branch":"develop"}`)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusInternalServerError, rec.Code, "body=%s", rec.Body.String())
	p := originProblem(t, rec)
	require.Equal(t, "Failed to set upstream branch", p.Title)
	require.Equal(t, "SetOriginUpstream: no origin configured", p.Detail)
}

// TestPatchOriginUpstream_PersistsToControlDB exercises setOriginUpstream
// against control.db: after PUT establishes an origin, PATCH
// .../origin/upstream must update the stored branch in mgr.Origins() (not just
// the injected in-memory copy), preserving the refspec-first ordering
// discipline carried over from the old single-function SetUpstreamBranch.
func TestPatchOriginUpstream_PersistsToControlDB(t *testing.T) {
	originsRoot := t.TempDir()
	s, m, ri := newControlDBTestServer(t, originsRoot)
	r := s.NewAPIRouter()

	url := seedBareRemoteForTest(t, filepath.Join(originsRoot, "upstream.git"))
	putBody := `{"url":"` + url + `","branch":"main","auth_method":"none"}`
	rec := httptest.NewRecorder()
	req := fromLoopback(httptest.NewRequest(http.MethodPut, "/repos/alpha/origin", strings.NewReader(putBody)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	if rec.Code < 200 || rec.Code >= 300 {
		t.Fatalf("PUT status: got %d, body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = fromLoopback(httptest.NewRequest(http.MethodPatch, "/repos/alpha/origin/upstream",
		strings.NewReader(`{"branch":"develop"}`)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH status: got %d, body=%s", rec.Code, rec.Body.String())
	}

	origin, err := m.Origins().Get(ri.UID())
	if err != nil {
		t.Fatalf("Origins().Get: %v", err)
	}
	if origin == nil || origin.Branch != "develop" {
		t.Fatalf("control.db branch: got %+v, want branch=develop", origin)
	}

	// The injected origin (and thus the next GetRemote) reflects it too,
	// without a restart.
	var remote *remotesync.Remote
	var refspecs []string
	ri.WithRead(func(svc *store.Service) {
		remote, err = svc.Remote().GetRemote("origin")
		refspecs = storetest.FetchRefspecs(svc, "origin")
	})
	if err != nil {
		t.Fatalf("GetRemote: %v", err)
	}
	if remote == nil || remote.Branch != "develop" {
		t.Fatalf("live GetRemote branch: got %+v, want branch=develop", remote)
	}

	// The git FETCH REFSPEC must track the new branch. This is the assertion the
	// stored-branch checks above cannot make: drop the ConfigureRemote call from
	// setOriginUpstream and every check above still passes while the next Sync
	// silently fetches the OLD branch. That wrong-branch sync is precisely what
	// the refspec-first ordering exists to prevent, so it has to be pinned here.
	got := make(map[string]bool, len(refspecs))
	for _, rs := range refspecs {
		got[rs] = true
	}
	if !got["+refs/heads/develop:refs/remotes/origin/develop"] {
		t.Errorf("fetch refspec must track develop after the upstream change; got %v", refspecs)
	}
	if got["+refs/heads/main:refs/remotes/origin/main"] {
		t.Errorf("the previous upstream's refspec must be gone; got %v", refspecs)
	}
	if !got["+refs/heads/"+ri.AgentBranch()+":refs/remotes/origin/"+ri.AgentBranch()] {
		t.Errorf("the agent-branch refspec must be preserved; got %v", refspecs)
	}
}

// TestDeleteOrigin_RemovesFromControlDB exercises the real DeleteOrigin: it must remove the row from control.db AND clear the
// injected origin on the running store, so a GET right after reports none
// without a restart.
func TestDeleteOrigin_RemovesFromControlDB(t *testing.T) {
	originsRoot := t.TempDir()
	s, m, ri := newControlDBTestServer(t, originsRoot)
	r := s.NewAPIRouter()

	url := seedBareRemoteForTest(t, filepath.Join(originsRoot, "upstream.git"))
	putBody := `{"url":"` + url + `","branch":"main","auth_method":"none"}`
	rec := httptest.NewRecorder()
	req := fromLoopback(httptest.NewRequest(http.MethodPut, "/repos/alpha/origin", strings.NewReader(putBody)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	if rec.Code < 200 || rec.Code >= 300 {
		t.Fatalf("PUT status: got %d, body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = fromLoopback(httptest.NewRequest(http.MethodDelete, "/repos/alpha/origin", nil))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE status: got %d, body=%s", rec.Code, rec.Body.String())
	}

	origin, err := m.Origins().Get(ri.UID())
	if err != nil {
		t.Fatalf("Origins().Get: %v", err)
	}
	if origin != nil {
		t.Errorf("expected no origin in control.db after delete, got %+v", origin)
	}

	var remote *remotesync.Remote
	ri.WithRead(func(svc *store.Service) {
		remote, err = svc.Remote().GetRemote("origin")
	})
	if err != nil {
		t.Fatalf("GetRemote: %v", err)
	}
	if remote != nil {
		t.Errorf("expected the live store to report no origin after delete, got %+v", remote)
	}
}

// A detach that cannot do its job must keep the durable record: the URL,
// auth_method and encrypted auth_token exist nowhere else. A detach against a
// repo that is going away (here: already unmounted) is refused by its machine
// before anything is touched.
func TestDeleteOrigin_UnmountedRepoKeepsTheDurableRecord(t *testing.T) {
	originsRoot := t.TempDir()
	s, m, ri := newControlDBTestServer(t, originsRoot)
	r := s.NewAPIRouter()

	wantURL := seedBareRemoteForTest(t, filepath.Join(originsRoot, "upstream.git"))
	rec := putOrigin(t, r, "alpha", `{"url":"`+wantURL+`","branch":"main","auth_method":"token","token":"tok-secret"}`)
	if rec.Code < 200 || rec.Code >= 300 {
		t.Fatalf("PUT status: got %d, body=%s", rec.Code, rec.Body.String())
	}

	m.Remove("alpha") // unmount: the store is closed for good

	if _, err := m.Send(context.Background(), ri, repos.DetachOrigin()); !errors.Is(err, repos.ErrClosed) {
		t.Fatalf("a detach against an unmounted repo: got %v, want ErrClosed", err)
	}

	origin, err := m.Origins().Get(ri.UID())
	if err != nil {
		t.Fatalf("Origins().Get: %v", err)
	}
	if origin == nil {
		t.Fatal("the control.db record was destroyed by a detach that could not do its job")
	}
	if origin.URL != wantURL || origin.AuthMethod != "token" || origin.AuthToken != "tok-secret" {
		t.Errorf("the record must survive intact: %+v", origin)
	}
}

// An attach must not record a connection it failed to wire up. The git write
// (ConfigureRemote) goes first in its apply: persisting ahead of it leaves
// control.db reporting a URL whose refspecs were never rewritten, which the
// next boot silently adopts — a PUT that answered 500 taking effect on
// restart. This is the discipline setOriginUpstream documents.
//
// The forced failure is a branch carrying a colon, which makes the fetch
// refspec malformed and CreateRemote refuse it.
func TestSetOrigin_FailedRefspecRewriteStoresNothing(t *testing.T) {
	originsRoot := t.TempDir()
	_, m, ri := newControlDBTestServer(t, originsRoot)

	url := seedBareRemoteForTest(t, filepath.Join(originsRoot, "upstream.git"))
	_, err := m.Send(context.Background(), ri, repos.AttachOrigin(repos.OriginSpec{
		URL:    url,
		Branch: "bad:branch",
	}))
	if err == nil {
		t.Fatal("a refspec that cannot be built must fail the call")
	}

	origin, gerr := m.Origins().Get(ri.UID())
	if gerr != nil {
		t.Fatalf("Origins().Get: %v", gerr)
	}
	if origin != nil {
		t.Fatalf("control.db recorded an origin the git config was never rewritten for: %+v", origin)
	}
}

// The other side of that ordering: ConfigureRemote succeeds and the durable
// write then fails.
//
// Nothing heals this on its own. The git remote names the NEW url while the
// injected origin — and so GetRemote, and so the reconcile loop's credential
// and upstream branch — still answer with the OLD one, so every tick fetches
// the new url carrying the old credential until someone restarts the process
// and the git config is re-derived from a record that was never written. The
// caller was told 500.
//
// The failure is not contrived: Origins.Set refuses any non-empty credential
// when the agent key could not be read, so this is what a whole class of
// installs does on every credentialed PUT.
func TestSetOrigin_FailedPersistRestoresTheGitRemote(t *testing.T) {
	originsRoot := t.TempDir()
	_, m, ri := newControlDBTestServerOpt(t, originsRoot, false) // no agent key → crypt == nil

	first := seedBareRemoteForTest(t, filepath.Join(originsRoot, "one.git"))
	if _, err := m.Send(context.Background(), ri, repos.AttachOrigin(repos.OriginSpec{
		URL: first, Branch: "main", AuthMethod: "none",
	})); err != nil {
		t.Fatalf("first attach (no credential, so storable without a crypt): %v", err)
	}

	second := seedBareRemoteForTest(t, filepath.Join(originsRoot, "two.git"))
	_, err := m.Send(context.Background(), ri, repos.AttachOrigin(repos.OriginSpec{
		URL: second, Branch: "main", AuthMethod: "token", AuthToken: "tok-secret",
	}))
	if err == nil {
		t.Fatal("Origins.Set must refuse a credential it cannot encrypt")
	}

	origin, gerr := m.Origins().Get(ri.UID())
	if gerr != nil {
		t.Fatalf("Origins().Get: %v", gerr)
	}
	if origin == nil || origin.URL != first {
		t.Fatalf("control.db origin: got %+v, want the untouched %q", origin, first)
	}

	var urls []string
	var remote *remotesync.Remote
	ri.WithRead(func(svc *store.Service) {
		urls = storetest.RemoteURLs(svc, "origin")
		remote, _ = svc.Remote().GetRemote("origin")
	})
	if len(urls) != 1 || urls[0] != first {
		t.Errorf("git remote url after a failed PUT: got %v, want [%q] — the repo is fetching a url "+
			"nothing records, with the previous origin's credential", urls, first)
	}
	if remote == nil || remote.URL != first {
		t.Errorf("GetRemote after a failed PUT: got %+v, want url=%q", remote, first)
	}
}

// setOriginUpstream shares that ordering and therefore that window: a refspec
// rewritten to the new branch while GetRemote still reports the old one leaves
// reconcileNow reconciling against a refs/remotes/origin/<old> nothing updates
// any more. The restore has to put the refspec back.
//
// The forced failure is a live repo whose control.db row is gone — the injected
// origin still answers GetRemote, so the call gets past its "no origin
// configured" guard and into ConfigureRemote, and Origins.SetBranch then
// updates no rows and says so.
func TestSetOriginUpstream_FailedPersistRestoresTheRefspec(t *testing.T) {
	originsRoot := t.TempDir()
	_, m, ri := newControlDBTestServer(t, originsRoot)

	url := seedBareRemoteForTest(t, filepath.Join(originsRoot, "upstream.git"))
	if _, err := m.Send(context.Background(), ri, repos.AttachOrigin(repos.OriginSpec{
		URL: url, Branch: "main", AuthMethod: "none",
	})); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := m.Origins().Delete(ri.UID()); err != nil {
		t.Fatalf("Origins().Delete: %v", err)
	}

	if err := setOriginUpstream(context.Background(), m, ri, "develop"); err == nil {
		t.Fatal("SetBranch against a missing row must fail the call")
	}

	var refspecs []string
	ri.WithRead(func(svc *store.Service) { refspecs = storetest.FetchRefspecs(svc, "origin") })
	got := make(map[string]bool, len(refspecs))
	for _, rs := range refspecs {
		got[rs] = true
	}
	if !got["+refs/heads/main:refs/remotes/origin/main"] {
		t.Errorf("the upstream refspec must be restored after a failed persist; got %v", refspecs)
	}
	if got["+refs/heads/develop:refs/remotes/origin/develop"] {
		t.Errorf("the refspec still tracks a branch nothing recorded; got %v", refspecs)
	}
	if !got["+refs/heads/"+ri.AgentBranch()+":refs/remotes/origin/"+ri.AgentBranch()] {
		t.Errorf("the agent-branch refspec must survive the restore; got %v", refspecs)
	}
}

// Every origin write goes through the manager's control.db tenant, which
// Manager.Close nils — and whose methods dereference their crypt on the first
// line. cmd/serve.go's 5s shutdown deadline lets a request outlive the tenant
// it is about to write to, so these must ASK for it and refuse, not dereference
// and panic into the crash-bundle path.
//
// The stopped manager here is one that was never started, which is the same
// nil-tenant state Close leaves behind. ri is nil on purpose: the guard has to
// fire before anything downstream is touched, and a setOriginUpstream that
// reached ri.WithRead first would panic on it.
func TestOriginWrites_RefuseAStoppedManager(t *testing.T) {
	m := repos.New(context.Background(), repos.Deps{
		Cfg:     config.Config{Home: t.TempDir(), OntologyRoot: "kb"},
		AgentID: testsigner.FixtureAgentID("agent/test"), AgentBranch: "agent/test",
	})
	calls := []struct {
		name string
		run  func() error
	}{
		{"setOriginUpstream", func() error {
			return setOriginUpstream(context.Background(), m, nil, "main")
		}},
	}
	for _, c := range calls {
		err := c.run()
		if !errors.Is(err, repos.ErrManagerStopped) {
			t.Errorf("%s against a stopped manager: got %v, want %v", c.name, err, repos.ErrManagerStopped)
		}
	}
}

// ATTACHING A REMOTE MUST NOT CHANGE THE REPO'S ONTOLOGY.
//
// "A repository is a knomit knowledge base if and only if it has an ontology"
// is enforced at CREATE — initClone refuses a remote without one, initInitialize
// refuses one that already has one. PUT /origin is the other door into exactly
// the same situation: it points an existing repo, which already has an ontology
// and facts written against it, at a remote that has its own. Nothing checked.
//
// The repo's ontology is fixed at create time and is never user-editable, so a
// remote whose taxonomy differs cannot be reconciled with it — every fact
// already written was validated against the local one, and every fact written
// afterwards would be validated against the remote's.
func TestHandleHALSetOrigin_RefusesARemoteWithADifferentOntology(t *testing.T) {
	root := t.TempDir()
	s := &Server{Manager: newRealManagerWithLocalOriginRoot(t, root)}
	r := s.NewAPIRouter()

	// A repo on the CODE taxonomy.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, fromLoopback(newJSONRequest(http.MethodPost, "/repos",
		strings.NewReader(`{"name":"kb","mode":"preset","ontology_preset":"code"}`))))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// POST /repos is 202: the repo is not there until the job finishes.
	awaitCreate(t, r, rec)

	// A remote that is a knowledge base on a DIFFERENT one (the default).
	url := seedKnomitRemoteForTest(t, filepath.Join(root, "remote.git"), "seed")

	rec = httptest.NewRecorder()
	body := `{"url":"` + url + `","branch":"main"}`
	r.ServeHTTP(rec, fromLoopback(newJSONRequest(http.MethodPut, "/repos/kb/origin", strings.NewReader(body))))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", rec.Code, rec.Body.String())
	}
	// The reader has to learn WHICH two taxonomies are in conflict; "conflict"
	// alone leaves them looking for something they cannot see.
	if b := rec.Body.String(); !strings.Contains(b, "source-code") || !strings.Contains(b, "general") {
		t.Fatalf("the refusal does not name both ontologies: %s", b)
	}
}

// The counterpart, and the reason this is not simply "refuse any remote with an
// ontology": attaching a remote that is NOT a knowledge base is how an existing
// local repo gets backed up. That must keep working, and the repo's own
// ontology must still be there afterwards.
func TestHandleHALSetOrigin_PlainRemoteIsAllowedAndKeepsTheOntology(t *testing.T) {
	root := t.TempDir()
	s := &Server{Manager: newRealManagerWithLocalOriginRoot(t, root)}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, fromLoopback(newJSONRequest(http.MethodPost, "/repos",
		strings.NewReader(`{"name":"kb","mode":"preset","ontology_preset":"code"}`))))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// POST /repos is 202: the repo is not there until the job finishes.
	awaitCreate(t, r, rec)

	url := seedPlainRemoteForTest(t, filepath.Join(root, "remote.git"))

	rec = httptest.NewRecorder()
	body := `{"url":"` + url + `","branch":"main"}`
	r.ServeHTTP(rec, fromLoopback(newJSONRequest(http.MethodPut, "/repos/kb/origin", strings.NewReader(body))))

	if rec.Code != http.StatusOK && rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want the attach to be allowed; body = %s", rec.Code, rec.Body.String())
	}

	ri := s.Manager.Get("kb")
	if ri == nil {
		t.Fatal("repo disappeared")
	}
	if got := ri.Ontology().ID; got != "source-code" {
		t.Fatalf("ontology id = %q, want source-code — attaching a remote replaced the repo's taxonomy", got)
	}
}

func TestHandleHALDeleteOrigin_SubscriptionIs409(t *testing.T) {
	originsRoot := t.TempDir()
	s, m, _ := newControlDBTestServer(t, originsRoot)
	url := seedBareRemoteKBForTest(t, filepath.Join(originsRoot, "kb.git"))
	if _, err := m.Create(context.Background(), repos.CreateSpec{
		Name: "sub", Mode: "subscribe", Origin: &repos.OriginSpec{URL: url},
	}, nil); err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, fromLoopback(httptest.NewRequest(http.MethodDelete, "/repos/sub/origin", nil)))
	require.Equal(t, http.StatusConflict, rec.Code, "body=%s", rec.Body.String())
	require.Contains(t, rec.Body.String(), "Subscription requires its origin")
	o, err := m.Origins().Get(m.Get("sub").UID())
	require.NoError(t, err)
	require.NotNil(t, o, "the refusal changed nothing")
}

// seedBareRemoteKBForTest is seedBareRemoteForTest plus an ontology, so the
// remote is a knowledge base and subscribe mode will accept it.
func seedBareRemoteKBForTest(t *testing.T, bare string) string {
	t.Helper()
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatalf("mkdir bare: %v", err)
	}
	runGitForTest(t, "", "init", "--bare", "--initial-branch=main", bare)
	work := t.TempDir()
	runGitForTest(t, "", "clone", bare, work)
	ont, err := fact.DefaultOntology().Serialize()
	if err != nil {
		t.Fatalf("serialize ontology: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(work, filepath.Dir(repos.OntologyPath)), 0o755); err != nil {
		t.Fatalf("mkdir ontology dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(work, repos.OntologyPath), ont, 0o644); err != nil {
		t.Fatalf("write ontology: %v", err)
	}
	runGitForTest(t, work, "add", "-A")
	runGitForTest(t, work, "commit", "-m", "seed kb")
	runGitForTest(t, work, "push", "origin", "main")
	runGitForTest(t, bare, "symbolic-ref", "HEAD", "refs/heads/main")
	return fileuri.New(bare)
}

// PUT /origin on a subscription must not DEMOTE it. Origins.Set with an empty
// Mode deletes the subscription row, so SetOrigin has to carry the stored mode
// through — and only a real Manager against a real control.db can show that.
func TestPutOrigin_OnASubscriptionPreservesItsMode(t *testing.T) {
	originsRoot := t.TempDir()
	s, m, _ := newControlDBTestServer(t, originsRoot)
	r := s.NewAPIRouter()

	url := seedBareRemoteKBForTest(t, filepath.Join(originsRoot, "kb.git"))
	sub, err := m.Create(context.Background(), repos.CreateSpec{
		Name: "sub", Mode: "subscribe", Origin: &repos.OriginSpec{URL: url},
	}, nil)
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	before, err := m.Origins().Get(sub.UID())
	if err != nil || before == nil {
		t.Fatalf("origin before: %v %+v", err, before)
	}
	require.Equal(t, control.OriginModeSubscribe, before.Mode, "precondition: created as a subscription")

	rec := httptest.NewRecorder()
	req := fromLoopback(httptest.NewRequest(http.MethodPut, "/repos/sub/origin",
		strings.NewReader(`{"url":"`+url+`","auth_method":"token","token":"s3cret"}`)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	if rec.Code < 200 || rec.Code >= 300 {
		t.Fatalf("PUT status: got %d, body=%s", rec.Code, rec.Body.String())
	}

	after, err := m.Origins().Get(sub.UID())
	require.NoError(t, err)
	require.NotNil(t, after)
	require.Equal(t, control.OriginModeSubscribe, after.Mode,
		"a credential update must not demote the subscription to sync")
	require.Equal(t, "s3cret", after.AuthToken, "the write actually landed")
}

// An attach writes the durable origin record, and Origins.Set is a full
// replacement: an empty Mode DELETES the subscription row. A connect session
// cannot reach a subscription today (handleCreateSession 409s first), but PUT
// /origin can — a credential refresh — so the rule is pinned at the write the
// AttachOrigin event's apply makes, not at that guard.
func TestAttachOrigin_PreservesSubscriptionMode(t *testing.T) {
	originsRoot := t.TempDir()
	_, m, _ := newControlDBTestServer(t, originsRoot)

	url := seedBareRemoteKBForTest(t, filepath.Join(originsRoot, "kb.git"))
	sub, err := m.Create(context.Background(), repos.CreateSpec{
		Name: "sub", Mode: "subscribe", Origin: &repos.OriginSpec{URL: url},
	}, nil)
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	before, err := m.Origins().Get(sub.UID())
	if err != nil || before == nil || before.Mode != control.OriginModeSubscribe {
		t.Fatalf("precondition: want a subscribe-mode origin, got %+v (err %v)", before, err)
	}

	if _, err := m.Send(context.Background(), sub, repos.AttachOrigin(repos.OriginSpec{
		URL: url, Branch: "main", AuthMethod: "token", AuthToken: "s3cret",
	})); err != nil {
		t.Fatalf("attach: %v", err)
	}

	after, err := m.Origins().Get(sub.UID())
	if err != nil || after == nil {
		t.Fatalf("origin after: %v %+v", err, after)
	}
	if after.Mode != control.OriginModeSubscribe {
		t.Errorf("Mode: got %q, want %q — the attach demoted the subscription",
			after.Mode, control.OriginModeSubscribe)
	}
	if after.AuthToken != "s3cret" {
		t.Errorf("AuthToken: got %q, want the value just written — the write did not land", after.AuthToken)
	}
}
