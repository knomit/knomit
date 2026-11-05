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
	cat, err := BuildCatalogue(defs, 0)
	require.NoError(t, err)
	for name, purpose := range approvedPurposes {
		require.Contains(t, strings.Split(cat, "\n"), "- "+name+": "+purpose)
	}
}

func TestCatalogue_LinesAreOnePerToolSortedByName(t *testing.T) {
	defs, _ := registeredToolDefs(t)
	cat, err := BuildCatalogue(defs, 0)
	require.NoError(t, err)
	lines := strings.Split(cat, "\n")
	require.Len(t, lines, len(defs))
	names := make([]string, len(defs))
	for i, d := range defs {
		names[i] = d.Name
	}
	sort.Strings(names)
	for i, n := range names {
		require.True(t, strings.HasPrefix(lines[i], "- "+n+": "), "line %d is %q, want the line of %s", i, lines[i], n)
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

	roCat, err := BuildCatalogue(roTools, 0)
	require.NoError(t, err)
	rwCat, err := BuildCatalogue(rwTools, 0)
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
		require.NotContains(t, roCat, "- "+name+":", "the read-only catalogue lists %s", name)
		require.Contains(t, rwCat, "- "+name+":")
	}
}

// A tool the table does not know (a script tool, one added later) gets the
// first sentence of its description, cut at the sentence.
//
// SABOTAGE: return the whole description -> this fails.
func TestCatalogue_DerivedPurposeCutsAtASentence(t *testing.T) {
	tool := llm.ToolDef{
		Name:        "script_deploy",
		Description: "Deploy the staging build to the review host. It restarts the service. Afterwards it posts the result.",
	}
	cat, err := BuildCatalogue([]llm.ToolDef{tool}, 0)
	require.NoError(t, err)
	require.Equal(t, "- script_deploy: Deploy the staging build to the review host.", cat)

	// No sentence end: the whole (short) description, trimmed.
	cat, err = BuildCatalogue([]llm.ToolDef{{Name: "plain", Description: "  just a phrase  "}}, 0)
	require.NoError(t, err)
	require.Equal(t, "- plain: just a phrase", cat)
}

// The derived purpose's length is the catalogue budget's share per tool: two
// budgets give two different cuts of the same description.
func TestCatalogue_DerivedPurposeLengthComesFromTheBudget(t *testing.T) {
	long := strings.Repeat("alpha beta ", 40) + "ends here."
	tools := []llm.ToolDef{{Name: "script_a", Description: long}, {Name: "script_b", Description: long}}
	small, err := BuildCatalogue(tools, 60)
	require.NoError(t, err)
	large, err := BuildCatalogue(tools, 600)
	require.NoError(t, err)
	require.Less(t, len(small), len(large))
	require.LessOrEqual(t, len(small)/4, 60)
	require.LessOrEqual(t, len(large)/4, 600)
	require.True(t, strings.HasPrefix(large, "- script_a: alpha beta"))
}

// With a budget below the lines the catalogue is names only; below the names,
// the chat refuses to open (the opening-too-big family).
//
// SABOTAGE: skip the names-only step -> the middle case errors and this fails.
func TestCatalogue_DegradesToNamesThenRefuses(t *testing.T) {
	defs, _ := registeredToolDefs(t)
	full, err := BuildCatalogue(defs, 0)
	require.NoError(t, err)
	fullTokens := len(full) / 4

	names := make([]string, len(defs))
	for i, d := range defs {
		names[i] = d.Name
	}
	sort.Strings(names)
	var nameLines []string
	for _, n := range names {
		nameLines = append(nameLines, "- "+n)
	}
	namesOnly := strings.Join(nameLines, "\n")
	namesTokens := len(namesOnly) / 4
	require.Less(t, namesTokens, fullTokens, "fixture: purposes cost something")

	// Budget between the two: names only.
	cat, err := BuildCatalogue(defs, namesTokens+(fullTokens-namesTokens)/2)
	require.NoError(t, err)
	require.Equal(t, namesOnly, cat)

	// Budget exactly the full catalogue: the lines.
	cat, err = BuildCatalogue(defs, fullTokens)
	require.NoError(t, err)
	require.Equal(t, full, cat)

	// Budget below the names: the error, with both sizes.
	_, err = BuildCatalogue(defs, namesTokens-1)
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
	require.True(t, w.Handles("knomit_query"), "a catalogue tool that is not loaded is the window's")
	require.False(t, w.Handles("not_a_tool"))

	// The projection sends the loader first; its description is the approved
	// text followed by the catalogue.
	b := llm.NewBuilder()
	require.NoError(t, AddPrefix(b, "instructions", defs))
	sent := w.Project(b.Snapshot())
	require.Len(t, sent.Hooks[llm.HookTools], 1)
	loader := sent.Hooks[llm.HookTools][0].Tool
	require.Equal(t, llm.LoadToolsName, loader.Name)
	cat, err := BuildCatalogue(defs, 0)
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
