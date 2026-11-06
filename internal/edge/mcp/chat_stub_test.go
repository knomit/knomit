package mcp

import (
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/mark3labs/mcp-go/client"

	"knomit/internal/engine/llm"
	"knomit/internal/foundation/auth"
)

// writerPrefix loads the real prefix of a writer: its instructions and tools.
func writerPrefix(t *testing.T) (string, []llm.ToolDef) {
	t.Helper()
	in := prefixInstance{home: t.TempDir(), branch: "agent/host-a-1111", host: "host-a", repo: "alpha",
		principal: auth.Principal{Kind: auth.KindBridge, ID: "uid:501", Via: auth.ViaSocket}, write: true}
	mgr := startPrefixManager(t, in)
	srv := NewServer("kb", mgr, false, in.grants())
	c, err := client.NewInProcessClient(srv)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	instr, tools, err := LoadPrefix(chatCtx(in), c, mcpgo.Implementation{Name: "knomit-chat", Version: "ollama/test"})
	require.NoError(t, err)
	return instr, tools
}

// openingTokens is the estimate of a chat's first request: the projection of
// the prefix through the window plus the session note and a short message.
func openingTokens(t *testing.T, w *llm.ToolWindow, instr string, tools []llm.ToolDef) (total int, rec llm.RequestRecord) {
	t.Helper()
	rec = w.Project(openingSnap(t, instr, tools)).Record()
	for h := llm.HookTools; h <= llm.HookTurn; h++ {
		total += rec.Tokens[h]
	}
	return total, rec
}

// openingSnap is the builder's snapshot of a chat's first request.
func openingSnap(t *testing.T, instr string, tools []llm.ToolDef) llm.Snapshot {
	t.Helper()
	b := llm.NewBuilder()
	require.NoError(t, AddPrefix(b, instr, tools))
	note := llm.Entry{ID: "session-context", Role: llm.RoleUser, Blocks: []llm.Block{{Kind: llm.BlockText,
		Text: "Session context: this chat is chat-00000000000000000000000000000000. Today's date (UTC) is 2026-11-05."}}}
	require.NoError(t, b.Add(llm.Item{ID: "session-context", Hook: llm.HookConversation, Entry: &note}))
	require.NoError(t, b.Add(llm.Item{ID: "turn", Hook: llm.HookTurn, Text: "what do we know about how agents recover from tool errors?"}))
	return b.Snapshot()
}

// A stub carries the approved purpose line of its tool and nothing else the
// model has to read: StubPurposes is toolPurposes for every registered tool,
// and a derived first sentence (capped) for one with no row.
//
// SABOTAGE: StubPurposes, return t.Description -> the first assertion fails.
func TestStubPurposes_AreTheApprovedLinesOrADerivedSentence(t *testing.T) {
	defs, _ := registeredToolDefs(t)
	got := StubPurposes(defs)
	for _, d := range defs {
		require.Equal(t, toolPurposes[d.Name], got[d.Name], d.Name)
	}
	long := llm.ToolDef{Name: "script_x", Description: "First sentence here. " + strings.Repeat("More text. ", 100)}
	p := StubPurposes([]llm.ToolDef{long})["script_x"]
	require.Equal(t, "First sentence here.", p)
	cut := StubPurposes([]llm.ToolDef{{Name: "s", Description: strings.Repeat("a", 500)}})["s"]
	require.Len(t, cut, stubPurposeLimit)
}

// In an adaptive chat the loader's catalogue is the names alone: the purposes
// are in the stubs. Trimming it saves tokens and loses no name the loader
// takes.
//
// SABOTAGE: chat_prefix.go NewChatToolWindow, build the loader's catalogue
// with the purposes appended -> the Equal fails.
func TestChatToolWindow_LoaderCatalogueIsNamesAndStubsCarryThePurposes(t *testing.T) {
	_, defs := writerPrefix(t)
	w, err := NewChatToolWindow(defs, nil, 0)
	require.NoError(t, err)
	b := llm.NewBuilder()
	require.NoError(t, AddPrefix(b, "i", defs))
	sent := w.Project(b.Snapshot())
	names, err := BuildNameCatalogue(defs, 0)
	require.NoError(t, err)
	loader := sent.Hooks[llm.HookTools][0].Tool
	require.Equal(t, llm.LoaderDescription+"\n"+names, loader.Description)
	_, stubs := splitTools(sent, defs)
	for _, it := range sent.Hooks[llm.HookTools][1:] {
		for _, s := range stubs {
			if it.Tool.Name == s {
				require.Equal(t, toolPurposes[s], it.Tool.Description, "the stub of %s carries its approved purpose", s)
			}
		}
	}
}

// The opening of an adaptive chat counts the core definitions AND the stubs.
// Measured on the real tools of a writer: the stubs of the 11 non-core tools
// cost under a tenth of the usable room at 32K, and the whole opening stays
// under half of it. The sizes are logged. At 16K the same opening is
// checked against that window's smaller room.
//
// SABOTAGE: llm/toolwindow.go Project, send every unloaded tool whole instead
// of as a stub -> the stub bound fails.
func TestChatToolWindow_StubsAreCheapAndTheOpeningFitsAt32KAnd16K(t *testing.T) {
	instr, defs := writerPrefix(t)
	w, err := NewChatToolWindow(defs, nil, 0)
	require.NoError(t, err)
	total, rec := openingTokens(t, w, instr, defs)
	_, coreTokens := w.Observe(openingSnap(t, instr, defs))
	stub := w.StubTokens()
	require.Positive(t, stub)
	require.Len(t, defs, 13)
	t.Logf("stubs for %d non-core tools: %d tokens; core %d; tools hook %d; opening %d; system %d; instructions %d",
		len(defs)-len(ChatCoreTools), stub, coreTokens, rec.Tokens[llm.HookTools], total, rec.Tokens[llm.HookSystem], rec.Tokens[llm.HookRepo])
	for _, window := range []int{32768, 16384} {
		usable := window - llm.ReplyReserve(llm.Caps{ContextWindow: window})
		t.Logf("window %d: usable %d, opening %d (%.1f%%), P %d", window, usable, total, 100*float64(total)/float64(usable), llm.PreloadBudget(usable))
	}
	// The chat refuses an opening that leaves less than P free (adaptive.go
	// openWindow). 16K with the default reserve (a quarter) opens; 16K with an
	// explicit MaxOutput of 8192 (review N1: U = 8192) is refused; 32K opens.
	fits := func(window, maxOut int) bool {
		usable := window - llm.ReplyReserve(llm.Caps{ContextWindow: window, MaxOutput: maxOut})
		return total <= usable-llm.PreloadBudget(usable)
	}
	require.True(t, fits(32768, 0), "32K opens")
	require.True(t, fits(16384, 0), "16K with the default reserve opens")
	require.False(t, fits(16384, 8192), "16K with MaxOutput 8192 does not: the opening alone is more than its usable room")
	require.Less(t, stub, 24576/10, "the stubs of all non-core tools cost under a tenth of U at 32K")
	require.Less(t, total, 24576/2, "the opening, core and stubs included, is under half of U at 32K")
}
