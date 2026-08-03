package web

import (
	"testing"

	"knomit/internal/web/hal"
)

// testLocalRepoID is "this repo" for the ref-view tests.
const testLocalRepoID = "3ec012f5b4d2"

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

	got := BuildRefViews(b, "alpha", a, []string{"https://arxiv.org/abs/1706.03762"}, resolver, testLocalRepoID)
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

// A schemeless ref IS a repo-relative fact path — that is what schemeless
// means. "config.yaml" is therefore a local fact ref that does not resolve,
// i.e. broken. It used to report as "url" because the old rule said "not .md
// means external", which had the client render it as a clickable external
// anchor for something that is not a URL at all.
func TestBuildRefViews_SchemelessNonMdIsBrokenNotURL(t *testing.T) {
	b := hal.URLBuilder{Base: "/api/v1"}
	a := hal.Anchor{Branch: "agent/test"}
	resolver := &stubRefResolver{}

	got := BuildRefViews(b, "alpha", a, []string{"config.yaml"}, resolver, testLocalRepoID)
	if got[0].Kind != "broken" {
		t.Errorf("kind: %q, want broken", got[0].Kind)
	}
	if got[0].Links != nil {
		t.Errorf("broken refs must not have links, got %+v", got[0].Links)
	}
}

func TestBuildRefViews_FactKind_WithTargetLink(t *testing.T) {
	b := hal.URLBuilder{Base: "/api/v1"}
	a := hal.Anchor{Branch: "agent/test"}
	resolver := &stubRefResolver{existing: map[string]bool{"know/exists.md": true}}

	got := BuildRefViews(b, "alpha", a, []string{"know/exists.md"}, resolver, testLocalRepoID)
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

	got := BuildRefViews(b, "alpha", a, []string{"know/missing.md"}, resolver, testLocalRepoID)
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

	got := BuildRefViews(b, "alpha", a, []string{"know/exists.md"}, resolver, testLocalRepoID)
	want := "/api/v1/repos/alpha/branches/agent:test/commits/abc123/facts/know/exists.md"
	if href := got[0].Links["target"].Href; href != want {
		t.Errorf("target: got %q, want %q", href, want)
	}
}

// THE BUG THIS TASK FIXES. A cross-repo ref ends in ".md", so the old rule
// judged it not-external and sent it to the resolver, which looked for a local
// fact literally named "kb://7b4887ce51d9/kb/z.md", found nothing, and reported
// the ref as BROKEN. A perfectly valid reference to another repo rendered as
// broken in the UI.
func TestBuildRefViews_ForeignRepoIsNotBroken(t *testing.T) {
	b := hal.URLBuilder{Base: "/api/v1"}
	a := hal.Anchor{Branch: "agent/test"}
	resolver := &stubRefResolver{existing: map[string]bool{}}

	got := BuildRefViews(b, "alpha", a,
		[]string{"kb://7b4887ce51d9/kb/z.md"}, resolver, testLocalRepoID)
	if got[0].Kind != "foreign" {
		t.Errorf("kind: %q, want foreign", got[0].Kind)
	}
	if got[0].Links != nil {
		t.Errorf("foreign refs carry no link (cross-mount hop is a known gap), got %+v", got[0].Links)
	}
	if got[0].Raw != "kb://7b4887ce51d9/kb/z.md" {
		t.Errorf("Raw must be verbatim, got %q", got[0].Raw)
	}
}

// The other half of the same bug: a src:// citation does not end in ".md", so
// it was reported as "url" — which the client renders as a clickable anchor for
// a scheme no browser can open.
func TestBuildRefViews_SourceIsNotURL(t *testing.T) {
	b := hal.URLBuilder{Base: "/api/v1"}
	a := hal.Anchor{Branch: "agent/test"}
	resolver := &stubRefResolver{}

	for _, ref := range []string{
		"src://7b4887ce51d9/internal/x.go@4154e92c8ff333435fd00c442489e855e4c3331e:36b1d45187d6a2c6ad18d591142227ad2a02a66e",
		"src://knomit/internal/legacy.go@ca1c272",
	} {
		got := BuildRefViews(b, "alpha", a, []string{ref}, resolver, testLocalRepoID)
		if got[0].Kind != "source_code" {
			t.Errorf("%q: kind = %q, want source_code", ref, got[0].Kind)
		}
		if got[0].Links != nil {
			t.Errorf("%q: source refs carry no link", ref)
		}
	}
}

// A kb://<own-id>/… ref is the canonical form for a fact in THIS repo, so it
// resolves like the bare form — and the resolver must be asked for the
// REPO-RELATIVE path, never the raw kb:// string.
func TestBuildRefViews_SelfQualifiedResolvesByRelativePath(t *testing.T) {
	b := hal.URLBuilder{Base: "/api/v1"}
	a := hal.Anchor{Branch: "agent/test"}
	rec := &recordingResolver{existing: map[string]bool{"know/exists.md": true}}

	got := BuildRefViews(b, "alpha", a,
		[]string{"kb://3ec012f5b4d2/know/exists.md"}, rec, testLocalRepoID)

	if got[0].Kind != "fact" {
		t.Fatalf("kind: %q, want fact", got[0].Kind)
	}
	if len(rec.asked) != 1 || rec.asked[0] != "know/exists.md" {
		t.Fatalf("resolver asked for %v, want [know/exists.md]", rec.asked)
	}
	want := "/api/v1/repos/alpha/branches/agent:test/facts/know/exists.md"
	if href := got[0].Links["target"].Href; href != want {
		t.Errorf("target: got %q, want %q", href, want)
	}
	// Raw is what the author wrote; only the link and lookup are canonicalized.
	if got[0].Raw != "kb://3ec012f5b4d2/know/exists.md" {
		t.Errorf("Raw must be verbatim, got %q", got[0].Raw)
	}
}

type recordingResolver struct {
	existing map[string]bool
	asked    []string
}

func (r *recordingResolver) Exists(path string) bool {
	r.asked = append(r.asked, path)
	return r.existing[path]
}
