package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/corpus/repos/control"
	"knomit/internal/engine/store"
	"knomit/internal/foundation/auth"
	"knomit/test/testenv/gitserver"
)

const linkedSecretValue = "LINKED-zz9-secret-value"

// linkedFixture: repo x cloned over smart HTTP (127.0.0.1) with a stored
// token secret "gh-pat" whose hosts list is 127.0.0.1. otherURL is the same
// server under another host name (localhost), not on the list.
type linkedFixture struct {
	*escFixture
	ri       *repos.RepoInstance
	srv      *gitserver.Server
	url      string
	otherURL string
	secretID string
	opTok    string
	me       string
}

func newLinkedFixture(t *testing.T) *linkedFixture {
	t.Helper()
	f := newEscFixture(t)
	root := filepath.Join(t.TempDir(), "srv")
	bare := filepath.Join(root, "kb.git")
	seedBareRemoteKBForTest(t, bare)
	runGitForTest(t, bare, "config", "http.receivepack", "true")
	srv := gitserver.New(t, root)
	t.Cleanup(srv.Close)
	srv.Fault().RequireBasicAuth("x-token", linkedSecretValue)
	url := srv.URL + "/kb.git"
	meta, err := f.m.Secrets().Create(control.SecretInput{Name: "gh-pat", Type: control.SecretTypeToken,
		Hosts: []string{"127.0.0.1"}, Value: control.SecretValue{Token: linkedSecretValue}}, time.Now())
	require.NoError(t, err)
	ri, err := f.m.Create(context.Background(), repos.CreateSpec{Name: "x", Mode: "clone",
		Origin: &repos.OriginSpec{URL: url, Branch: "main", AuthMethod: "token", SecretID: meta.ID}}, nil)
	require.NoError(t, err)
	// The boot seed for the local principal (app.seedOwnPrincipal), plus
	// admin: the socket caller is the host's owner.
	me, err := auth.LocalPrincipal()
	require.NoError(t, err)
	set, err := auth.ParseSet(f.s.Auth.EffectiveLoopbackDefault())
	require.NoError(t, err)
	set[auth.Admin] = struct{}{}
	for p := range set {
		require.NoError(t, f.grants.Grant(context.Background(), me, p, "boot:loopback_default"))
	}
	return &linkedFixture{escFixture: f, ri: ri, srv: srv, url: url, me: me.ID,
		otherURL: strings.Replace(url, "127.0.0.1", "localhost", 1),
		secretID: meta.ID, opTok: f.mintOperatorToken(t, "x")}
}

// admin sends a request as a local principal (the socket), which passes the
// admin gate.
func (f *linkedFixture) admin(method, path, body string) *httptest.ResponseRecorder {
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.RemoteAddr = "@"
	return serve(f.s.Handler(), req.WithContext(auth.WithPeer(req.Context(), auth.Peer{ID: f.me, Via: auth.LocalVia})))
}

// operator sends a request with an operator bearer (no admin).
func (f *linkedFixture) operator(method, path, body string) *httptest.ResponseRecorder {
	return escDo(f.s.OAuthHandler(), f.opTok, method, path, body)
}

func (f *linkedFixture) requireLinked(t *testing.T, url string) {
	t.Helper()
	o, err := f.m.Origins().Get(f.ri.UID())
	require.NoError(t, err)
	require.NotNil(t, o)
	require.Equal(t, f.secretID, o.SecretID)
	require.Equal(t, url, o.URL)
	require.NoError(t, f.ri.WithRead(func(svc *store.Service) {
		mem := svc.Remote().InjectedOrigin()
		require.NotNil(t, mem)
		require.Equal(t, f.secretID, mem.SecretID)
		require.Equal(t, url, mem.URL)
	}))
}

func TestOriginGet_SecretFieldsAdminOnly(t *testing.T) {
	f := newLinkedFixture(t)
	exp := time.Now().Add(5 * 24 * time.Hour)
	require.NoError(t, f.m.Secrets().SetExpiry(f.secretID, &exp, time.Now()))

	rec := f.admin(http.MethodGet, "/api/v1/repos/x/origin", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var view map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &view))
	require.Equal(t, f.secretID, view["secret_id"])
	require.Equal(t, true, view["secret_linked"])
	require.Equal(t, "gh-pat", view["secret_name"])
	require.Equal(t, "expiring", view["secret_status"])
	require.NotEmpty(t, view["secret_expires_at"])
	require.NotContains(t, rec.Body.String(), linkedSecretValue)

	reader := f.mintRepoToken(t, "x")
	rec = escDo(f.s.OAuthHandler(), reader, http.MethodGet, "/api/v1/repos/x/origin", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	view = nil
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &view))
	require.NotContains(t, view, "secret_id")
	require.Equal(t, true, view["secret_linked"], "every reader learns the repo is linked")
	require.Equal(t, "gh-pat", view["secret_name"])
	require.Equal(t, "expiring", view["secret_status"])
	require.NotContains(t, rec.Body.String(), linkedSecretValue)
}

func TestOriginPut_LinkedRefusesPastedCredential(t *testing.T) {
	f := newLinkedFixture(t)
	rec := f.operator(http.MethodPut, "/api/v1/repos/x/origin", `{"url":"`+f.url+`","auth_method":"token","token":"pasted"}`)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "secret_id")
	f.requireLinked(t, f.url)
}

func TestOriginPut_LinkedURLChangeNeedsAdmin(t *testing.T) {
	f := newLinkedFixture(t)
	rec := f.operator(http.MethodPut, "/api/v1/repos/x/origin", `{"url":"`+f.otherURL+`"}`)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	f.requireLinked(t, f.url)

	rec = f.admin(http.MethodPut, "/api/v1/repos/x/origin", `{"url":"`+f.otherURL+`"}`)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "gh-pat")
	require.Contains(t, rec.Body.String(), "localhost")
	f.requireLinked(t, f.url)
	for _, a := range f.srv.Fault().AuthAttempts() {
		require.True(t, a.OK || a.Password != linkedSecretValue, "the secret was sent and refused")
	}
}

func TestOriginPut_LinkedPartialKeepsLink(t *testing.T) {
	f := newLinkedFixture(t)
	rec := f.operator(http.MethodPut, "/api/v1/repos/x/origin", `{"branch":"main"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	f.requireLinked(t, f.url)
	// The same URL spelled out is not a URL change: operator is enough.
	rec = f.operator(http.MethodPut, "/api/v1/repos/x/origin", `{"url":"`+f.url+`","branch":"main"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	f.requireLinked(t, f.url)
}

func TestOriginDelete_LinkedNeedsAdmin(t *testing.T) {
	f := newLinkedFixture(t)
	rec := f.operator(http.MethodDelete, "/api/v1/repos/x/origin", "")
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	f.requireLinked(t, f.url)

	rec = f.admin(http.MethodDelete, "/api/v1/repos/x/origin", "")
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	o, err := f.m.Origins().Get(f.ri.UID())
	require.NoError(t, err)
	require.Nil(t, o)
	require.NoError(t, f.m.Secrets().Delete(f.secretID), "the detach dropped the link")
}

// PATCH /origin/upstream re-injects the origin from memory: the link stays.
func TestCarry_PatchUpstream(t *testing.T) {
	f := newLinkedFixture(t)
	rec := f.operator(http.MethodPatch, "/api/v1/repos/x/origin/upstream", `{"branch":"main"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	f.requireLinked(t, f.url)
}
