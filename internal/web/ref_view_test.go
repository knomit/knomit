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

	got := BuildRefViews(b, "alpha", a, []string{"https://arxiv.org/abs/1706.03762"}, resolver, testLocalRepoID, nil)
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

	got := BuildRefViews(b, "alpha", a, []string{"config.yaml"}, resolver, testLocalRepoID, nil)
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

	got := BuildRefViews(b, "alpha", a, []string{"know/exists.md"}, resolver, testLocalRepoID, nil)
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

	got := BuildRefViews(b, "alpha", a, []string{"know/missing.md"}, resolver, testLocalRepoID, nil)
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

	got := BuildRefViews(b, "alpha", a, []string{"know/exists.md"}, resolver, testLocalRepoID, nil)
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
		[]string{"kb://7b4887ce51d9/kb/z.md"}, resolver, testLocalRepoID, nil)
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
		got := BuildRefViews(b, "alpha", a, []string{ref}, resolver, testLocalRepoID, nil)
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
		[]string{"kb://3ec012f5b4d2/know/exists.md"}, rec, testLocalRepoID, nil)

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

// Display is the compact LABEL the UI renders in place of Raw. It exists
// server-side because taking a src:// ref apart is ref PARSING, and the client
// is forbidden a second parser (kb/invariants/ui/factbody/ref-scheme-branching).
func TestBuildRefViews_DisplayShortensRepoIDAndHashes(t *testing.T) {
	b := hal.URLBuilder{Base: "/api/v1"}
	a := hal.Anchor{Branch: "agent/test"}
	resolver := &stubRefResolver{}
	namer := func(id12 string) string {
		if id12 == "7b4887ce51d9" {
			return "knomit"
		}
		return ""
	}

	cases := []struct {
		name string
		ref  string
		want string
	}{{
		name: "src: known id becomes a name, both hashes abbreviate",
		ref:  "src://7b4887ce51d9/internal/refs/refs.go@8cba88ff2e1c0556c90b1c9b21574772303b28cf:c451fd992c42a2f30f0db62108259c0647b773dc",
		want: "src://knomit/internal/refs/refs.go@8cba88ff…:c451fd99…",
	}, {
		name: "src: unknown id stays an id rather than inventing a name",
		ref:  "src://ffffffffffff/internal/refs/refs.go@8cba88ff2e1c0556c90b1c9b21574772303b28cf:c451fd992c42a2f30f0db62108259c0647b773dc",
		want: "src://ffffffffffff/internal/refs/refs.go@8cba88ff…:c451fd99…",
	}, {
		name: "src: line range survives the abbreviation",
		ref:  "src://7b4887ce51d9/internal/refs/refs.go@8cba88ff2e1c0556c90b1c9b21574772303b28cf:c451fd992c42a2f30f0db62108259c0647b773dc#L241-L259",
		want: "src://knomit/internal/refs/refs.go@8cba88ff…:c451fd99…#L241-259",
	}, {
		name: "src legacy: named repo and short commit pass through untouched",
		ref:  "src://knomit/internal/legacy.go@ca1c272",
		want: "src://knomit/internal/legacy.go@ca1c272",
	}, {
		name: "foreign kb ref gets the same repo-name overlay",
		ref:  "kb://7b4887ce51d9/kb/z.md",
		want: "kb://knomit/kb/z.md",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := BuildRefViews(b, "alpha", a, []string{tc.ref}, resolver, testLocalRepoID, namer)
			if got[0].Display != tc.want {
				t.Errorf("Display = %q, want %q", got[0].Display, tc.want)
			}
			if got[0].Raw != tc.ref {
				t.Errorf("Raw must stay verbatim: got %q, want %q", got[0].Raw, tc.ref)
			}
		})
	}
}

// A nil namer is the legitimate "no mount table" case (a bare test server, or a
// Server built without a Manager). It must leave ids alone, not panic.
func TestBuildRefViews_NilNamerLeavesIDsInPlace(t *testing.T) {
	b := hal.URLBuilder{Base: "/api/v1"}
	a := hal.Anchor{Branch: "agent/test"}
	got := BuildRefViews(b, "alpha", a,
		[]string{"src://7b4887ce51d9/x.go@8cba88ff2e1c0556c90b1c9b21574772303b28cf:c451fd992c42a2f30f0db62108259c0647b773dc"},
		&stubRefResolver{}, testLocalRepoID, nil)
	if want := "src://7b4887ce51d9/x.go@8cba88ff…:c451fd99…"; got[0].Display != want {
		t.Errorf("Display = %q, want %q", got[0].Display, want)
	}
}

// Kinds the client already renders readably carry no Display, so the client's
// `display || raw` fallback keeps showing exactly what it shows today. A local
// fact is rendered by its repo-relative Path — a competing shortening of the
// same ref could only diverge from it.
func TestBuildRefViews_NoDisplayForLocalAndURLKinds(t *testing.T) {
	b := hal.URLBuilder{Base: "/api/v1"}
	a := hal.Anchor{Branch: "agent/test"}
	resolver := &stubRefResolver{existing: map[string]bool{"know/exists.md": true}}
	namer := func(string) string { return "knomit" }

	for _, ref := range []string{
		"know/exists.md",                    // fact
		"know/missing.md",                   // broken
		"https://arxiv.org/abs/1706.03762",  // url
		"file:///tmp/notes.txt",             // url
		"kb://" + testLocalRepoID + "/x.md", // local fact in canonical form
	} {
		got := BuildRefViews(b, "alpha", a, []string{ref}, resolver, testLocalRepoID, namer)
		if got[0].Display != "" {
			t.Errorf("%q (kind %s): Display = %q, want empty", ref, got[0].Kind, got[0].Display)
		}
	}
}
