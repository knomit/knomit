package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"knomit/internal/client/sessions"
	"knomit/internal/version"
)

func TestRunProxy_HeadersOnEveryRequestAndDeleteOnEOF(t *testing.T) {
	type seen struct {
		method, sid, client, ua string
	}
	var (
		mu   sync.Mutex
		reqs []seen
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs = append(reqs, seen{r.Method, r.Header.Get("Mcp-Session-Id"), r.Header.Get(sessions.ClientHeader), r.Header.Get("User-Agent")})
		mu.Unlock()
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "mcp-session-abc")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer srv.Close()

	ident := buildIdentity("agent/x", time.Now())
	hdr := clientHeaders(ident)
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n")
	var out bytes.Buffer
	sid, err := runProxy(in, &out, srv.Client(), srv.URL, hdr)
	if err != nil {
		t.Fatal(err)
	}
	if sid != "mcp-session-abc" {
		t.Fatalf("session id %q", sid)
	}
	terminateSession(srv.Client(), srv.URL, sid, hdr)

	mu.Lock()
	defer mu.Unlock()
	if len(reqs) != 3 {
		t.Fatalf("requests: %+v", reqs)
	}
	for i, r := range reqs {
		if r.ua != "knomit-bridge/"+version.String() || r.client == "" {
			t.Fatalf("req %d missing identity headers: %+v", i, r)
		}
		if got, err := sessions.ParseClientHeader(r.client); err != nil || got.InstanceID != ident.InstanceID {
			t.Fatalf("req %d header: %+v err=%v", i, got, err)
		}
	}
	if reqs[0].sid != "" || reqs[1].sid != "mcp-session-abc" {
		t.Fatalf("session threading: %+v", reqs)
	}
	if reqs[2].method != http.MethodDelete || reqs[2].sid != "mcp-session-abc" {
		t.Fatalf("no DELETE after EOF: %+v", reqs[2])
	}
	if !strings.Contains(out.String(), `"id":1`) {
		t.Fatalf("responses not forwarded: %s", out.String())
	}
}

// Exit must never wait on the server: no session means no request at all,
// and an unreachable server is a debug line, not a delay.
func TestTerminateSession_NoSessionOrDeadServerIsQuiet(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		terminateSession(&http.Client{}, "http://127.0.0.1:1", "", nil)
		terminateSession(&http.Client{}, "http://127.0.0.1:1", "mcp-session-x", nil)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("terminateSession blocked on a dead server")
	}
}
