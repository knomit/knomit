package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/client/sessions"
	"knomit/internal/repos"
	"knomit/internal/web/hal"
)

func TestHandleHALClientSessions(t *testing.T) {
	store := newClientSessionsStore(t)
	m := newTestManagerWithUIDRepo(t, "alpha", "u-alpha")
	now := time.Now()
	ctx := context.Background()
	if err := store.Touch(ctx, sessions.Observation{
		SessionID: "s1", Binding: "repo:u-alpha", RemoteIP: "1.2.3.4", UserAgent: "ua", Now: now,
		Client: &sessions.BridgeInfo{InstanceID: "i1", Transport: "stdio", PID: 5, ParentApp: "claude",
			Host: "h", User: "u", Cwd: "/w", Branch: "agent/test", Version: "1"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetClientInfo(ctx, "s1", "repo:u-alpha", "claude-code", "2", "1.2.3.4", "ua", now); err != nil {
		t.Fatal(err)
	}
	if err := store.Touch(ctx, sessions.Observation{SessionID: "old", Binding: "lens:gone", Now: now.Add(-4 * time.Hour)}); err != nil {
		t.Fatal(err)
	}

	s := &Server{Manager: m, ClientSessions: store}
	r := s.NewAPIRouter()

	get := func(q string) map[string]any {
		t.Helper()
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions"+q, nil))
		if rec.Code != 200 || rec.Header().Get("Content-Type") != hal.ContentType {
			t.Fatalf("status=%d ct=%s body=%s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}

	body := get("")
	items := body["_embedded"].(map[string]any)["sessions"].([]any)
	if len(items) != 1 || body["count"].(float64) != 1 {
		t.Fatalf("default hides the 4h-old row: %v", body)
	}
	it := items[0].(map[string]any)
	if it["id"] != "s1" || it["state"] != "live" || it["transport"] != "stdio" {
		t.Fatalf("%v", it)
	}
	b := it["binding"].(map[string]any)
	if b["kind"] != "repo" || b["uid"] != "u-alpha" || b["name"] != "alpha" {
		t.Fatalf("binding: %v", b)
	}
	c := it["client"].(map[string]any)
	if c["name"] != "claude-code" || c["initialized"] != true {
		t.Fatalf("client: %v", c)
	}
	br := it["bridge"].(map[string]any)
	if br["parent"] != "claude" || br["pid"].(float64) != 5 {
		t.Fatalf("bridge: %v", br)
	}
	pol := body["policy"].(map[string]any)
	if pol["dead_after_s"].(float64) != 3600 || pol["hidden_after_s"].(float64) != 10800 || pol["live_window_s"].(float64) != 360 {
		t.Fatalf("policy: %v", pol)
	}

	body = get("?include=hidden")
	items = body["_embedded"].(map[string]any)["sessions"].([]any)
	if len(items) != 2 {
		t.Fatalf("include=hidden: %d", len(items))
	}
	for _, x := range items {
		it := x.(map[string]any)
		if it["id"] == "old" {
			b := it["binding"].(map[string]any)
			if b["kind"] != "lens" || b["uid"] != "gone" || b["name"] != nil {
				t.Fatalf("unresolvable binding must keep uid with null name: %v", b)
			}
		}
	}

	body = get("?binding=lens:gone&include=hidden")
	if body["count"].(float64) != 1 {
		t.Fatalf("binding filter: %v", body)
	}
}

func TestHandleHALClientSessions_NoStore503(t *testing.T) {
	s := &Server{Manager: newTestManagerWithUIDRepo(t, "alpha", "u-alpha")}
	rec := httptest.NewRecorder()
	s.NewAPIRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d", rec.Code)
	}
}

// ReadOnly is the public read-only DEMO mode, so the session list must not
// hand an anonymous visitor the operator's hostname, username, working
// directories, pids or IP. Presence itself stays visible.
func TestHandleHALClientSessions_ReadOnlyRedactsOperatorDetail(t *testing.T) {
	store := newClientSessionsStore(t)
	m := newTestManagerWithUIDRepo(t, "alpha", "u-alpha")
	now := time.Now()
	ctx := context.Background()
	if err := store.Touch(ctx, sessions.Observation{
		SessionID: "s1", Binding: "repo:u-alpha", RemoteIP: "203.0.113.7", UserAgent: "knomit-bridge/1.4", Now: now,
		Client: &sessions.BridgeInfo{InstanceID: "i1", Transport: "stdio", PID: 5, ParentPID: 4, ParentApp: "claude",
			Host: "h1v302", User: "pba", Cwd: "/home/pba/secret-project", Branch: "agent/h1v302-8215ac8f", Version: "1.4"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetClientInfo(ctx, "s1", "repo:u-alpha", "claude-code", "2.1", "203.0.113.7", "knomit-bridge/1.4", now); err != nil {
		t.Fatal(err)
	}

	get := func(readOnly bool) map[string]any {
		t.Helper()
		s := &Server{Manager: m, ClientSessions: store, ReadOnly: readOnly}
		rec := httptest.NewRecorder()
		s.NewAPIRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions", nil))
		if rec.Code != 200 {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body["_embedded"].(map[string]any)["sessions"].([]any)[0].(map[string]any)
	}

	// Normal mode: the operator sees everything.
	open := get(false)
	openBridge := open["bridge"].(map[string]any)
	if openBridge["host"] != "h1v302" || openBridge["cwd"] != "/home/pba/secret-project" ||
		openBridge["pid"].(float64) != 5 || open["remote_addr"] != "203.0.113.7" ||
		open["branch"] != "agent/h1v302-8215ac8f" || open["user_agent"] != "knomit-bridge/1.4" {
		t.Fatalf("normal mode must not redact: %v", open)
	}

	// Read-only demo: operator detail is gone.
	ro := get(true)
	roBridge := ro["bridge"].(map[string]any)
	for k, v := range map[string]any{"host": "", "user": "", "cwd": "", "parent": "", "version": ""} {
		if roBridge[k] != v {
			t.Errorf("bridge.%s = %v, want %q", k, roBridge[k], v)
		}
	}
	if roBridge["pid"].(float64) != 0 || roBridge["parent_pid"].(float64) != 0 {
		t.Errorf("pids not zeroed: %v", roBridge)
	}
	if ro["remote_addr"] != "" || ro["user_agent"] != "" || ro["branch"] != "" {
		t.Errorf("remote_addr/user_agent/branch not redacted: %v", ro)
	}

	// What presence NEEDS is still there.
	if ro["id"] != "s1" || ro["state"] != "live" || ro["transport"] != "stdio" || ro["instance_id"] != open["instance_id"] {
		t.Errorf("redaction removed the presence fields: %v", ro)
	}
	if ro["client"].(map[string]any)["name"] != "claude-code" {
		t.Errorf("client name must survive: %v", ro["client"])
	}
	if ro["binding"].(map[string]any)["name"] != "alpha" || ro["request_count"].(float64) != 1 {
		t.Errorf("binding/request_count must survive: %v", ro)
	}
}

// The name lookup is built ONCE per response, not once per row: control.db
// runs at SetMaxOpenConns(1) and the UI polls hundreds of rows every 30s, so
// a per-row lookup would serialise the whole page behind one connection.
// This pins the behaviour that index must preserve — every row resolved,
// across both kinds and a uid that resolves to nothing.
func TestHandleHALClientSessions_ResolvesEveryRow(t *testing.T) {
	store := newClientSessionsStore(t)
	m := newTestManagerWithUIDRepo(t, "alpha", "u-alpha")
	m.Set("beta", repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{Name: "beta", UID: "u-beta", AgentBranch: "agent/test"}))

	now := time.Now()
	ctx := context.Background()
	seed := func(id, binding string) {
		t.Helper()
		if err := store.Touch(ctx, sessions.Observation{SessionID: id, Binding: binding, Now: now}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 5; i++ {
		seed(fmt.Sprintf("a%d", i), "repo:u-alpha")
		seed(fmt.Sprintf("b%d", i), "repo:u-beta")
		seed(fmt.Sprintf("g%d", i), "lens:gone")
	}

	s := &Server{Manager: m, ClientSessions: store}
	rec := httptest.NewRecorder()
	s.NewAPIRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions", nil))
	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	items := body["_embedded"].(map[string]any)["sessions"].([]any)
	if len(items) != 15 {
		t.Fatalf("rows=%d", len(items))
	}
	want := map[string]any{"a": "alpha", "b": "beta", "g": nil}
	for _, x := range items {
		it := x.(map[string]any)
		id := it["id"].(string)
		b := it["binding"].(map[string]any)
		if b["name"] != want[id[:1]] {
			t.Errorf("%s: binding.name = %v, want %v", id, b["name"], want[id[:1]])
		}
	}
}

// The `bindings` array is every HANDLE the session has presented, most
// recently used first, one entry per handle — not per target. `binding` stays
// as the last-seen pin, so an older client reading only that key is unaffected.
func TestHandleHALClientSessions_BindingSet(t *testing.T) {
	store := newClientSessionsStore(t)
	m := newTestManagerWithUIDRepo(t, "alpha", "u-alpha")
	now := time.Now()
	ctx := context.Background()

	require.NoError(t, store.Touch(ctx, sessions.Observation{
		SessionID: "s1", Binding: "repo:u-alpha", Now: now,
		Client: &sessions.BridgeInfo{InstanceID: "i1", Transport: "stdio", Branch: "agent/test"},
	}))
	// TWO handles naming the SAME repo — two callers, two rows. hB is used
	// later, so it must sort first.
	require.NoError(t, store.RecordSessionBinding(ctx, "s1", "hA", "repo:u-alpha", "", now.Add(-time.Minute)))
	require.NoError(t, store.RecordSessionBinding(ctx, "s1", "hB", "repo:u-alpha", "main", now))
	require.NoError(t, store.RecordSessionBinding(ctx, "s1", "hB", "repo:u-alpha", "main", now))

	s := &Server{Manager: m, ClientSessions: store}
	rec := httptest.NewRecorder()
	s.NewAPIRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions", nil))
	require.Equal(t, 200, rec.Code)

	var body struct {
		Embedded struct {
			Sessions []clientSessionView `json:"sessions"`
		} `json:"_embedded"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Embedded.Sessions, 1)
	row := body.Embedded.Sessions[0]

	require.Equal(t, "u-alpha", row.Binding.UID, "the singular field stays the last-seen pin")
	require.Len(t, row.Bindings, 2, "two handles on one repo are two entries")
	require.Equal(t, "hB", row.Bindings[0].Handle, "most recently used first")
	require.Equal(t, "hA", row.Bindings[1].Handle)
	require.Equal(t, "repo", row.Bindings[0].Kind)
	require.Equal(t, "u-alpha", row.Bindings[0].UID)
	require.NotNil(t, row.Bindings[0].Name)
	require.Equal(t, "alpha", *row.Bindings[0].Name, "the name resolves through the same one-pass index")
	require.Equal(t, "main", row.Bindings[0].Branch)
	require.Equal(t, 2, row.Bindings[0].RequestCount)
	require.Equal(t, 1, row.Bindings[1].RequestCount)
}

// A session that has only used URL-scoped mounts presents no handle, so its
// set is empty — and the key is still an array, never null, because every
// consumer maps over it.
func TestHandleHALClientSessions_NoHandlesIsEmptyArray(t *testing.T) {
	store := newClientSessionsStore(t)
	m := newTestManagerWithUIDRepo(t, "alpha", "u-alpha")
	require.NoError(t, store.Touch(context.Background(), sessions.Observation{
		SessionID: "s1", Binding: "repo:u-alpha", Now: time.Now(),
	}))

	s := &Server{Manager: m, ClientSessions: store}
	rec := httptest.NewRecorder()
	s.NewAPIRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions", nil))
	require.Contains(t, rec.Body.String(), `"bindings":[]`,
		"the key must serialize as [] and never as null")
}

// ?binding= matches through ANY handle in the set, not only the last-seen pin,
// and returns the session once however many of its handles match.
func TestHandleHALClientSessions_FilterMatchesAnyHandle(t *testing.T) {
	store := newClientSessionsStore(t)
	m := newTestManagerWithUIDRepo(t, "alpha", "u-alpha")
	now := time.Now()
	ctx := context.Background()

	// Last seen on u-other; two live handles on u-alpha.
	require.NoError(t, store.Touch(ctx, sessions.Observation{SessionID: "s1", Binding: "repo:u-other", Now: now}))
	require.NoError(t, store.RecordSessionBinding(ctx, "s1", "hA", "repo:u-alpha", "", now))
	require.NoError(t, store.RecordSessionBinding(ctx, "s1", "hB", "repo:u-alpha", "", now))
	// An unrelated session, to prove the filter still excludes.
	require.NoError(t, store.Touch(ctx, sessions.Observation{SessionID: "s2", Binding: "repo:u-zzz", Now: now}))

	s := &Server{Manager: m, ClientSessions: store}
	count := func(q string) int {
		t.Helper()
		rec := httptest.NewRecorder()
		s.NewAPIRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions?binding="+q, nil))
		require.Equal(t, 200, rec.Code)
		var body struct {
			Count int `json:"count"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		return body.Count
	}
	require.Equal(t, 1, count("repo:u-alpha"), "matched through the set, and counted ONCE")
	require.Equal(t, 1, count("repo:u-other"), "still matches the last-seen pin")
	require.Equal(t, 0, count("repo:u-nope"))
}

// READ-ONLY redacts the handle and the branch inside each binding row: a handle
// is a live routing capability and an agent branch embeds the hostname. What
// the session is bound TO stays visible — that is the presence story.
func TestHandleHALClientSessions_ReadOnlyRedactsHandleAndBranch(t *testing.T) {
	store := newClientSessionsStore(t)
	m := newTestManagerWithUIDRepo(t, "alpha", "u-alpha")
	now := time.Now()
	ctx := context.Background()
	require.NoError(t, store.Touch(ctx, sessions.Observation{SessionID: "s1", Binding: "repo:u-alpha", Now: now}))
	require.NoError(t, store.RecordSessionBinding(ctx, "s1", "secret-handle", "repo:u-alpha", "agent/hostname", now))

	s := &Server{Manager: m, ClientSessions: store, ReadOnly: true}
	rec := httptest.NewRecorder()
	s.NewAPIRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions", nil))
	require.Equal(t, 200, rec.Code)
	require.NotContains(t, rec.Body.String(), "secret-handle", "a live handle must not reach a demo visitor")
	require.NotContains(t, rec.Body.String(), "agent/hostname", "an agent branch embeds the hostname")

	var body struct {
		Embedded struct {
			Sessions []clientSessionView `json:"sessions"`
		} `json:"_embedded"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Embedded.Sessions, 1)
	require.Len(t, body.Embedded.Sessions[0].Bindings, 1, "the entry survives; only two fields are stripped")
	require.Equal(t, "alpha", *body.Embedded.Sessions[0].Bindings[0].Name)
	require.Equal(t, "", body.Embedded.Sessions[0].Bindings[0].Handle)
	require.Equal(t, "", body.Embedded.Sessions[0].Bindings[0].Branch)
}
