package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func toolItem(name string) Item {
	return Item{ID: "tool:" + name, Hook: HookTools, Tool: &ToolDef{Name: name, Description: "d-" + name, Schema: json.RawMessage(`{"type":"object"}`)}}
}

func TestBuilder_ToolsSortedRegardlessOfInsertion(t *testing.T) {
	b := NewBuilder()
	for _, n := range []string{"knomit_repos", "knomit_bind", "knomit_query"} {
		require.NoError(t, b.Add(toolItem(n)))
	}
	var names []string
	for _, it := range b.Snapshot().Hooks[HookTools] {
		names = append(names, it.Tool.Name)
	}
	require.Equal(t, []string{"knomit_bind", "knomit_query", "knomit_repos"}, names)

	// And the canonical prefix does not depend on insertion order either.
	b2 := NewBuilder()
	for _, n := range []string{"knomit_query", "knomit_repos", "knomit_bind"} {
		require.NoError(t, b2.Add(toolItem(n)))
	}
	require.Equal(t, string(b.Snapshot().CanonicalPrefix(HookTools)), string(b2.Snapshot().CanonicalPrefix(HookTools)))
}

func TestBuilder_ReplaceKeepsPosition(t *testing.T) {
	b := NewBuilder()
	for i, txt := range []string{"one", "two", "three"} {
		require.NoError(t, b.Add(Item{ID: fmt.Sprintf("c%d", i), Hook: HookConversation, Text: txt}))
	}
	require.NoError(t, b.Replace("c1", Item{ID: "c1", Hook: HookConversation, Text: "TWO"}))
	got := b.Snapshot().Hooks[HookConversation]
	require.Equal(t, []string{"one", "TWO", "three"}, []string{got[0].Text, got[1].Text, got[2].Text})

	// Moving hooks and changing ids are refused, and leave the item alone.
	require.Error(t, b.Replace("c1", Item{ID: "c1", Hook: HookTurn, Text: "x"}))
	require.Error(t, b.Replace("c1", Item{ID: "c9", Hook: HookConversation, Text: "x"}))
	require.ErrorIs(t, b.Replace("nope", Item{ID: "nope", Hook: HookConversation}), ErrUnknownItem)
	require.Equal(t, "TWO", b.Snapshot().Hooks[HookConversation][1].Text)
}

func TestBuilder_RemoveThenAddSameIDFails(t *testing.T) {
	b := NewBuilder()
	it := Item{ID: "w1", Hook: HookWorkingSet, Text: "fact"}
	require.NoError(t, b.Add(it))
	require.True(t, b.Remove("w1"))
	require.False(t, b.Remove("w1"))
	require.Empty(t, b.Snapshot().Hooks[HookWorkingSet])
	require.NoError(t, b.Add(it))
	require.ErrorIs(t, b.Add(it), ErrDuplicateItem)
	// IDs are unique across hooks, not per hook.
	require.ErrorIs(t, b.Add(Item{ID: "w1", Hook: HookTurn, Text: "x"}), ErrDuplicateItem)
}

func TestCanonicalJSON_StableAcrossMapOrder(t *testing.T) {
	m := map[string]any{}
	for i := 0; i < 20; i++ {
		m[fmt.Sprintf("k%02d", 19-i)] = map[string]any{"description": fmt.Sprintf("<b>%d</b> & more", i), "n": i}
	}
	var first string
	for i := 0; i < 50; i++ {
		raw, err := json.Marshal(m) // a map: encoding/json already sorts, but the input to CanonicalJSON is any JSON
		require.NoError(t, err)
		// Shuffle the top-level key order in the INPUT bytes, so the test
		// does not lean on json.Marshal's own sorting.
		var parts []string
		for k, v := range m {
			vb, _ := json.Marshal(v)
			parts = append(parts, fmt.Sprintf("%q:%s", k, vb))
		}
		shuffled := json.RawMessage("{" + strings.Join(parts, ",") + "}")
		got, err := CanonicalJSON(shuffled)
		require.NoError(t, err)
		got2, err := CanonicalJSON(raw)
		require.NoError(t, err)
		require.Equal(t, string(got), string(got2))
		if i == 0 {
			first = string(got)
			continue
		}
		require.Equal(t, first, string(got))
	}
	require.Contains(t, first, "<b>0</b> & more")
	require.NotContains(t, first, "\\"+"u003c")
	require.True(t, strings.HasPrefix(first, `{"k00":`), first)
	// Numbers keep their literal spelling.
	n, err := CanonicalJSON(json.RawMessage(`{"a":1.50,"b":10000000000000000001}`))
	require.NoError(t, err)
	require.Equal(t, `{"a":1.50,"b":10000000000000000001}`, string(n))
}

func TestBuilder_FrameHookNotImplemented(t *testing.T) {
	b := NewBuilder()
	err := b.AddFrame(FrameSpec{Root: "alpha", Anchor: "abc123"})
	require.True(t, errors.Is(err, ErrFrameNotImplemented), "%v", err)
	require.Empty(t, b.Snapshot().Hooks[HookFrame])
	require.NoError(t, b.Add(Item{Hook: HookFrame, ID: "f1", Text: "x"}))
	require.Equal(t, "f1", b.Snapshot().Hooks[HookFrame][0].ID)
}

func TestSnapshot_RecordNamesItemsPerHook(t *testing.T) {
	b := NewBuilder()
	sys := "You are knomit's chat assistant, answering from the knowledge base."
	require.NoError(t, b.Add(Item{ID: "system", Hook: HookSystem, Text: sys}))
	require.NoError(t, b.Add(Item{ID: "instructions", Hook: HookRepo, Text: "ontology topics"}))
	require.NoError(t, b.Add(toolItem("knomit_query")))
	r := b.Snapshot().Record()
	require.Equal(t, []string{"instructions"}, r.Items[HookRepo])
	require.Equal(t, []string{"system"}, r.Items[HookSystem])
	require.Equal(t, []string{"tool:knomit_query"}, r.Items[HookTools])
	require.Equal(t, len(sys)/4, r.Tokens[HookSystem])
	require.Equal(t, len("ontology topics")/4, r.Tokens[HookRepo])
	require.NotEqual(t, r.Tokens[HookSystem], r.Tokens[HookRepo]) // the fixture discriminates
	require.True(t, r.Estimated)
	require.Empty(t, r.Items[HookTurn])
}

func TestSnapshot_CanonicalPrefixStopsAtHook(t *testing.T) {
	b := NewBuilder()
	require.NoError(t, b.Add(Item{ID: "system", Hook: HookSystem, Text: "S"}))
	require.NoError(t, b.Add(Item{ID: "instructions", Hook: HookRepo, Text: "R"}))
	require.NoError(t, b.Add(Item{ID: "u1", Hook: HookTurn, Text: "volatile <question>"}))
	s := b.Snapshot()
	p3 := string(s.CanonicalPrefix(HookRepo))
	require.Equal(t, 3, strings.Count(p3, "\n"))
	require.Contains(t, p3, `"text":"R"`)
	require.NotContains(t, p3, "volatile")
	require.Contains(t, string(s.CanonicalPrefix(HookTurn)), "volatile <question>")
}
