package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeCreateServer answers POST /api/v1/repos with a 202 running job and
// then `final` on the second poll. It records the create body.
func fakeCreateServer(t *testing.T, final map[string]any, gotBody *map[string]any) *httptest.Server {
	t.Helper()
	var polls atomic.Int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos":
			require.NoError(t, json.NewDecoder(r.Body).Decode(gotBody))
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{"create_id": "c1", "state": "running"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repo-creates/c1":
			if polls.Add(1) < 2 {
				_ = json.NewEncoder(w).Encode(map[string]any{"create_id": "c1", "state": "running"})
				return
			}
			_ = json.NewEncoder(w).Encode(final)
		default:
			http.NotFound(w, r)
		}
	}))
}

// The create body names mode "template" and template {repo, name}; the
// command polls until the job ends and exits 0 only on done.
// SABOTAGE: return nil on a failed job → the failure row goes red.
func TestRepoCreate_TemplateBodyAndPoll(t *testing.T) {
	repoCreatePoll = time.Millisecond
	var body map[string]any
	srv := fakeCreateServer(t, map[string]any{"create_id": "c1", "state": "done"}, &body)
	defer srv.Close()
	var out bytes.Buffer
	require.NoError(t, createFromTemplate(context.Background(), srv.Client(), srv.URL, "my-kb", "knomit-playbooks", "general", &out))
	require.Equal(t, "template", body["mode"])
	require.Equal(t, map[string]any{"repo": "knomit-playbooks", "name": "general"}, body["template"])
	require.Contains(t, out.String(), "created repo my-kb from template knomit-playbooks/general")

	srv2 := fakeCreateServer(t, map[string]any{"create_id": "c1", "state": "failed", "error": "template invalid: x"}, &body)
	defer srv2.Close()
	err := createFromTemplate(context.Background(), srv2.Client(), srv2.URL, "my-kb", "p", "x", &out)
	require.ErrorContains(t, err, "template invalid: x")
}

// A refusal before the job starts is a problem+json status: the error carries
// its title and detail.
func TestRepoCreate_RefusalIsNamed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"title":"Template source not found","detail":"template source repo is not mounted on this instance: \"nope\""}`))
	}))
	defer srv.Close()
	err := createFromTemplate(context.Background(), srv.Client(), srv.URL, "a", "nope", "x", &bytes.Buffer{})
	require.ErrorContains(t, err, "404 Template source not found")
	require.ErrorContains(t, err, "nope")
}

// --template needs both halves; flags may follow the positional.
func TestRunRepo_Usage(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"delete", "x"},
		{"create"},
		{"create", "x"},
		{"create", "x", "--template", "only-repo"},
		{"create", "x", "--template", "/name"},
		{"create", "x", "--template", "a/b/c"},
	} {
		require.Error(t, runRepo(args, &bytes.Buffer{}), strings.Join(args, " "))
	}
}

// N5: `kb repo` is the first argument exactly; the proxy's -repo flag never
// dispatches to it.
func TestIsRepoCommand(t *testing.T) {
	require.True(t, isRepoCommand([]string{"repo", "create", "x"}))
	for _, args := range [][]string{{"-repo", "work", "http://h"}, {"--repo", "work"}, {"http://h"}, nil} {
		require.False(t, isRepoCommand(args), "%v", args)
	}
}
