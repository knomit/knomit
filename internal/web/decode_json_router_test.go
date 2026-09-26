package web

// decodeJSON's Content-Type rule, reached the way a browser reaches it: over a
// real 127.0.0.1 listener, through Server.Handler with every edge and gate in
// place. The helper's own table test proves the rule; these prove nothing in
// front of the handlers answers first, and that the route whose body is
// optional still works without one.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// A text/plain body is what a cross-site HTML form or no-cors fetch sends. The
// Origin check refuses it when an Origin is present; this layer refuses it
// whatever the Origin says, including none at all.
func TestDecodeJSON_PlainTextFactPostIsRefusedThroughTheRouter(t *testing.T) {
	srv, writer := originGuardServer(t, nil)
	own := "http://" + ownHostOf(srv)

	for _, c := range []struct {
		name, origin string
	}{
		{"from this listener's own page", own},
		{"with no Origin, as a browser too old to send one", ""},
	} {
		code, body, _ := originReq{method: "POST", path: factsPath, body: originGuardFactBody,
			contentType: "text/plain", origin: c.origin}.do(t, srv)
		if code != http.StatusUnsupportedMediaType || !strings.Contains(body, "Unsupported Media Type") {
			t.Fatalf("%s: text/plain POST /facts: %d %.300s; want 415 Unsupported Media Type", c.name, code, body)
		}
	}
	if writer.writeCalls != 0 {
		t.Fatalf("a text/plain body reached the fact writer %d time(s)", writer.writeCalls)
	}

	// Control: the same request as JSON is the write it always was.
	code, body, _ := originReq{method: "POST", path: factsPath, body: originGuardFactBody,
		contentType: "application/json", origin: own}.do(t, srv)
	if code != http.StatusCreated || writer.writeCalls != 1 {
		t.Fatalf("same-origin JSON POST: %d %.300s writeCalls=%d; want 201 and one write", code, body, writer.writeCalls)
	}
}

// Experiment commit takes an optional body: the web UI sends none for an
// ordinary commit, rollback or sync. That must keep working through the whole
// handler, while a body that is present must still be JSON.
func TestDecodeOptionalJSON_ExperimentCommitThroughTheRouter(t *testing.T) {
	m := repos.New(context.Background(), repos.Deps{
		Cfg:         config.Config{Home: t.TempDir(), OntologyRoot: "kb"},
		AgentBranch: "agent/test",
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })
	newE2EMount(t, m, resolutionRepo, false)

	s := &Server{Manager: m, Auth: config.Defaults().Auth, OntologyRoot: "kb"}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	own := "http://" + ownHostOf(srv)

	open := func(name string) {
		t.Helper()
		require.NoError(t, m.Get(resolutionRepo).WithRead(func(svc *store.Service) {
			_, err := svc.Experiments().OpenExperiment(context.Background(), name, "", "agent/test")
			require.NoError(t, err)
		}))
	}
	commitPath := func(name string) string {
		return "/api/v1/repos/" + resolutionRepo + "/experiments/" + name + "/commit"
	}

	// A body that is present but not JSON: refused before anything merges.
	open("plain")
	code, body, _ := originReq{method: "POST", path: commitPath("plain"),
		body: `{"resolutions":{}}`, contentType: "text/plain", origin: own}.do(t, srv)
	if code != http.StatusUnsupportedMediaType || !strings.Contains(body, "Unsupported Media Type") {
		t.Fatalf("commit with a text/plain body: %d %.300s; want 415", code, body)
	}
	// Still open: the refusal committed nothing.
	code, body, _ = originReq{method: "POST", path: commitPath("plain"), origin: own}.do(t, srv)
	if code != http.StatusNoContent {
		t.Fatalf("the refused experiment is no longer committable: %d %.300s", code, body)
	}

	// No body and no Content-Type, exactly as the web UI sends it.
	open("bare")
	code, body, _ = originReq{method: "POST", path: commitPath("bare"), origin: own}.do(t, srv)
	if code != http.StatusNoContent {
		t.Fatalf("body-less commit: %d %.300s; want 204", code, body)
	}
}
