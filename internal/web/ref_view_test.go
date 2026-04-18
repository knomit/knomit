package web

import (
	"testing"

	"knomit/internal/web/hal"
)

// stubRefResolver is a minimal RefResolver for tests. The key `know/exists.md`
// resolves; anything else is broken.
type stubRefResolver struct {
	existing map[string]bool
}

func (s *stubRefResolver) Exists(path string) bool { return s.existing[path] }

func TestBuildRefViews_URLKind_ExternalHttp(t *testing.T) {
	b := hal.URLBuilder{Base: "/api/v1"}
	a := hal.Anchor{Branch: "agent/test"}
	resolver := &stubRefResolver{existing: map[string]bool{}}

	got := BuildRefViews(b, "alpha", a, []string{"https://arxiv.org/abs/1706.03762"}, resolver)
	if len(got) != 1 {
		t.Fatalf("len: %d", len(got))
	}
	if got[0].Kind != "url" {
		t.Errorf("kind: %q", got[0].Kind)
	}
	if got[0].Links != nil {
		t.Errorf("url refs must not have links, got %+v", got[0].Links)
	}
}

func TestBuildRefViews_URLKind_NoMdSuffix(t *testing.T) {
	b := hal.URLBuilder{Base: "/api/v1"}
	a := hal.Anchor{Branch: "agent/test"}
	resolver := &stubRefResolver{}

	got := BuildRefViews(b, "alpha", a, []string{"config.yaml"}, resolver)
	if got[0].Kind != "url" {
		t.Errorf("kind: %q, want url", got[0].Kind)
	}
}

func TestBuildRefViews_FactKind_WithTargetLink(t *testing.T) {
	b := hal.URLBuilder{Base: "/api/v1"}
	a := hal.Anchor{Branch: "agent/test"}
	resolver := &stubRefResolver{existing: map[string]bool{"know/exists.md": true}}

	got := BuildRefViews(b, "alpha", a, []string{"know/exists.md"}, resolver)
	if got[0].Kind != "fact" {
		t.Errorf("kind: %q", got[0].Kind)
	}
	want := "/api/v1/repos/alpha/branches/agent:test/facts/know/exists.md"
	if href := got[0].Links["target"].Href; href != want {
		t.Errorf("target: got %q, want %q", href, want)
	}
}

func TestBuildRefViews_BrokenKind_NoTargetLink(t *testing.T) {
	b := hal.URLBuilder{Base: "/api/v1"}
	a := hal.Anchor{Branch: "agent/test"}
	resolver := &stubRefResolver{existing: map[string]bool{}}

	got := BuildRefViews(b, "alpha", a, []string{"know/missing.md"}, resolver)
	if got[0].Kind != "broken" {
		t.Errorf("kind: %q", got[0].Kind)
	}
	if got[0].Links != nil {
		t.Errorf("broken refs must not have links, got %+v", got[0].Links)
	}
}

func TestBuildRefViews_CommitAnchoredTarget_CarriesShaSegment(t *testing.T) {
	b := hal.URLBuilder{Base: "/api/v1"}
	a := hal.Anchor{Branch: "agent/test", Commit: "abc123"}
	resolver := &stubRefResolver{existing: map[string]bool{"know/exists.md": true}}

	got := BuildRefViews(b, "alpha", a, []string{"know/exists.md"}, resolver)
	want := "/api/v1/repos/alpha/branches/agent:test/commits/abc123/facts/know/exists.md"
	if href := got[0].Links["target"].Href; href != want {
		t.Errorf("target: got %q, want %q", href, want)
	}
}
