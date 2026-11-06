package mcp

import (
	"testing"

	"github.com/mark3labs/mcp-go/client"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/engine/llm"
	"knomit/internal/foundation/auth"
)

// ChatCoreTools is the named policy list: bind and query, nothing else. The
// test pins the list so a change of it is a change here, and checks both are
// tools the server registers.
//
// SABOTAGE: chat_prefix.go, add "knomit_learn" to ChatCoreTools -> fails.
func TestChatCoreTools_IsBindAndQuery(t *testing.T) {
	require.Equal(t, []string{"knomit_bind", "knomit_query"}, ChatCoreTools)
	defs, _ := registeredToolDefs(t)
	have := map[string]bool{}
	for _, d := range defs {
		have[d.Name] = true
	}
	for _, n := range ChatCoreTools {
		require.True(t, have[n], n)
	}
}

// The chat's window opens with the core tools loaded, pinned, and the catalogue
// still lists every tool; the other tools stay adaptive.
//
// SABOTAGE: chat_prefix.go NewChatToolWindow, drop `Core: ChatCoreTools` ->
// fails.
func TestChatToolWindow_OpensWithTheCoreSet(t *testing.T) {
	defs, _ := registeredToolDefs(t)
	w, err := NewChatToolWindow(defs, llm.NewLexicalRanker(defs), 0)
	require.NoError(t, err)
	require.Equal(t, ChatCoreTools, w.Loaded())
	require.Equal(t, ChatCoreTools, w.Core())
	require.False(t, w.Handles("knomit_bind"))
	require.False(t, w.Handles("knomit_query"))
	require.True(t, w.Handles("knomit_explain"), "a tool outside the core is still the window's until loaded")

	b := llm.NewBuilder()
	require.NoError(t, AddPrefix(b, "instructions", defs))
	sent := w.Project(b.Snapshot())
	full, stubs := splitTools(sent, defs)
	require.Equal(t, []string{llm.LoadToolsName, "knomit_bind", "knomit_query"}, full, "full definitions: the loader and the core")
	require.Len(t, stubs, len(defs)-len(ChatCoreTools), "every other tool is a stub")

	for _, d := range defs {
		require.Contains(t, sent.Hooks[llm.HookTools][0].Tool.Description, "- "+d.Name, "the catalogue lists every tool, core included")
	}
}

// A read-only principal is not shown every tool; a core tool it does not have
// is not loaded (the window ignores names outside the catalogue).
func TestChatToolWindow_CoreToolTheCatalogueLacksIsNotLoaded(t *testing.T) {
	defs, _ := registeredToolDefs(t)
	var without []llm.ToolDef
	for _, d := range defs {
		if d.Name != "knomit_query" {
			without = append(without, d)
		}
	}
	w, err := NewChatToolWindow(without, nil, 0)
	require.NoError(t, err)
	require.Equal(t, []string{"knomit_bind"}, w.Loaded())
}

// The opening of a chat on a 32K model still fits with the core set, measured
// on the real tools, system prompt and server instructions of a writer: the
// request (loader with catalogue, core tools, hooks 2 and 3, the session note
// and a turn) must be under U = 24576 (32768 less the 8192 reply reserve), and
// the preload budget P = U/8 must still leave room to be spent. The measured
// size is logged.
//
// SABOTAGE: chat_prefix.go, add every tool name to ChatCoreTools -> the bound
// assertion fails.
func TestChatToolWindow_OpeningWithTheCoreSetFitsA32KModel(t *testing.T) {
	in := prefixInstance{home: t.TempDir(), branch: "agent/host-a-1111", host: "host-a", repo: "alpha",
		principal: auth.Principal{Kind: auth.KindBridge, ID: "uid:501", Via: auth.ViaSocket}, write: true}
	mgr := startPrefixManager(t, in)
	srv := NewServer("kb", mgr, false, in.grants())
	c, err := client.NewInProcessClient(srv)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	instr, tools, err := LoadPrefix(chatCtx(in), c, mcpgo.Implementation{Name: "knomit-chat", Version: "ollama/test"})
	require.NoError(t, err)

	const window = 32768
	caps := llm.Caps{ContextWindow: window}
	usable := window - llm.ReplyReserve(caps)
	require.Equal(t, 24576, usable)
	p := llm.PreloadBudget(usable)

	w, err := NewChatToolWindow(tools, llm.NewLexicalRanker(tools), p)
	require.NoError(t, err)
	b := llm.NewBuilder()
	require.NoError(t, AddPrefix(b, instr, tools))
	note := llm.Entry{ID: "session-context", Role: llm.RoleUser, Blocks: []llm.Block{{Kind: llm.BlockText,
		Text: "Session context: this chat is chat-00000000000000000000000000000000. Today's date (UTC) is 2026-11-05."}}}
	require.NoError(t, b.Add(llm.Item{ID: "session-context", Hook: llm.HookConversation, Entry: &note}))
	require.NoError(t, b.Add(llm.Item{ID: "turn", Hook: llm.HookTurn, Text: "what do we know about how agents recover from tool errors?"}))

	rec := w.Project(b.Snapshot()).Record()
	total, coreTokens := 0, 0
	for h := llm.HookTools; h <= llm.HookTurn; h++ {
		total += rec.Tokens[h]
	}
	_, coreTokens = w.Observe(b.Snapshot())
	t.Logf("opening: %d tokens of U=%d (%.1f%%); tools hook %d (core %d), system %d, instructions %d; P=%d",
		total, usable, 100*float64(total)/float64(usable), rec.Tokens[llm.HookTools], coreTokens,
		rec.Tokens[llm.HookSystem], rec.Tokens[llm.HookRepo], p)
	require.Less(t, total, usable/2, "the opening, core set included, uses under half of U: the rest is for the conversation")
	require.Less(t, coreTokens, p, "the core set is cheaper than the preload budget it replaces for these two tools")
}

// splitTools sorts a projection's hook-1 tools into those sent whole (the
// loader, and the tools whose definition is the builder's own) and the stubs.
func splitTools(s llm.Snapshot, defs []llm.ToolDef) (full, stubs []string) {
	whole := map[string]string{}
	for _, d := range defs {
		whole[d.Name] = string(d.Schema)
	}
	for _, it := range s.Hooks[llm.HookTools] {
		if sch, ok := whole[it.Tool.Name]; ok && sch != string(it.Tool.Schema) {
			stubs = append(stubs, it.Tool.Name)
			continue
		}
		full = append(full, it.Tool.Name)
	}
	return full, stubs
}
