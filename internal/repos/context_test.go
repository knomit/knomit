package repos_test

import (
	"context"
	"testing"

	"knomit/internal/repos"
)

// TestRepoFromContextOpt_MissingRepo verifies the non-panicking variant
// returns (nil, false) when no repo is in context.
func TestRepoFromContextOpt_MissingRepo(t *testing.T) {
	ri, ok := repos.RepoFromContextOpt(context.Background())
	if ok {
		t.Error("expected ok=false for empty context")
	}
	if ri != nil {
		t.Error("expected nil ri for empty context")
	}
}

// TestRepoFromContextOpt_PresentRepo verifies the non-panicking variant
// returns (ri, true) when a repo is in context.
func TestRepoFromContextOpt_PresentRepo(t *testing.T) {
	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:        "example",
		AgentBranch: "agent/test",
	})
	ctx := repos.WithRepoInstance(context.Background(), ri)
	got, ok := repos.RepoFromContextOpt(ctx)
	if !ok {
		t.Error("expected ok=true")
	}
	if got != ri {
		t.Errorf("got different instance: %p vs %p", got, ri)
	}
}
