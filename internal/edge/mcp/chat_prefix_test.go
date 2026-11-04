package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/corpus/repos/control"
	"knomit/internal/domain/fact"
	"knomit/internal/engine/llm"
	"knomit/internal/engine/store"
	"knomit/internal/foundation/auth"
	"knomit/internal/foundation/config"
	"knomit/internal/testsupport/testsigner"
)

// prefixInstance is one knomit server as a fleet member: its own home, agent
// branch, host label, repo and principal. Every axis differs between the
// instances of a test (pairwise-distinct values).
type prefixInstance struct {
	home, branch, host, repo string
	principal                auth.Principal
	write                    bool
}

func (in prefixInstance) grants() auth.StaticGrants {
	set := auth.Set{auth.Read: {}}
	if in.write {
		set[auth.Write] = struct{}{}
	}
	return auth.StaticGrants{in.principal.String(): set}
}

// startPrefixManager starts a manager for in with one registered, mounted repo.
func startPrefixManager(t *testing.T, in prefixInstance) *repos.Manager {
	t.Helper()
	m := repos.New(context.Background(), repos.Deps{
		Cfg:         config.Config{Home: in.home},
		AgentID:     testsigner.FixtureAgentID(in.branch),
		AgentBranch: in.branch,
		AgentHost:   in.host,
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	svc, err := store.Open(filepath.Join(in.home, in.repo+".db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, in.branch))
	uid := "uid-" + in.repo
	require.NoError(t, m.Repos().Insert(control.RepoRecord{UID: uid, Name: in.repo, State: control.StateActive, Profile: "code", CreatedAt: 1}))
	m.Set(in.repo, repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: in.repo, UID: uid, AgentBranch: in.branch, Svc: svc,
		Ontology: fact.CodeOntology(), OntologyRoot: "kb",
	}))
	return m
}

// prefixRun is what one chat session start produces.
type prefixRun struct {
	canonical []byte // CanonicalPrefix(HookRepo)
	anthropic []byte // "system" and "tools" of the HTTP body the API received
	tools     []llm.ToolDef
}

// wirePrefix sends s through the real Anthropic adapter to an httptest server
// and returns the "system" and "tools" bytes of the body it received, as
// {"system":…,"tools":…}. It also checks them against llm.AnthropicPrefixJSON,
// so that export stays exactly what the API gets.
func wirePrefix(t *testing.T, s llm.Snapshot) []byte {
	t.Helper()
	var (
		mu   sync.Mutex
		body []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		body = b
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range []string{
			`{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"claude-test-x","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`,
			`{"type":"message_stop"}`,
		} {
			var typ struct{ Type string }
			_ = json.Unmarshal([]byte(e), &typ)
			_, _ = io.WriteString(w, "event: "+typ.Type+"\ndata: "+e+"\n\n")
		}
	}))
	defer srv.Close()
	chat := llm.NewAnthropicChat("claude-test-x", llm.AnthropicOptions{APIKey: "k-test", BaseURL: srv.URL})
	_, err := chat.Stream(context.Background(), s, llm.StreamOptions{}, nil)
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	var got map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &got))
	require.NotEmpty(t, got["system"])
	require.NotEmpty(t, got["tools"])
	wire := []byte(`{"system":` + string(got["system"]) + `,"tools":` + string(got["tools"]) + `}`)

	exported, err := llm.AnthropicPrefixJSON(s, "claude-test-x")
	require.NoError(t, err)
	require.Equal(t, string(exported), string(wire), "AnthropicPrefixJSON differs from the received body")
	return wire
}

// hookLine returns hook h's line of a CanonicalPrefix.
func hookLine(t *testing.T, prefix []byte, h llm.Hook) string {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(string(prefix), "\n"), "\n")
	require.GreaterOrEqual(t, len(lines), int(h))
	return lines[h-1]
}

// chatCtx is the context every in-process chat call carries: the principal
// and the unscoped endpoint kind.
func chatCtx(in prefixInstance) context.Context {
	return auth.WithPrincipal(repos.WithSessionScoped(context.Background()), in.principal)
}

// runPrefix builds a fresh server and in-process client on mgr and loads the
// shared prefix, as a chat start does.
func runPrefix(t *testing.T, mgr *repos.Manager, in prefixInstance) prefixRun {
	t.Helper()
	srv := NewServer("kb", mgr, false, in.grants())
	c, err := client.NewInProcessClient(srv)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	ctx := chatCtx(in)
	instr, tools, err := LoadPrefix(ctx, c, mcpgo.Implementation{Name: "knomit-chat", Version: "anthropic/claude-test-x"})
	require.NoError(t, err)
	b := llm.NewBuilder()
	require.NoError(t, AddPrefix(b, instr, tools))
	require.NoError(t, b.Add(llm.Item{ID: "turn", Hook: llm.HookTurn, Text: "hello"}))
	s := b.Snapshot()
	return prefixRun{canonical: s.CanonicalPrefix(llm.HookRepo), anthropic: wirePrefix(t, s), tools: tools}
}

func runInstance(t *testing.T, in prefixInstance) []prefixRun {
	t.Helper()
	mgr := startPrefixManager(t, in)
	return []prefixRun{runPrefix(t, mgr, in), runPrefix(t, mgr, in)}
}

// exampleDate is the one date hook 1 may hold: the fixed RFC 3339 example in
// the `expires` field description (factschema.go), source text that is the
// same on every machine and every day. Any other date is a leak.
var (
	datePattern  = regexp.MustCompile(`20[0-9]{2}-[0-9]{2}-[0-9]{2}`)
	yearPattern  = regexp.MustCompile(`20[0-9]{2}-`)
	exampleDates = map[string]bool{"2026-10-01": true}
)

// machineNeedles are the real identity of the machine running the test. Both
// test instances run in one process on one machine, so a value read from the
// machine (os.Hostname, $HOME, the working directory, the temp root, the OS
// user) is EQUAL on both and passes every equality check; only these needles
// catch it. Each must be non-empty, or the check would pass vacuously.
type machineNeedles struct {
	substrings map[string]string         // name → path or full hostname
	words      map[string]*regexp.Regexp // name → short name, on word boundaries
}

// genericUsernames are account names that are also ordinary words of the
// prefix text (a CI container running as root, say): as a word they cannot
// tell a leak from prose. The user's home directory is still a needle.
var genericUsernames = map[string]bool{"root": true, "runner": true, "user": true, "admin": true, "build": true}

func realMachineNeedles(t *testing.T) machineNeedles {
	t.Helper()
	host, err := os.Hostname()
	require.NoError(t, err)
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	wd, err := os.Getwd()
	require.NoError(t, err)
	u, err := user.Current()
	require.NoError(t, err)
	tmp := filepath.Clean(os.TempDir())
	for name, v := range map[string]string{"hostname": host, "home": home, "wd": wd, "tempdir": tmp, "username": u.Username} {
		require.NotEmpty(t, v, "machine needle %s is empty", name)
	}
	n := machineNeedles{
		substrings: map[string]string{"os.Hostname": host, "os.UserHomeDir": home, "os.Getwd": wd, "os.TempDir": tmp},
		words:      map[string]*regexp.Regexp{},
	}
	if env := os.Getenv("HOME"); env != "" && env != home {
		n.substrings["$HOME"] = env
	}
	short := host
	if i := strings.IndexByte(host, '.'); i > 0 {
		short = host[:i]
	}
	n.words["short hostname"] = regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(short) + `\b`)
	if genericUsernames[strings.ToLower(u.Username)] {
		t.Logf("username %q is a generic word; checking $HOME %q for it instead", u.Username, home)
	} else {
		n.words["username"] = regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(u.Username) + `\b`)
	}
	return n
}

// requireNoMachineBytes asserts none of the instance-specific values, none of
// the real machine's identity, and no date other than the fixed example,
// appear in b.
func requireNoMachineBytes(t *testing.T, label string, b []byte, ins ...prefixInstance) {
	t.Helper()
	s := string(b)
	m := realMachineNeedles(t)
	require.GreaterOrEqual(t, len(m.substrings), 4)
	require.GreaterOrEqual(t, len(m.words), 1)
	for name, needle := range m.substrings {
		require.NotContains(t, s, needle, "%s: real machine value (%s) in the shared prefix", label, name)
	}
	for name, re := range m.words {
		require.NotRegexp(t, re, s, "%s: real machine name (%s) in the shared prefix", label, name)
	}
	for _, in := range ins {
		for _, needle := range []string{in.home, in.host, in.branch, in.principal.ID, in.principal.String()} {
			require.NotContains(t, s, needle, label)
		}
		require.Regexp(t, `^[a-z]+$`, in.repo) // the word-boundary check below needs a plain word
		require.NotRegexp(t, `\b`+in.repo+`\b`, s, label)
	}
	require.NotContains(t, s, "agent/host-", label)
	for _, d := range datePattern.FindAllString(s, -1) {
		require.True(t, exampleDates[d], "%s: date %q in the shared prefix", label, d)
	}
	require.NotContains(t, s, time.Now().UTC().Format("2006-01-02"), label)
}

// writeToolNames are the tools a principal without Write is not shown.
func writeToolNames() []string {
	var out []string
	for _, r := range toolRegistrations(nil) {
		if r.write {
			out = append(out, r.tool.Name)
		}
	}
	sort.Strings(out)
	return out
}

func defNames(defs []llm.ToolDef) []string {
	out := make([]string, len(defs))
	for i, d := range defs {
		out[i] = d.Name
	}
	return out
}

// TestChatPrefix_Hooks1to3ByteIdenticalAcrossInstances is the fleet cache
// invariant: for one permission set, hooks 1-3 render to the same bytes on
// two servers that differ in home, agent branch, host, repo and principal,
// and on two runs of each. Across permission sets hooks 2-3 stay identical and
// hook 1 differs by exactly the write tools. The Anthropic bytes compared are
// the ones an httptest server received, and neither form holds the real
// machine's hostname, home, working directory, temp root or user.
func TestChatPrefix_Hooks1to3ByteIdenticalAcrossInstances(t *testing.T) {
	a := prefixInstance{home: t.TempDir(), branch: "agent/host-a-1111", host: "host-a", repo: "alpha",
		principal: auth.Principal{Kind: auth.KindBridge, ID: "uid:501", Via: auth.ViaSocket}, write: true}
	b := prefixInstance{home: t.TempDir(), branch: "agent/host-b-2222", host: "host-b", repo: "beta",
		principal: auth.Principal{Kind: auth.KindBridge, ID: "uid:1000", Via: auth.ViaSocket}, write: true}
	require.NotEqual(t, a.home, b.home)

	runs := append(runInstance(t, a), runInstance(t, b)...)
	require.Len(t, runs, 4)
	for i, r := range runs[1:] {
		require.Equal(t, string(runs[0].canonical), string(r.canonical), "canonical prefix of run %d", i+1)
		require.Equal(t, string(runs[0].anthropic), string(r.anthropic), "anthropic prefix of run %d", i+1)
	}

	// Non-vacuity: the bytes are the real prefix, not an empty agreement.
	ref := runs[0]
	require.Len(t, ref.tools, len(toolRegistrations(nil)))
	for _, bs := range [][]byte{ref.canonical, ref.anthropic} {
		require.Contains(t, string(bs), "knomit_query")
		require.Contains(t, string(bs), "## No repo bound yet")
		require.Contains(t, string(bs), "You are the chat assistant of a knomit knowledge base")
	}
	require.Equal(t, 3, strings.Count(string(ref.canonical), "\n"))
	requireNoMachineBytes(t, "canonical", ref.canonical, a, b)
	requireNoMachineBytes(t, "anthropic", ref.anthropic, a, b)
	// Hooks 2 and 3 hold no date at all, not even the example one.
	require.NotRegexp(t, yearPattern, hookLine(t, ref.canonical, llm.HookSystem))
	require.NotRegexp(t, yearPattern, hookLine(t, ref.canonical, llm.HookRepo))
	require.NotRegexp(t, yearPattern, ChatSystemPrompt)

	t.Run("permission sets", func(t *testing.T) {
		ra := prefixInstance{home: t.TempDir(), branch: "agent/host-c-3333", host: "host-c", repo: "gamma",
			principal: auth.Principal{Kind: auth.KindBridge, ID: "uid:2002", Via: auth.ViaSocket}}
		rb := prefixInstance{home: t.TempDir(), branch: "agent/host-d-4444", host: "host-d", repo: "delta",
			principal: auth.Principal{Kind: auth.KindHost, ID: "reader-7", Via: auth.ViaToken}}
		reader := runInstance(t, ra)
		reader2 := runInstance(t, rb)

		// Within the read-only set, the whole prefix is byte-identical too.
		for _, r := range []prefixRun{reader[1], reader2[0], reader2[1]} {
			require.Equal(t, string(reader[0].canonical), string(r.canonical))
			require.Equal(t, string(reader[0].anthropic), string(r.anthropic))
		}
		requireNoMachineBytes(t, "reader canonical", reader[0].canonical, ra, rb)
		requireNoMachineBytes(t, "reader anthropic", reader[0].anthropic, ra, rb)

		// Hooks 2 and 3 are identical across permission sets.
		for _, h := range []llm.Hook{llm.HookSystem, llm.HookRepo} {
			require.Equal(t, hookLine(t, ref.canonical, h), hookLine(t, reader[0].canonical, h), "hook %d", h)
		}
		// Hook 1 differs by exactly the write tools.
		require.NotEqual(t, hookLine(t, ref.canonical, llm.HookTools), hookLine(t, reader[0].canonical, llm.HookTools))
		writer := map[string]llm.ToolDef{}
		for _, d := range ref.tools {
			writer[d.Name] = d
		}
		var missing []string
		for _, d := range reader[0].tools {
			require.Equal(t, writer[d.Name], d, "a tool both sets see is byte-identical: %s", d.Name)
			delete(writer, d.Name)
		}
		for n := range writer {
			missing = append(missing, n)
		}
		sort.Strings(missing)
		require.Equal(t, []string{"knomit_experiment", "knomit_hypothesize", "knomit_learn", "knomit_retract", "knomit_review", "knomit_update"}, missing)
		require.Equal(t, writeToolNames(), missing)

		// Hook 1 is the read-only principal's own tools/list, read here
		// independently of LoadPrefix.
		srv := NewServer("kb", nil, false, ra.grants())
		raw, errMsg := rpcMsg(t, srv, chatCtx(ra), "tools/list", map[string]any{})
		require.Empty(t, errMsg)
		var listed struct {
			Tools []struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				InputSchema json.RawMessage `json:"inputSchema"`
			} `json:"tools"`
		}
		require.NoError(t, json.Unmarshal(raw, &listed))
		var own []llm.ToolDef
		for _, lt := range listed.Tools {
			schema, err := llm.CanonicalJSON(lt.InputSchema)
			require.NoError(t, err)
			own = append(own, llm.ToolDef{Name: lt.Name, Description: lt.Description, Schema: schema})
		}
		sort.Slice(own, func(i, j int) bool { return own[i].Name < own[j].Name })
		require.Equal(t, own, reader[0].tools)
		require.Len(t, own, len(toolRegistrations(nil))-len(missing))
		require.NotContains(t, defNames(own), "knomit_learn")
	})
}

func TestAddPrefix_ItemsPerHook(t *testing.T) {
	b := llm.NewBuilder()
	tools := []llm.ToolDef{
		{Name: "knomit_repos", Schema: json.RawMessage(`{"type":"object"}`)},
		{Name: "knomit_bind", Schema: json.RawMessage(`{"type":"object"}`)},
	}
	require.NoError(t, AddPrefix(b, "INSTR", tools))
	r := b.Snapshot().Record()
	require.Equal(t, []string{"tool:knomit_bind", "tool:knomit_repos"}, r.Items[llm.HookTools])
	require.Equal(t, []string{"system"}, r.Items[llm.HookSystem])
	require.Equal(t, []string{"instructions"}, r.Items[llm.HookRepo])
	s := b.Snapshot()
	require.Equal(t, ChatSystemPrompt, s.Hooks[llm.HookSystem][0].Text)
	require.Equal(t, "INSTR", s.Hooks[llm.HookRepo][0].Text)
	// The caller's slice is not aliased by the builder's items.
	tools[0].Name = "mutated"
	require.Equal(t, "knomit_repos", b.Snapshot().Hooks[llm.HookTools][1].Tool.Name)
}

// LoadPrefix follows nextCursor: under a page size smaller than the tool set
// it still returns every tool the principal may see, in name order.
func TestLoadPrefix_PagesToolsList(t *testing.T) {
	for _, write := range []bool{true, false} {
		in := prefixInstance{home: t.TempDir(), branch: "agent/host-p-5555", host: "host-p", repo: "paged",
			principal: auth.Principal{Kind: auth.KindBridge, ID: "uid:77", Via: auth.ViaSocket}, write: write}
		mgr := startPrefixManager(t, in)
		load := func(opts ...mcpserver.ServerOption) []llm.ToolDef {
			c, err := client.NewInProcessClient(newServer(opts, "kb", mgr, false, in.grants()))
			require.NoError(t, err)
			t.Cleanup(func() { _ = c.Close() })
			_, tools, err := LoadPrefix(chatCtx(in), c, mcpgo.Implementation{Name: "knomit-chat", Version: "anthropic/claude-test-x"})
			require.NoError(t, err)
			return tools
		}
		whole := load()
		want := len(toolRegistrations(nil))
		if !write {
			want -= len(writeToolNames())
		}
		require.Len(t, whole, want, "write=%v", write)

		// The page size really splits the list: one raw tools/list returns
		// a short page and a cursor.
		paged := newServer([]mcpserver.ServerOption{mcpserver.WithPaginationLimit(4)}, "kb", mgr, false, in.grants())
		c, err := client.NewInProcessClient(paged)
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close() })
		require.NoError(t, c.Start(chatCtx(in)))
		var init mcpgo.InitializeRequest
		init.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
		_, err = c.Initialize(chatCtx(in), init)
		require.NoError(t, err)
		first, err := c.ListToolsByPage(chatCtx(in), mcpgo.ListToolsRequest{})
		require.NoError(t, err)
		require.Less(t, len(first.Tools), want)
		require.NotEmpty(t, first.NextCursor)

		require.Equal(t, whole, load(mcpserver.WithPaginationLimit(4)), "write=%v", write)
	}
}
