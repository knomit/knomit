package mcp

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/engine/llm"
	"knomit/internal/foundation/auth"
)

// registeredToolDefs are the server's own tools as the chat gets them from
// tools/list, in registration order, with the registration's write flag.
func registeredToolDefs(t *testing.T) (defs []llm.ToolDef, writes map[string]bool) {
	t.Helper()
	writes = map[string]bool{}
	for _, r := range toolRegistrations(nil) {
		d, err := toolDef(r.tool)
		require.NoError(t, err)
		defs = append(defs, d)
		writes[d.Name] = r.write
	}
	require.NotEmpty(t, defs)
	return defs, writes
}

// approvedPurposes are the user-approved catalogue lines (design C3). They are
// pinned here on purpose: a change of approved wording must be a change here.
var approvedPurposes = map[string]string{
	"knomit_repos":       "list the repos and lenses you can bind to",
	"knomit_bind":        "bind to one repo or lens and get the binding handle every other call needs",
	"knomit_query":       "search facts by text, entities, domain, path or confidence",
	"knomit_explain":     "read one fact in full, with its history and provenance",
	"knomit_materialize": "read a fact together with the facts it rests on, as one document",
	"knomit_changes":     "list facts added, changed or removed under a folder since a commit",
	"knomit_skill":       "list or read this repo's own how-to skills",
	"knomit_learn":       "write a new fact (the person approves first)",
	"knomit_update":      "change an existing fact (the person approves first)",
	"knomit_retract":     "remove a fact (the person approves first)",
	"knomit_review":      "run the knowledge-base maintenance review",
	"knomit_hypothesize": "generate predictions from existing facts; only when the person asks",
	"knomit_experiment":  "open, sync, commit or roll back an experiment branch",
}

// A new tool without a purpose line fails here, and so does a purpose line for
// a tool that no longer exists.
//
// SABOTAGE: delete one row of toolPurposes -> the first loop fails naming it.
func TestCatalogue_EveryRegisteredToolHasAPurpose(t *testing.T) {
	defs, _ := registeredToolDefs(t)
	registered := map[string]bool{}
	for _, d := range defs {
		registered[d.Name] = true
		require.NotEmpty(t, toolPurposes[d.Name], "%s has no purpose line in toolPurposes", d.Name)
	}
	for name := range toolPurposes {
		require.True(t, registered[name], "toolPurposes names %s, which no registration has", name)
	}
}

func TestCatalogue_PurposesAreTheApprovedLines(t *testing.T) {
	require.Equal(t, approvedPurposes, toolPurposes)
	defs, _ := registeredToolDefs(t)
	got := StubPurposes(defs)
	for name, purpose := range approvedPurposes {
		require.Equal(t, purpose, got[name], "the stub of %s carries the approved line", name)
	}
}

func TestCatalogue_LinesAreOnePerToolSortedByName(t *testing.T) {
	defs, _ := registeredToolDefs(t)
	cat, err := BuildNameCatalogue(defs, 0)
	require.NoError(t, err)
	lines := strings.Split(cat, "\n")
	require.Len(t, lines, len(defs))
	names := make([]string, len(defs))
	for i, d := range defs {
		names[i] = d.Name
	}
	sort.Strings(names)
	for i, n := range names {
		require.Equal(t, "- "+n, lines[i])
	}
}

// The catalogue is built from the tools the principal is shown: a read-only
// principal's holds none of the write tools, a writer's holds them.
//
// SABOTAGE: build the catalogue from toolPurposes instead of the tools passed
// in -> the read-only catalogue lists the write tools and this fails.
func TestCatalogue_ReadOnlyPrincipalSeesNoWriteTool(t *testing.T) {
	m, _, _ := bindFixture(t)
	key := chatPrincipal.String()

	open := func(id string, grants auth.StaticGrants) []llm.ToolDef {
		s, _, tools, err := OpenInProcessSession(context.Background(), NewServer("kb", m, false, grants), m, chatOpts(id))
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close(context.Background()) })
		return tools
	}
	roTools := open(chatIDA, auth.StaticGrants{key: {auth.Read: {}}})
	rwTools := open(chatIDB, auth.StaticGrants{key: {auth.Read: {}, auth.Write: {}}})

	roCat, err := BuildNameCatalogue(roTools, 0)
	require.NoError(t, err)
	rwCat, err := BuildNameCatalogue(rwTools, 0)
	require.NoError(t, err)

	require.Len(t, strings.Split(roCat, "\n"), len(roTools))
	require.Len(t, strings.Split(rwCat, "\n"), len(rwTools))
	require.Less(t, len(roTools), len(rwTools), "fixture: the writer sees tools the reader does not")
	roNames := map[string]bool{}
	for _, d := range roTools {
		roNames[d.Name] = true
	}
	var writeOnly []string
	for _, d := range rwTools {
		if !roNames[d.Name] {
			writeOnly = append(writeOnly, d.Name)
		}
	}
	require.Contains(t, writeOnly, "knomit_learn", "fixture: the tools only a writer is shown")
	for _, name := range writeOnly {
		require.NotContains(t, strings.Split(roCat, "\n"), "- "+name, "the read-only catalogue lists %s", name)
		require.Contains(t, strings.Split(rwCat, "\n"), "- "+name)
	}
}

// A tool the table does not know (a script tool, one added later) has as its
// stub purpose the first sentence of its description, cut at the sentence.
//
// SABOTAGE: return the whole description -> this fails.
func TestCatalogue_DerivedPurposeCutsAtASentence(t *testing.T) {
	tool := llm.ToolDef{
		Name:        "script_deploy",
		Description: "Deploy the staging build to the review host. It restarts the service. Afterwards it posts the result.",
	}
	got := StubPurposes([]llm.ToolDef{tool, {Name: "plain", Description: "  just a phrase  "}})
	require.Equal(t, "Deploy the staging build to the review host.", got["script_deploy"])
	// No sentence end: the whole (short) description, trimmed.
	require.Equal(t, "just a phrase", got["plain"])
}

// A derived purpose is cut to the limit, at a rune boundary.
func TestCatalogue_DerivedPurposeIsCutToTheLimit(t *testing.T) {
	long := strings.Repeat("alpha beta ", 40) + "ends here."
	require.LessOrEqual(t, len(derivedPurpose(long, 60)), 60)
	require.Greater(t, len(derivedPurpose(long, 600)), len(derivedPurpose(long, 60)))
	require.Equal(t, "é", derivedPurpose("éé", 3), "a cut never splits a rune")
}

// With a budget below the names the chat refuses to open (the opening-too-big
// family); at the names exactly it opens.
//
// SABOTAGE: ignore the budget in BuildNameCatalogue -> the error case fails.
func TestCatalogue_NamesBudgetRefusesBelowTheNames(t *testing.T) {
	defs, _ := registeredToolDefs(t)
	namesOnly, err := BuildNameCatalogue(defs, 0)
	require.NoError(t, err)
	namesTokens := len(namesOnly) / 4

	cat, err := BuildNameCatalogue(defs, namesTokens)
	require.NoError(t, err)
	require.Equal(t, namesOnly, cat)

	_, err = BuildNameCatalogue(defs, namesTokens-1)
	var big *CatalogueTooBigError
	require.True(t, errors.As(err, &big), "got %v", err)
	require.Equal(t, namesTokens, big.Need)
	require.Equal(t, namesTokens-1, big.Budget)
}

// NewChatToolWindow gives the window the loader whose description carries the
// catalogue, the tool names, mcp.Replayable and the ranker.
func TestChatToolWindow_CarriesCatalogueNamesReplayAndRanker(t *testing.T) {
	defs, _ := registeredToolDefs(t)
	ranker := llm.NewLexicalRanker(defs)
	w, err := NewChatToolWindow(defs, ranker, 0)
	require.NoError(t, err)
	require.True(t, w.Handles(llm.LoadToolsName))
	require.True(t, w.Handles("knomit_explain"), "a catalogue tool that is not loaded is the window's")
	require.False(t, w.Handles("not_a_tool"))

	// The projection sends the loader first; its description is the approved
	// text followed by the catalogue.
	b := llm.NewBuilder()
	require.NoError(t, AddPrefix(b, "instructions", defs))
	sent := w.Project(b.Snapshot())
	full, stubs := splitTools(sent, defs)
	require.Len(t, full, 1+len(ChatCoreTools), "the loader, then the core tools, whole")
	require.Len(t, stubs, len(defs)-len(ChatCoreTools))
	loader := sent.Hooks[llm.HookTools][0].Tool
	require.Equal(t, llm.LoadToolsName, loader.Name)
	cat, err := BuildNameCatalogue(defs, 0)
	require.NoError(t, err)
	require.Equal(t, llm.LoaderDescription+"\n"+cat, loader.Description)

	// Replay: a stub of a knomit_query result offers the re-call, one of a
	// knomit_learn result does not.
	big := strings.Repeat("r", 4000)
	conv := func(id, tool string) llm.Item {
		return llm.Item{ID: "c-" + id, Hook: llm.HookConversation, Entry: &llm.Entry{
			ID: "c-" + id, Role: llm.RoleAssistant, Blocks: []llm.Block{
				{Kind: llm.BlockToolUse, ToolUse: &llm.ToolUse{ID: id, Name: tool, Args: []byte(`{"binding":"h"}`)}},
			}}}
	}
	res := func(id string) llm.Item {
		return llm.Item{ID: "r-" + id, Hook: llm.HookConversation, Entry: &llm.Entry{
			ID: "r-" + id, Role: llm.RoleUser, Blocks: []llm.Block{
				{Kind: llm.BlockToolResult, ToolResult: &llm.ToolResult{ToolUseID: id, Content: big}},
			}}}
	}
	for _, it := range []llm.Item{conv("q1", "knomit_query"), res("q1"), conv("l1", "knomit_learn"), res("l1")} {
		require.NoError(t, b.Add(it))
	}
	stubbed := w.StubEnded(w.Project(b.Snapshot()), 24576)
	got := map[string]string{}
	for _, it := range stubbed.Hooks[llm.HookConversation] {
		for _, bl := range it.Entry.Blocks {
			if bl.ToolResult != nil {
				got[bl.ToolResult.ToolUseID] = bl.ToolResult.Content
			}
		}
	}
	require.True(t, strings.HasPrefix(got["q1"], fmt.Sprintf(llm.StubReplayable, "knomit_query", 1000, "knomit_query")), got["q1"])
	require.True(t, strings.HasPrefix(got["l1"], fmt.Sprintf(llm.StubNotReplayable, "knomit_learn", 1000)), got["l1"])
}

// A server tool named like the loader refuses to open, with the approved
// sentence.
func TestChatToolWindow_LoaderNameCollisionIsRefused(t *testing.T) {
	defs := []llm.ToolDef{{Name: "knomit_query", Description: "Search."}, {Name: llm.LoadToolsName, Description: "Mine."}}
	_, err := NewChatToolWindow(defs, nil, 0)
	require.EqualError(t, err, fmt.Sprintf(llm.ReservedToolNameMessage, llm.LoadToolsName))
}

// The hook-2 bytes (ChatSystemPrompt) are the same whether the chat is adaptive
// or static: the catalogue lives in the loader's description, not in the
// system prompt.
//
// SABOTAGE: have NewChatToolWindow or AddPrefix append the catalogue to the
// system item -> the bytes differ and this fails.
func TestChatPrefix_HookTwoIsTheSameForAdaptiveAndStatic(t *testing.T) {
	defs, _ := registeredToolDefs(t)
	static := llm.NewBuilder()
	require.NoError(t, AddPrefix(static, "instructions", defs))
	w, err := NewChatToolWindow(defs, nil, 0)
	require.NoError(t, err)
	adaptive := w.Project(static.Snapshot())
	require.Equal(t, static.Snapshot().Hooks[llm.HookSystem], adaptive.Hooks[llm.HookSystem])
	require.Equal(t, static.Snapshot().Hooks[llm.HookRepo], adaptive.Hooks[llm.HookRepo])
	require.Equal(t, ChatSystemPrompt, adaptive.Hooks[llm.HookSystem][0].Text)
}
