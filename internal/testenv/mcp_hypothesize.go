package testenv

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"knomit/internal/mcp"
	"knomit/internal/repos"
)

// Hypothesize calls the MCP knomit_hypothesize handler against this
// branch with the given args (e.g., "session_id", "response"). Pass nil
// or empty map to start a fresh session. Returns a typed view for
// assertions on the returned work item.
//
// The branch must be the storyboard's agent branch ("agent/test"); the
// handler internally reads ri.AgentBranch() and operates on that branch
// regardless of which BranchHandle the helper is called on. We keep the
// receiver on BranchHandle for ergonomics but assert this invariant.
func (b *BranchHandle) Hypothesize(args map[string]string) *HypothesizeView {
	t := b.repo.sb.t
	t.Helper()

	if want := b.repo.ri.AgentBranch(); b.name != want {
		t.Fatalf("Hypothesize: branch %q is not the agent branch %q — call Hypothesize on Branch(%q)",
			b.name, want, want)
	}

	argMap := map[string]any{}
	for k, v := range args {
		argMap[k] = v
	}
	var req mcpgo.CallToolRequest
	req.Params.Arguments = argMap

	ctx := repos.WithRepoInstance(context.Background(), b.repo.ri)
	result, err := mcp.HypothesizeHandler()(ctx, req)
	if err != nil {
		t.Fatalf("Hypothesize: handler error: %v", err)
	}
	if result.IsError {
		var msg string
		for _, c := range result.Content {
			if tc, ok := c.(mcpgo.TextContent); ok {
				msg = tc.Text
				break
			}
		}
		t.Fatalf("Hypothesize: handler returned IsError=true: %s", msg)
	}

	var text string
	found := false
	for _, c := range result.Content {
		if tc, ok := c.(mcpgo.TextContent); ok {
			text = tc.Text
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("Hypothesize: result has no TextContent: %+v", result.Content)
	}

	var parsed mcp.HypothesizeResult
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		t.Fatalf("Hypothesize: unmarshal result %q: %v", text, err)
	}
	return &HypothesizeView{t: t, branch: b, result: &parsed}
}

// HypothesizeView wraps the result of a knomit_hypothesize call with
// assertion helpers. The session is implicit: Continue re-invokes the
// handler with the same session_id.
type HypothesizeView struct {
	t      *testing.T
	branch *BranchHandle
	result *mcp.HypothesizeResult
}

// Result returns the underlying handler response (escape hatch).
func (v *HypothesizeView) Result() *mcp.HypothesizeResult { return v.result }

// SessionID returns the session ID from the response.
func (v *HypothesizeView) SessionID() string { return v.result.SessionID }

// IsDone reports whether the session has completed.
func (v *HypothesizeView) IsDone() bool { return v.result.Done }

// MustBeDone fails the test if the session is not complete.
func (v *HypothesizeView) MustBeDone() *HypothesizeView {
	v.t.Helper()
	if !v.result.Done {
		v.t.Fatalf("Hypothesize: expected Done=true, got false (item=%+v)", v.result.Item)
	}
	return v
}

// MustNotBeDone fails the test if the session is already complete.
func (v *HypothesizeView) MustNotBeDone() *HypothesizeView {
	v.t.Helper()
	if v.result.Done {
		v.t.Fatalf("Hypothesize: expected Done=false, got true")
	}
	return v
}

// Item returns the current work item, failing the test if nil.
func (v *HypothesizeView) Item() *mcp.HypothesizeItem {
	v.t.Helper()
	if v.result.Item == nil {
		v.t.Fatalf("Hypothesize: expected Item, got nil (Done=%v)", v.result.Done)
	}
	return v.result.Item
}

// Instructions returns the work item's Instructions field.
func (v *HypothesizeView) Instructions() string {
	return v.Item().Instructions
}

// MustHaveInstructionsContaining fails the test if the work item's
// Instructions field does not contain the given substring.
func (v *HypothesizeView) MustHaveInstructionsContaining(substr string) *HypothesizeView {
	v.t.Helper()
	got := v.Instructions()
	if !strings.Contains(got, substr) {
		v.t.Fatalf("Hypothesize: Instructions missing %q\n--- full instructions ---\n%s\n--- end ---", substr, got)
	}
	return v
}

// MustNotHaveInstructionsContaining fails the test if the work item's
// Instructions field contains the given substring.
func (v *HypothesizeView) MustNotHaveInstructionsContaining(substr string) *HypothesizeView {
	v.t.Helper()
	got := v.Instructions()
	if strings.Contains(got, substr) {
		v.t.Fatalf("Hypothesize: Instructions unexpectedly contains %q\n--- full instructions ---\n%s\n--- end ---", substr, got)
	}
	return v
}

// Continue re-invokes Hypothesize on the same branch with the current
// session_id and the given response. Returns the next view.
func (v *HypothesizeView) Continue(response string) *HypothesizeView {
	v.t.Helper()
	return v.branch.Hypothesize(map[string]string{
		"session_id": v.SessionID(),
		"response":   response,
	})
}
