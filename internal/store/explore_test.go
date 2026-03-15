package store_test

import (
	"testing"
)

func TestCreateExploreSession(t *testing.T) {
	idx := newTestIndex(t)

	s, err := idx.CreateExploreSession("machine/test", "concepts/")
	if err != nil {
		t.Fatal(err)
	}
	if s.ID == "" {
		t.Fatal("expected non-empty session ID")
	}
	if s.Branch != "machine/test" {
		t.Fatalf("expected branch machine/test, got %q", s.Branch)
	}
	if s.PathPrefix != "concepts/" {
		t.Fatalf("expected path_prefix concepts/, got %q", s.PathPrefix)
	}
	if s.LastCommit != "" {
		t.Fatalf("expected empty last_commit, got %q", s.LastCommit)
	}
	if s.Status != "active" {
		t.Fatalf("expected active status, got %q", s.Status)
	}
	if s.CreatedAt == "" {
		t.Fatal("expected non-empty created_at")
	}
	if s.UpdatedAt == "" {
		t.Fatal("expected non-empty updated_at")
	}
}

func TestGetExploreSession(t *testing.T) {
	idx := newTestIndex(t)

	s, err := idx.CreateExploreSession("machine/test", "")
	if err != nil {
		t.Fatal(err)
	}

	got, err := idx.GetExploreSession(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected session, got nil")
	}
	if got.ID != s.ID {
		t.Fatalf("expected ID %q, got %q", s.ID, got.ID)
	}
	if got.Branch != s.Branch {
		t.Fatalf("expected branch %q, got %q", s.Branch, got.Branch)
	}
	if got.PathPrefix != s.PathPrefix {
		t.Fatalf("expected path_prefix %q, got %q", s.PathPrefix, got.PathPrefix)
	}
	if got.Status != "active" {
		t.Fatalf("expected active, got %q", got.Status)
	}

	// Nonexistent returns nil, no error.
	got2, err := idx.GetExploreSession("nonexistent-id")
	if err != nil {
		t.Fatal(err)
	}
	if got2 != nil {
		t.Fatal("expected nil for nonexistent session")
	}
}

func TestUpdateExploreSession(t *testing.T) {
	idx := newTestIndex(t)

	s, err := idx.CreateExploreSession("machine/test", "")
	if err != nil {
		t.Fatal(err)
	}

	if err := idx.UpdateExploreSession(s.ID, "abc123", "completed"); err != nil {
		t.Fatal(err)
	}

	got, err := idx.GetExploreSession(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastCommit != "abc123" {
		t.Fatalf("expected last_commit abc123, got %q", got.LastCommit)
	}
	if got.Status != "completed" {
		t.Fatalf("expected completed, got %q", got.Status)
	}
	// updated_at should be set (non-empty).
	if got.UpdatedAt == "" {
		t.Fatal("expected non-empty updated_at after update")
	}
}

func TestExploreSeenPaths(t *testing.T) {
	idx := newTestIndex(t)

	s, err := idx.CreateExploreSession("machine/test", "")
	if err != nil {
		t.Fatal(err)
	}

	// Initially empty.
	seen, err := idx.GetExploreSeenPaths(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 0 {
		t.Fatalf("expected 0 seen paths, got %d", len(seen))
	}

	// Add some paths.
	if err := idx.AddExploreSeenPaths(s.ID, []string{"a.md", "b.md", "c.md"}); err != nil {
		t.Fatal(err)
	}

	seen, err = idx.GetExploreSeenPaths(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 3 {
		t.Fatalf("expected 3 seen paths, got %d", len(seen))
	}
	for _, p := range []string{"a.md", "b.md", "c.md"} {
		if !seen[p] {
			t.Fatalf("expected %q in seen paths", p)
		}
	}

	// Add duplicates — should not grow.
	if err := idx.AddExploreSeenPaths(s.ID, []string{"b.md", "c.md", "d.md"}); err != nil {
		t.Fatal(err)
	}

	seen, err = idx.GetExploreSeenPaths(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 4 {
		t.Fatalf("expected 4 seen paths after adding 1 new + 2 duplicates, got %d", len(seen))
	}
}

func TestGCExploreSessions(t *testing.T) {
	idx := newTestIndex(t)

	// Create 4 sessions on the same branch.
	var ids []string
	for i := 0; i < 4; i++ {
		s, err := idx.CreateExploreSession("machine/test", "")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, s.ID)
	}

	// Add seen paths to session 1 to test cascade.
	if err := idx.AddExploreSeenPaths(ids[1], []string{"x.md", "y.md"}); err != nil {
		t.Fatal(err)
	}

	// Keep only 2 most recent.
	if err := idx.GCExploreSessions("machine/test", 2); err != nil {
		t.Fatal(err)
	}

	// Sessions 0, 1 should be deleted; 2, 3 should remain.
	for _, id := range ids[:2] {
		got, err := idx.GetExploreSession(id)
		if err != nil {
			t.Fatal(err)
		}
		if got != nil {
			t.Fatalf("expected session %s to be deleted by GC", id)
		}
	}
	for _, id := range ids[2:] {
		got, err := idx.GetExploreSession(id)
		if err != nil {
			t.Fatal(err)
		}
		if got == nil {
			t.Fatalf("expected session %s to survive GC", id)
		}
	}

	// Seen paths for deleted session should also be gone (cascade).
	seen, err := idx.GetExploreSeenPaths(ids[1])
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 0 {
		t.Fatalf("expected 0 seen paths after GC cascade, got %d", len(seen))
	}
}
