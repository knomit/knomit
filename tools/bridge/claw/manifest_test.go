package claw

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSnapshotTools(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
			ID     int    `json:"id"`
		}
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		_ = json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"serverInfo":{"name":"knomit"}}}`))
		case "tools/list":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"knomit_query","description":"Search.","inputSchema":{"type":"object"}}]}}`))
		default:
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()

	out, err := SnapshotTools(srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("SnapshotTools: %v", err)
	}
	if !strings.Contains(string(out), `"knomit_query"`) {
		t.Fatalf("manifest missing tool: %s", out)
	}
	var got []map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("manifest is not a JSON array: %v", err)
	}
	if len(got) != 1 || got[0]["name"] != "knomit_query" {
		t.Fatalf("unexpected manifest: %s", out)
	}
}

// TestSnapshotTools_RequiresSessionID mirrors the real knomit server: it
// issues an Mcp-Session-Id on the initialize response and rejects any
// later request that doesn't echo it back (404 "Invalid session ID").
// A live probe against http://localhost:19278 surfaced this — the plain
// httptest double above doesn't exercise it since it never checks the
// header, so it passed even before SnapshotTools threaded the session ID
// through.
func TestSnapshotTools_RequiresSessionID(t *testing.T) {
	const wantSession = "mcp-session-test-1234"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
			ID     int    `json:"id"`
		}
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		_ = json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", wantSession)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"serverInfo":{"name":"knomit"}}}`))
		case "tools/list":
			if r.Header.Get("Mcp-Session-Id") != wantSession {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"error":{"code":-32001,"message":"Invalid session ID"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"knomit_query","description":"Search.","inputSchema":{"type":"object"}}]}}`))
		default:
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()

	out, err := SnapshotTools(srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("SnapshotTools: %v", err)
	}
	if !strings.Contains(string(out), `"knomit_query"`) {
		t.Fatalf("manifest missing tool: %s", out)
	}
}
