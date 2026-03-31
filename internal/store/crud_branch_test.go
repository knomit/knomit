package store

import (
	"context"
	"testing"
)

func TestLastCommit_BranchScoped(t *testing.T) {
	ctx := context.Background()
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	// Both branches start empty.
	for _, branch := range []string{"main", "machine/dev"} {
		hash, err := idx.GetLastCommit(ctx, branch)
		if err != nil {
			t.Fatal(err)
		}
		if hash != "" {
			t.Fatalf("expected empty hash for %q, got %q", branch, hash)
		}
	}

	// Set last_commit on main.
	if err := idx.SetLastCommit(ctx, "main", "aaa111"); err != nil {
		t.Fatal(err)
	}

	// main should return the value; machine/dev should still be empty.
	hash, err := idx.GetLastCommit(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if hash != "aaa111" {
		t.Fatalf("expected 'aaa111' for main, got %q", hash)
	}

	hash, err = idx.GetLastCommit(ctx, "machine/dev")
	if err != nil {
		t.Fatal(err)
	}
	if hash != "" {
		t.Fatalf("expected empty for machine/dev, got %q", hash)
	}

	// Set last_commit on machine/dev independently.
	if err := idx.SetLastCommit(ctx, "machine/dev", "bbb222"); err != nil {
		t.Fatal(err)
	}

	hash, err = idx.GetLastCommit(ctx, "machine/dev")
	if err != nil {
		t.Fatal(err)
	}
	if hash != "bbb222" {
		t.Fatalf("expected 'bbb222' for machine/dev, got %q", hash)
	}

	// main is unchanged.
	hash, err = idx.GetLastCommit(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if hash != "aaa111" {
		t.Fatalf("expected main still 'aaa111', got %q", hash)
	}

	// Overwrite main.
	if err := idx.SetLastCommit(ctx, "main", "ccc333"); err != nil {
		t.Fatal(err)
	}
	hash, err = idx.GetLastCommit(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if hash != "ccc333" {
		t.Fatalf("expected 'ccc333' for main, got %q", hash)
	}

	// machine/dev still independent.
	hash, err = idx.GetLastCommit(ctx, "machine/dev")
	if err != nil {
		t.Fatal(err)
	}
	if hash != "bbb222" {
		t.Fatalf("expected machine/dev still 'bbb222', got %q", hash)
	}
}
