package web

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/edge/web/hal"
)

// testLocalRepoID is "this repo" for the ref-view tests.
const testLocalRepoID = "3ec012f5b4d2"

// The ref views resolve fact-path refs through readerRefResolver over a REAL
// store; there is no resolver stub. newRefResolverFixture gives each choice the
// resolver makes two candidates that answer differently:
//
//   - the branch of a HEAD lookup: agent/elsewhere (E) forks from agent/test
//     (A) after g2; kb/aonly.md is then written on A only and kb/late.md on E
//     only, so a lookup on the wrong branch flips both;
//   - the commit (anchor vs HEAD): x0 is an E commit before kb/late.md and g2
//     an A commit before kb/aonly.md, so a lookup at the branch's HEAD instead
//     of at the anchor calls them facts. Only that direction (absent at the
//     anchor, present at HEAD) is pinned. The other (present at the anchor,
//     absent at HEAD) cannot be built on one branch, because FactExistsAt
//     steps over retractions and so never turns false later; across branches
//     it would need an anchor off the requested branch (a1 on E), which the
//     anchored walk assumes never happens
//     (kb/gotchas/store/anchored-read-ignores-branch), so no row asks for it;
//   - walk-back: kb/gone.md is retracted on E at e2, and still resolves there
//     (FactExistsAt steps over retractions; a retracted target keeps a
//     navigable last-valid version).
//
// The store discards the branch of an ANCHORED lookup
// (kb/gotchas/store/anchored-read-ignores-branch), so it has no observable
// candidate there.
type refResolverFixture struct {
	head readerRefResolver // E at HEAD; at() derives the others
	c    map[string]string
}

func newRefResolverFixture(t *testing.T) *refResolverFixture {
	t.Helper()
	ctx := context.Background()
	m, svcs := newRealStoreManager(t, "alpha")
	svc := svcs["alpha"]
	f := &refResolverFixture{c: map[string]string{}}

	write := func(step, branch, path, title string) {
		t.Helper()
		res, err := svc.Facts().WriteFact(ctx, branch, path, factText(t, path, title, 0.9, nil), step+": "+path, "learn")
		require.NoError(t, err)
		require.NotEmpty(t, res.CommitHash)
		f.c[step] = res.CommitHash
	}

	write("g1", fsAgentBranch, "kb/shared.md", "Shared")
	write("g2", fsAgentBranch, "kb/gone.md", "Gone on elsewhere")
	require.NoError(t, svc.CreateBranch(ctx, fsOtherBranch, fsAgentBranch))
	write("a1", fsAgentBranch, "kb/aonly.md", "Agent only")
	write("x0", fsOtherBranch, "kb/x.md", "Unrelated")
	write("e1", fsOtherBranch, "kb/late.md", "Late")
	h, _, err := svc.Facts().BatchWriteFacts(ctx, fsOtherBranch, nil, []string{"kb/gone.md"}, "e2: retract", "retract")
	require.NoError(t, err)
	f.c["e2"] = h

	f.head = readerRefResolver{ctx: ctx, ri: m.Get("alpha"), branch: fsOtherBranch}
	return f
}

// at is the fixture's resolver on branch, anchored at step's commit ("" is
// HEAD).
func (f *refResolverFixture) at(branch, step string) readerRefResolver {
	r := f.head // a copy: the struct is a value
	r.branch = branch
	r.commit = ""
	if step != "" {
		r.commit = f.c[step]
	}
	return r
}

// Exists answers at the resolver's own branch and anchor commit. Sabotage:
// drop the commit (x0 sees kb/late.md), drop or swap the branch (E's HEAD
// loses kb/late.md or gains kb/aonly.md), or invert the answer.
func TestReaderRefResolver_Exists_AnchorAndBranch(t *testing.T) {
	f := newRefResolverFixture(t)
	paths := []string{"kb/shared.md", "kb/late.md", "kb/aonly.md", "kb/gone.md", "kb/never.md"}
	for _, tc := range []struct {
		name         string
		branch, step string
		want         []bool // per paths
	}{
		{"E at HEAD", fsOtherBranch, "", []bool{true, true, false, true, false}},
		{"E at x0 (before kb/late.md)", fsOtherBranch, "x0", []bool{true, false, false, true, false}},
		{"A at HEAD", fsAgentBranch, "", []bool{true, false, true, true, false}},
		{"A at g2 (before kb/aonly.md)", fsAgentBranch, "g2", []bool{true, false, false, true, false}},
		{"A at a1", fsAgentBranch, "a1", []bool{true, false, true, true, false}},
	} {
		r := f.at(tc.branch, tc.step)
		got := make([]bool, 0, len(paths))
		for _, p := range paths {
			got = append(got, r.Exists(p))
		}
		require.Equal(t, tc.want, got, "%s: Exists over %v", tc.name, paths)
	}
}

// Every ref is classified on its own, in order, with two refs of every kind:
// a local fact present at the resolver's anchor (bare or self-qualified kb://)
// is "fact" with a target link at the VIEW's anchor; one absent there is
// "broken" with its repo-relative path; the rest never reach the resolver.
// Sabotage: resolve every ref as refs[0], ask the resolver for the raw kb://
// string (the self-qualified fact turns broken), resolve at HEAD (kb/late.md
// turns fact), or link without the view's commit.
func TestBuildRefViews_EveryKindInOrder(t *testing.T) {
	f := newRefResolverFixture(t)
	b := hal.URLBuilder{Base: "/api/v1"}
	a := hal.Anchor{Branch: fsOtherBranch, Commit: f.c["x0"]}
	self := "kb://" + testLocalRepoID + "/"
	srcA := "src://7b4887ce51d9/internal/x.go@4154e92c8ff333435fd00c442489e855e4c3331e:36b1d45187d6a2c6ad18d591142227ad2a02a66e"
	srcB := "src://knomit/internal/legacy.go@ca1c272"
	link := func(p string) hal.LinkMap {
		return hal.LinkMap{"target": {Href: "/api/v1/repos/alpha/branches/" + fsOtherURL + "/commits/" + f.c["x0"] + "/facts/" + p}}
	}

	got := BuildRefViews(b, "alpha", a, []string{
		"kb/shared.md",
		"kb/late.md",
		"kb://7b4887ce51d9/kb/z.md",
		self + "kb/gone.md",
		".knomit/skills/x/SKILL.md",
		srcA,
		"https://arxiv.org/abs/1706.03762",
		self + "kb/aonly.md",
		"kb://aaaaaaaaaaaa/kb/w.md",
		self + ".knomit/ontology.yaml",
		srcB,
		"file:///etc/hosts",
	}, f.at(fsOtherBranch, "x0"), testLocalRepoID)

	require.Equal(t, []RefView{
		{Raw: "kb/shared.md", Kind: "fact", Path: "kb/shared.md", Links: link("kb/shared.md")},
		{Raw: "kb/late.md", Kind: "broken", Path: "kb/late.md"},
		{Raw: "kb://7b4887ce51d9/kb/z.md", Kind: "foreign"},
		{Raw: self + "kb/gone.md", Kind: "fact", Path: "kb/gone.md", Links: link("kb/gone.md")},
		{Raw: ".knomit/skills/x/SKILL.md", Kind: "system_file", Path: ".knomit/skills/x/SKILL.md"},
		{Raw: srcA, Kind: "source_code"},
		{Raw: "https://arxiv.org/abs/1706.03762", Kind: "url"},
		{Raw: self + "kb/aonly.md", Kind: "broken", Path: "kb/aonly.md"},
		{Raw: "kb://aaaaaaaaaaaa/kb/w.md", Kind: "foreign"},
		{Raw: self + ".knomit/ontology.yaml", Kind: "system_file", Path: ".knomit/ontology.yaml"},
		{Raw: srcB, Kind: "source_code"},
		{Raw: "file:///etc/hosts", Kind: "url"},
	}, got)
}

// A ref to a .knomit/ file is a system_file: not a fact (so never "broken" for
// want of a fact at that path), no link, Path the exact file path, case kept.
func TestBuildRefViews_SystemFileKind(t *testing.T) {
	f := newRefResolverFixture(t)
	b := hal.URLBuilder{Base: "/api/v1"}
	a := hal.Anchor{Branch: "agent/test"}
	refs := []string{
		".knomit/skills/x/SKILL.md",
		"kb://" + testLocalRepoID + "/.knomit/ontology.yaml",
		"kb://7b4887ce51d9/.knomit/recipes/r.js",
	}
	got := BuildRefViews(b, "alpha", a, refs, f.head, testLocalRepoID)
	want := []string{".knomit/skills/x/SKILL.md", ".knomit/ontology.yaml", ".knomit/recipes/r.js"}
	require.Len(t, got, len(want))
	for i, v := range got {
		if v.Kind != "system_file" || v.Path != want[i] || v.Links != nil {
			t.Errorf("ref %q: got %+v, want kind system_file, path %q, no links", refs[i], v, want[i])
		}
	}
}

func TestBuildRefViews_URLKind_ExternalHttp(t *testing.T) {
	f := newRefResolverFixture(t)
	b := hal.URLBuilder{Base: "/api/v1"}
	a := hal.Anchor{Branch: "agent/test"}

	got := BuildRefViews(b, "alpha", a, []string{"https://arxiv.org/abs/1706.03762"}, f.head, testLocalRepoID)
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
	f := newRefResolverFixture(t)
	b := hal.URLBuilder{Base: "/api/v1"}
	a := hal.Anchor{Branch: "agent/test"}

	got := BuildRefViews(b, "alpha", a, []string{"config.yaml"}, f.head, testLocalRepoID)
	if got[0].Kind != "broken" {
		t.Errorf("kind: %q, want broken", got[0].Kind)
	}
	if got[0].Links != nil {
		t.Errorf("broken refs must not have links, got %+v", got[0].Links)
	}
}

func TestBuildRefViews_FactKind_WithTargetLink(t *testing.T) {
	f := newRefResolverFixture(t)
	b := hal.URLBuilder{Base: "/api/v1"}
	a := hal.Anchor{Branch: "agent/test"}

	got := BuildRefViews(b, "alpha", a, []string{"kb/shared.md"}, f.head, testLocalRepoID)
	if got[0].Kind != "fact" {
		t.Errorf("kind: %q", got[0].Kind)
	}
	want := "/api/v1/repos/alpha/branches/agent:test/facts/kb/shared.md"
	if href := got[0].Links["target"].Href; href != want {
		t.Errorf("target: got %q, want %q", href, want)
	}
}

func TestBuildRefViews_BrokenKind_NoTargetLink(t *testing.T) {
	f := newRefResolverFixture(t)
	b := hal.URLBuilder{Base: "/api/v1"}
	a := hal.Anchor{Branch: "agent/test"}

	got := BuildRefViews(b, "alpha", a, []string{"kb/missing.md"}, f.head, testLocalRepoID)
	if got[0].Kind != "broken" {
		t.Errorf("kind: %q", got[0].Kind)
	}
	if got[0].Links != nil {
		t.Errorf("broken refs must not have links, got %+v", got[0].Links)
	}
}

func TestBuildRefViews_CommitAnchoredTarget_CarriesShaSegment(t *testing.T) {
	f := newRefResolverFixture(t)
	b := hal.URLBuilder{Base: "/api/v1"}
	a := hal.Anchor{Branch: "agent/test", Commit: "abc123"}

	got := BuildRefViews(b, "alpha", a, []string{"kb/shared.md"}, f.head, testLocalRepoID)
	want := "/api/v1/repos/alpha/branches/agent:test/commits/abc123/facts/kb/shared.md"
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
	f := newRefResolverFixture(t)
	b := hal.URLBuilder{Base: "/api/v1"}
	a := hal.Anchor{Branch: "agent/test"}

	got := BuildRefViews(b, "alpha", a,
		[]string{"kb://7b4887ce51d9/kb/z.md"}, f.head, testLocalRepoID)
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
	f := newRefResolverFixture(t)
	b := hal.URLBuilder{Base: "/api/v1"}
	a := hal.Anchor{Branch: "agent/test"}

	for _, ref := range []string{
		"src://7b4887ce51d9/internal/x.go@4154e92c8ff333435fd00c442489e855e4c3331e:36b1d45187d6a2c6ad18d591142227ad2a02a66e",
		"src://knomit/internal/legacy.go@ca1c272",
	} {
		got := BuildRefViews(b, "alpha", a, []string{ref}, f.head, testLocalRepoID)
		if got[0].Kind != "source_code" {
			t.Errorf("%q: kind = %q, want source_code", ref, got[0].Kind)
		}
		if got[0].Links != nil {
			t.Errorf("%q: source refs carry no link", ref)
		}
	}
}

// A kb://<own-id>/… ref is the canonical form for a fact in THIS repo, so it
// resolves like the bare form — the resolver must be asked for the
// REPO-RELATIVE path, never the raw kb:// string, which names no fact in the
// store (so asking for it would report the ref broken).
func TestBuildRefViews_SelfQualifiedResolvesByRelativePath(t *testing.T) {
	f := newRefResolverFixture(t)
	b := hal.URLBuilder{Base: "/api/v1"}
	a := hal.Anchor{Branch: "agent/test"}
	raw := "kb://" + testLocalRepoID + "/kb/shared.md"
	require.False(t, f.head.Exists(raw), "precondition: the raw kb:// string names no fact")

	got := BuildRefViews(b, "alpha", a, []string{raw}, f.head, testLocalRepoID)

	if got[0].Kind != "fact" {
		t.Fatalf("kind: %q, want fact", got[0].Kind)
	}
	require.Equal(t, "kb/shared.md", got[0].Path)
	want := "/api/v1/repos/alpha/branches/agent:test/facts/kb/shared.md"
	if href := got[0].Links["target"].Href; href != want {
		t.Errorf("target: got %q, want %q", href, want)
	}
	// Raw is what the author wrote; only the link and lookup are canonicalized.
	if got[0].Raw != raw {
		t.Errorf("Raw must be verbatim, got %q", got[0].Raw)
	}
}
