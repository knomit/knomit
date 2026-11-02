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

	"knomit/internal/corpus/repos"
	"knomit/internal/corpus/repos/control"
	"knomit/internal/foundation/auth"
)

// unlinkedX turns the fixture's linked repo x into one with a pasted token
// (an admin unlink), so the link tests start from a plain origin.
func (f *linkedFixture) unlinkedX(t *testing.T) {
	t.Helper()
	rec := f.admin(http.MethodPut, "/api/v1/repos/x/origin/credential",
		`{"secret_id":null,"auth_method":"token","token":"`+linkedSecretValue+`"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	o, err := f.m.Origins().Get(f.ri.UID())
	require.NoError(t, err)
	require.Empty(t, o.SecretID)
}

func TestLinkAPI_AdminLinks(t *testing.T) {
	f := newLinkedFixture(t)
	f.unlinkedX(t)
	rec := f.admin(http.MethodPut, "/api/v1/repos/x/origin/credential", `{"secret_id":"`+f.secretID+`"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	f.requireLinked(t, f.url)
	o, err := f.m.Origins().Get(f.ri.UID())
	require.NoError(t, err)
	require.Empty(t, o.AuthToken, "linking clears the pasted token")
	require.NoError(t, f.ri.OriginRoundForTest(context.Background()))
}

func TestLinkAPI_NonAdminRefused(t *testing.T) {
	f := newLinkedFixture(t)
	f.unlinkedX(t)
	body := `{"secret_id":"` + f.secretID + `"}`

	rec := f.operator(http.MethodPut, "/api/v1/repos/x/origin/credential", body)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// Anonymous loopback (the web UI's listener) without the browser proof.
	req := fromLoopback(httptest.NewRequest(http.MethodPut, "/api/v1/repos/x/origin/credential", strings.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
	rec = serve(f.s.Handler(), req)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// An unlink is gated the same way.
	rec = f.operator(http.MethodPut, "/api/v1/repos/x/origin/credential", `{"secret_id":null}`)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	o, err := f.m.Origins().Get(f.ri.UID())
	require.NoError(t, err)
	require.Empty(t, o.SecretID, "nothing linked")
	require.Equal(t, linkedSecretValue, o.AuthToken)
}

func TestLinkAPI_Refusals(t *testing.T) {
	f := newLinkedFixture(t)
	put := func(body string) *httptest.ResponseRecorder {
		return f.admin(http.MethodPut, "/api/v1/repos/x/origin/credential", body)
	}
	rec := put(`{"secret_id":"` + f.secretID + `","auth_method":"token","token":"x"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	rec = put(`{"secret_id":42}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	rec = put(`{"secret_id":"no-such-secret"}`)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())

	// A secret whose hosts list does not allow the origin's host.
	meta, err := f.m.Secrets().Create(controlTokenInput("gl-pat", "gitlab.com"), time.Now())
	require.NoError(t, err)
	rec = put(`{"secret_id":"` + meta.ID + `"}`)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "gl-pat")
	f.requireLinked(t, f.url)
}

// The explicit none: "secret_id": null with no credential leaves the repo
// anonymous, and the secret becomes deletable.
func TestLinkAPI_UnlinkExplicitNone(t *testing.T) {
	f := newLinkedFixture(t)
	rec := f.admin(http.MethodPut, "/api/v1/repos/x/origin/credential", `{"secret_id":null}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	o, err := f.m.Origins().Get(f.ri.UID())
	require.NoError(t, err)
	require.Empty(t, o.SecretID)
	require.Equal(t, "none", o.AuthMethod)
	require.NoError(t, f.m.Secrets().Delete(f.secretID))
}

func TestCreateClone_WithSecretID(t *testing.T) {
	f := newLinkedFixture(t)
	body := `{"name":"y","mode":"clone","origin":{"url":"` + f.url + `","branch":"main","secret_id":"` + f.secretID + `"}}`

	apiOp := f.mintTokenAt(t, "/api/v1", "api-operator", auth.Read, auth.Write, auth.Operator)
	rec := escDo(f.s.OAuthHandler(), apiOp, http.MethodPost, "/api/v1/repos", body)
	require.Equal(t, http.StatusForbidden, rec.Code, "an operator cannot create with a stored secret: %s", rec.Body.String())

	rec = f.admin(http.MethodPost, "/api/v1/repos",
		`{"name":"y","mode":"clone","origin":{"url":"`+f.url+`","secret_id":"`+f.secretID+`","auth_token":"x"}}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	rec = f.admin(http.MethodPost, "/api/v1/repos",
		`{"name":"y","mode":"clone","origin":{"url":"`+f.otherURL+`","secret_id":"`+f.secretID+`"}}`)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())

	// Delete x first: one local copy per knowledge base.
	require.NoError(t, f.m.DeleteRepo("x", "test"))
	rec = f.admin(http.MethodPost, "/api/v1/repos", body)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	var acc struct {
		ID string `json:"create_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &acc))
	st := awaitJob(t, f.m, acc.ID)
	require.Equal(t, repos.CreateDone, st.State, "create failed: %v", st.Err)
	ri := f.m.Get("y")
	require.NotNil(t, ri)
	o, err := f.m.Origins().Get(ri.UID())
	require.NoError(t, err)
	require.Equal(t, f.secretID, o.SecretID)
	require.Equal(t, "token", o.AuthMethod)
	require.Empty(t, o.AuthToken)
}

func TestProbe_WithSecretID(t *testing.T) {
	f := newLinkedFixture(t)
	rec := f.admin(http.MethodPost, "/api/v1/repos:probe-origin", `{"url":"`+f.url+`","secret_id":"`+f.secretID+`"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var res map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &res))
	require.Equal(t, true, res["reachable"], rec.Body.String())
	require.NotEqual(t, true, res["auth_required"], rec.Body.String())

	// Without the secret the server refuses the anonymous probe.
	rec = f.admin(http.MethodPost, "/api/v1/repos:probe-origin", `{"url":"`+f.url+`"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &res))
	require.Equal(t, true, res["auth_required"], rec.Body.String())

	apiOp := f.mintTokenAt(t, "/api/v1", "api-operator", auth.Read, auth.Write, auth.Operator)
	rec = escDo(f.s.OAuthHandler(), apiOp, http.MethodPost, "/api/v1/repos:probe-origin", `{"url":"`+f.url+`","secret_id":"`+f.secretID+`"}`)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	rec = f.admin(http.MethodPost, "/api/v1/repos:probe-initialized", `{"url":"`+f.url+`","branch":"main","secret_id":"`+f.secretID+`"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"initialized":"yes"`)
}

// adminSSE drives one SSE step of a connect session as the local admin.
func (f *linkedFixture) adminSSE(t *testing.T, method, path, body string) string {
	t.Helper()
	rec := newStreamRecorder()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	req.RemoteAddr = "@"
	f.s.Handler().ServeHTTP(rec, req.WithContext(auth.WithPeer(req.Context(), auth.Peer{ID: f.me, Via: auth.LocalVia})))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	return rec.Body.String()
}

func (f *linkedFixture) startSession(t *testing.T, repo, body string, asAdmin bool, tok ...string) string {
	t.Helper()
	var rec *httptest.ResponseRecorder
	if asAdmin {
		rec = f.admin(http.MethodPost, "/api/v1/repos/"+repo+"/origin-sessions", body)
	} else {
		rec = escDo(f.s.OAuthHandler(), tok[0], http.MethodPost, "/api/v1/repos/"+repo+"/origin-sessions", body)
	}
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct {
		SessionID string `json:"session_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out.SessionID
}

// sessionRepoZ deletes x (one local copy per knowledge base) and creates
// z, a local repo with no origin, for the connect-session tests.
func (f *linkedFixture) sessionRepoZ(t *testing.T) *repos.RepoInstance {
	t.Helper()
	f.s.AgentBranch = escAgent
	withOriginSessionLifetime(t, f.s)
	require.NoError(t, f.m.DeleteRepo("x", "test"))
	ri, err := f.m.Create(context.Background(), repos.CreateSpec{Name: "z", Mode: "preset"}, nil)
	require.NoError(t, err)
	return ri
}

// connectZ runs a whole connect session on z as the local admin.
func (f *linkedFixture) connectZ(t *testing.T, body string) {
	t.Helper()
	id := f.startSession(t, "z", body, true)
	base := "/api/v1/repos/z/origin-sessions/" + id
	out := f.adminSSE(t, http.MethodGet, base+"/test", "")
	require.NotContains(t, out, `"phase":"error"`, out)
	out = f.adminSSE(t, http.MethodPost, base+"/apply", `{"conflict_strategy":"local_wins"}`)
	require.NotContains(t, out, `"phase":"error"`, out)
	out = f.adminSSE(t, http.MethodPost, base+"/commit", "")
	require.NotContains(t, out, `"phase":"error"`, out)
	require.Contains(t, out, `"phase":"done"`, out)
}

// A session with secret_id tests with the secret and commits the link.
func TestOriginSession_TestAndCommitWithSecretID(t *testing.T) {
	f := newLinkedFixture(t)
	z := f.sessionRepoZ(t)
	zTok := f.mintOperatorToken(t, "z")

	rec := escDo(f.s.OAuthHandler(), zTok, http.MethodPost, "/api/v1/repos/z/origin-sessions", `{"url":"`+f.url+`","secret_id":"`+f.secretID+`"}`)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec = f.admin(http.MethodPost, "/api/v1/repos/z/origin-sessions", `{"url":"`+f.otherURL+`","secret_id":"`+f.secretID+`"}`)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	rec = f.admin(http.MethodPost, "/api/v1/repos/z/origin-sessions", `{"url":"`+f.url+`","secret_id":"`+f.secretID+`","token":"x"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	f.connectZ(t, `{"url":"`+f.url+`","secret_id":"`+f.secretID+`"}`)
	o, err := f.m.Origins().Get(z.UID())
	require.NoError(t, err)
	require.NotNil(t, o)
	require.Equal(t, f.secretID, o.SecretID)
	require.Equal(t, "token", o.AuthMethod)
	require.Empty(t, o.AuthToken)
}

// A commit that would replace a linked repo's origin (unlinking it) needs
// admin, even when the session itself carries a pasted credential.
func TestOriginSession_CommitOnLinkedRepoNeedsAdmin(t *testing.T) {
	f := newLinkedFixture(t)
	z := f.sessionRepoZ(t)
	f.connectZ(t, `{"url":"`+f.url+`","secret_id":"`+f.secretID+`"}`)

	zTok := f.mintOperatorToken(t, "z")
	id := f.startSession(t, "z", `{"url":"`+f.url+`","auth_method":"token","token":"`+linkedSecretValue+`"}`, false, zTok)
	base := "/api/v1/repos/z/origin-sessions/" + id
	out := escDo(f.s.OAuthHandler(), zTok, http.MethodGet, base+"/test", "").Body.String()
	require.NotContains(t, out, `"phase":"error"`, out)
	apply := escDo(f.s.OAuthHandler(), zTok, http.MethodPost, base+"/apply", `{"conflict_strategy":"local_wins"}`)
	require.Equal(t, http.StatusOK, apply.Code, apply.Body.String())
	require.NotContains(t, apply.Body.String(), `"phase":"error"`, apply.Body.String())
	rec := escDo(f.s.OAuthHandler(), zTok, http.MethodPost, base+"/commit", "")
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	o, err := f.m.Origins().Get(z.UID())
	require.NoError(t, err)
	require.Equal(t, f.secretID, o.SecretID, "the link stands")
}

func awaitJob(t *testing.T, m *repos.Manager, id string) repos.CreateStatus {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		job, ok := m.CreateJobByID(id)
		require.True(t, ok)
		st := job.Status()
		switch st.State {
		case repos.CreateDone, repos.CreateFailed, repos.CreateCancelled:
			return st
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("create %s did not finish", id)
	return repos.CreateStatus{}
}

func controlTokenInput(name, host string) control.SecretInput {
	return control.SecretInput{Name: name, Type: control.SecretTypeToken, Hosts: []string{host}, Value: control.SecretValue{Token: "t"}}
}
