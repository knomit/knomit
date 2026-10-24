package kb

import (
	"testing"

	"knomit/internal/edge/web/hal"
	"knomit/internal/engine/store/index"
)

func TestBuildGraphRefItems_AnchorsToSourceCommit(t *testing.T) {
	b := hal.URLBuilder{Base: "https://k.example.com"}
	a := hal.Anchor{Branch: "main"}
	refs := []index.RefSummary{
		{Path: "kb/d.md", Title: "D", Commit: "1234abc"},
		{Path: "kb/d.md", Title: "D v2", Commit: "1236def"},
	}

	got := buildGraphRefItems(b, "alpha", a, refs)
	if len(got) != 2 {
		t.Fatalf("len: got %d, want 2", len(got))
	}
	if got[0].Commit != "1234abc" {
		t.Errorf("got[0].Commit: %q, want %q", got[0].Commit, "1234abc")
	}
	if got[1].Commit != "1236def" {
		t.Errorf("got[1].Commit: %q, want %q", got[1].Commit, "1236def")
	}

	wantHref0 := "https://k.example.com/repos/alpha/branches/main/commits/1234abc/facts/kb/d.md"
	if got := got[0].Links["self"].Href; got != wantHref0 {
		t.Errorf("got[0] self: %q, want %q", got, wantHref0)
	}
	wantHref1 := "https://k.example.com/repos/alpha/branches/main/commits/1236def/facts/kb/d.md"
	if got := got[1].Links["self"].Href; got != wantHref1 {
		t.Errorf("got[1] self: %q, want %q", got, wantHref1)
	}
}
