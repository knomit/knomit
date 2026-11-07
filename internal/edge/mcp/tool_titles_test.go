package mcp

import (
	"encoding/json"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/engine/llm"
)

// approvedTitles is the user's ruling (2026-11-06): the words a person reads
// for each tool, carried by the tool's own annotations.title.
var approvedTitles = map[string]string{
	"knomit_bind":        "Bind",
	"knomit_repos":       "List repos",
	"knomit_query":       "Query",
	"knomit_explain":     "Explain",
	"knomit_materialize": "Materialize",
	"knomit_changes":     "Changes",
	"knomit_skill":       "Skill",
	"knomit_learn":       "Learn",
	"knomit_update":      "Update",
	"knomit_retract":     "Retract",
	"knomit_review":      "Review",
	"knomit_hypothesize": "Hypothesize",
	"knomit_experiment":  "Experiment",
}

// allToolDefinitions is every tool definition the server can list: the
// registrations plus the URL-scoped knomit_bind.
func allToolDefinitions() []mcpgo.Tool {
	var out []mcpgo.Tool
	for _, r := range toolRegistrations(nil) {
		out = append(out, r.tool)
	}
	return append(out, scopedBindTool())
}

// Every tool has a non-empty title, and it is the approved one. A new tool
// without a title fails here.
//
// SABOTAGE: drop WithTitleAnnotation from any tool -> fails naming it.
func TestToolTitles_EveryToolHasItsApprovedTitle(t *testing.T) {
	for _, tool := range allToolDefinitions() {
		require.NotEmpty(t, tool.Annotations.Title, "%s has no annotations.title", tool.Name)
		if want, ok := approvedTitles[tool.Name]; ok {
			require.Equal(t, want, tool.Annotations.Title, tool.Name)
		}
	}
	require.Equal(t, "Load tools", llm.LoaderDef("").Title, "the chat's loader has its title too")
}

// A tool's main argument, when it marks one, names arguments its schema has,
// and every marked name is a string argument (the row shows one string).
func TestToolTitles_KeyArgumentsAreStringArgumentsOfTheSchema(t *testing.T) {
	marked := 0
	for _, tool := range allToolDefinitions() {
		keys := toolKeyArgs(tool)
		if len(keys) > 0 {
			marked++
		}
		for _, k := range keys {
			p, ok := tool.InputSchema.Properties[k].(map[string]any)
			require.True(t, ok, "%s marks %q, which is not one of its arguments", tool.Name, k)
			require.Equal(t, "string", p["type"], "%s marks %q, which is not a string", tool.Name, k)
		}
	}
	require.Greater(t, marked, 5, "the tools mark their main arguments")
}

// toolDef carries the title and the key arguments to the chat, over the wire
// the chat really reads (marshal, as tools/list sends it, then unmarshal).
//
// SABOTAGE: toolDef drops the title or the key -> fails.
func TestToolDef_CarriesTitleAndKeyFromTheWire(t *testing.T) {
	var tool mcpgo.Tool
	wire, err := json.Marshal(bindTool())
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(wire, &tool))
	def, err := toolDef(tool)
	require.NoError(t, err)
	require.Equal(t, "Bind", def.Title)
	require.Equal(t, []string{"repo", "lens"}, def.Key)
}

// The title and the key are not part of what the model is sent, so they do
// not change the cached tool prefix: a tool with them encodes into the
// canonical prefix byte for byte as the same tool without them.
//
// SABOTAGE: give ToolDef.Title a json name -> the bytes differ.
func TestToolDef_TitleAndKeyDoNotChangeThePrefixBytes(t *testing.T) {
	with := queryTool()
	without := queryTool()
	without.Annotations.Title = ""
	without.Meta = nil
	a, err := toolDef(with)
	require.NoError(t, err)
	b, err := toolDef(without)
	require.NoError(t, err)
	require.NotEmpty(t, a.Title)
	require.Empty(t, b.Title)

	prefix := func(d llm.ToolDef) []byte {
		bl := llm.NewBuilder()
		require.NoError(t, bl.Add(llm.Item{ID: "tool:" + d.Name, Hook: llm.HookTools, Tool: &d}))
		return bl.Snapshot().CanonicalPrefix(llm.HookTurn)
	}
	require.Equal(t, string(prefix(b)), string(prefix(a)))
	// The marker lives in _meta, never in the schema the model is sent.
	require.NotContains(t, string(a.Schema), MetaKeyArgs)
}
