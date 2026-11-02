package mcp

import "testing"

// knomit_query's `repo` argument shows the user-ruled text of
// knomit_materialize's search.repo (ruling S6), word for word, on both tools.
//
// Sabotage: reword either description (or give knomit_query its own string)
// and this fails naming the tool.
func TestQueryRepoDescriptionIsMaterializesSearchRepo(t *testing.T) {
	const ruled = "Lens endpoints only: search only these member repos, by name (the lens list's repo filter)."
	prop, ok := queryTool().InputSchema.Properties["repo"].(map[string]any)
	if !ok {
		t.Fatalf("knomit_query declares no repo argument: %v", queryTool().InputSchema.Properties)
	}
	if got := prop["description"]; got != ruled {
		t.Errorf("knomit_query repo description = %q, want %q", got, ruled)
	}
	if got := prop["type"]; got != "array" {
		t.Errorf("knomit_query repo type = %v, want array", got)
	}
	ms, _ := factsetSearchKeys["repo"].(map[string]any)
	if got := ms["description"]; got != ruled {
		t.Errorf("knomit_materialize search.repo description = %q, want %q", got, ruled)
	}
}
