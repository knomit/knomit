package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"knomit/internal/client/sessions"
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
	if err := store.SetClientInfo(ctx, "s1", "repo:u-alpha", "claude-code", "2", now); err != nil {
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
