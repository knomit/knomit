package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	mcpserver "github.com/mark3labs/mcp-go/server"

	"knomit/internal/fact"
	kmcp "knomit/internal/mcp"
	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/testsupport/testsigner"
)

// T-C7 (F08 PR C): the bridge is a line-for-line JSON-RPC proxy, so the
// skills a repo-scoped mount serves as MCP prompts reach a stdio client
// through it unchanged — prompts/list and prompts/get included. The server
// behind the proxy is knomit's REAL MCP server over streamable HTTP, with the
// repo in the request context as the repo-scoped mount puts it there.
func TestBridge_ProxiesPrompts(t *testing.T) {
	svc, err := store.Open(filepath.Join(t.TempDir(), "k.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	svc.SetSigner(testsigner.Signer())
	if err := svc.InitRepo(context.Background(), map[string]string{}, "agent/test"); err != nil {
		t.Fatal(err)
	}
	src := "---\nname: work-task\ndescription: Work one task.\n---\nWORK BODY\n"
	if _, err := svc.Facts().WriteFact(context.Background(), svc.UpstreamBranch(), fact.SkillPath("work-task"), src, "skill", "updated"); err != nil {
		t.Fatal(err)
	}
	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "mission", UID: "uid-mission", AgentBranch: "agent/test", Svc: svc,
		Ontology: fact.CodeOntology(), OntologyRoot: "kb",
	})

	mcpHTTP := mcpserver.NewStreamableHTTPServer(kmcp.NewServer("kb", nil, false, nil))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := repos.WithBranch(repos.WithRepoInstance(r.Context(), ri), "agent/test")
		mcpHTTP.ServeHTTP(w, r.WithContext(ctx))
	}))
	defer srv.Close()

	in := strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"bridge-test","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"prompts/list","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"prompts/get","params":{"name":"work-task"}}`,
	}, "\n") + "\n")
	var out bytes.Buffer
	if _, err := runProxy(in, &out, srv.Client(), srv.URL, http.Header{}); err != nil {
		t.Fatal(err)
	}

	byID := map[float64]map[string]any{}
	sc := bufio.NewScanner(&out)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		var msg map[string]any
		if json.Unmarshal(sc.Bytes(), &msg) != nil {
			continue
		}
		if id, ok := msg["id"].(float64); ok {
			byID[id] = msg
		}
	}
	list, ok := byID[2]["result"].(map[string]any)
	if !ok {
		t.Fatalf("no prompts/list result through the proxy: %s", out.String())
	}
	prompts, _ := list["prompts"].([]any)
	if len(prompts) != 1 || prompts[0].(map[string]any)["name"] != "work-task" {
		t.Fatalf("prompts/list through the proxy: %v", list)
	}
	get, ok := byID[3]["result"].(map[string]any)
	if !ok {
		t.Fatalf("no prompts/get result through the proxy: %s", out.String())
	}
	raw, _ := json.Marshal(get)
	if !strings.Contains(string(raw), "WORK BODY") {
		t.Fatalf("prompts/get body missing: %s", raw)
	}
}
