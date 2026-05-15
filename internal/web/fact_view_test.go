package web

import (
	"encoding/json"
	"strings"
	"testing"

	knomitfact "knomit/internal/fact"
	"knomit/internal/web/hal"
)

func makeTestFact() knomitfact.Fact {
	f := knomitfact.NewFact("know/ai/ml/abc12345.md")
	f.Title = "Attention is all you need"
	f.Body = "Body goes here."
	f.Type = knomitfact.Type("observation")
	f.Domain = []string{"ai", "ml"}
	f.Entities = []string{"transformer"}
	f.Refs = []string{"know/ai/ml/xyz99999.md", "https://arxiv.org/abs/1706.03762"}
	f.Confidence = 0.92
	f.Sources = 3
	return f
}

func TestFactView_HEAD_RequiredLinks(t *testing.T) {
	b := hal.URLBuilder{Base: "/api/v1"}
	a := hal.Anchor{Branch: "agent/test"} // HEAD: empty Commit
	headCommit := "7f3a8b2c"
	resolver := &stubRefResolver{existing: map[string]bool{"know/ai/ml/xyz99999.md": true}}

	view := BuildFactView(b, "alpha", a, headCommit, makeTestFact(), resolver)
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	wantRels := []string{"self", "incoming", "outgoing", "commits", "snapshot", "parent", "branch"}
	for _, rel := range wantRels {
		if !strings.Contains(string(raw), `"`+rel+`":{"href":`) {
			t.Errorf("missing link %q in %s", rel, raw)
		}
	}
	if strings.Contains(string(raw), `"live":`) {
		t.Errorf("HEAD view must not have a live link")
	}
	if !strings.Contains(string(raw), "/commits/7f3a8b2c/facts/") {
		t.Error("snapshot href missing /commits/{headCommit}/ segment")
	}
}

func TestFactView_CommitAnchored_HasIncomingLiveNoSnapshot(t *testing.T) {
	b := hal.URLBuilder{Base: "/api/v1"}
	a := hal.Anchor{Branch: "agent/test", Commit: "abc12399999999999999999999999999999999"}
	resolver := &stubRefResolver{existing: map[string]bool{"know/ai/ml/xyz99999.md": true}}

	view := BuildFactView(b, "alpha", a, "", makeTestFact(), resolver)
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	wantRels := []string{"self", "incoming", "outgoing", "commits", "live", "commit", "parent", "branch"}
	for _, rel := range wantRels {
		if !strings.Contains(string(raw), `"`+rel+`":{"href":`) {
			t.Errorf("missing link %q in %s", rel, raw)
		}
	}
	if strings.Contains(string(raw), `"snapshot":`) {
		t.Errorf("commit-anchored view must not have a snapshot link (self is the snapshot)")
	}

	if !strings.Contains(string(raw), "/commits/abc12399999999999999999999999999999999/facts/") {
		t.Error("self href missing /commits/{sha}/ segment")
	}
}

func TestFactView_RefsAreStructured_AnchorPropagates(t *testing.T) {
	b := hal.URLBuilder{Base: "/api/v1"}
	a := hal.Anchor{Branch: "agent/test", Commit: "abc12399999999999999999999999999999999"}
	resolver := &stubRefResolver{existing: map[string]bool{"know/ai/ml/xyz99999.md": true}}

	view := BuildFactView(b, "alpha", a, "", makeTestFact(), resolver)
	if len(view.Refs) != 2 {
		t.Fatalf("refs len: %d", len(view.Refs))
	}
	if view.Refs[0].Kind != "fact" {
		t.Errorf("ref 0 kind: %q", view.Refs[0].Kind)
	}
	target := view.Refs[0].Links["target"].Href
	if !strings.Contains(target, "/commits/abc12399999999999999999999999999999999/facts/know/ai/ml/xyz99999.md") {
		t.Errorf("ref 0 target must carry commit anchor: %q", target)
	}
	if view.Refs[1].Kind != "url" {
		t.Errorf("ref 1 kind: %q", view.Refs[1].Kind)
	}
	if view.Refs[1].Links != nil {
		t.Errorf("ref 1 links: %+v", view.Refs[1].Links)
	}
}

func TestFactView_AsOf_HEADUsesHeadCommit(t *testing.T) {
	b := hal.URLBuilder{Base: "/api/v1"}
	a := hal.Anchor{Branch: "agent/test"}
	resolver := &stubRefResolver{}

	view := BuildFactView(b, "alpha", a, "7f3a8b2c", makeTestFact(), resolver)
	if view.AsOf.Branch != "agent/test" || view.AsOf.Commit != "7f3a8b2c" {
		t.Errorf("as_of: %+v, want {agent/test, 7f3a8b2c}", view.AsOf)
	}
}

func TestFactView_AsOf_CommitAnchoredUsesAnchorCommit(t *testing.T) {
	b := hal.URLBuilder{Base: "/api/v1"}
	a := hal.Anchor{Branch: "agent/test", Commit: "abc123"}
	resolver := &stubRefResolver{}

	view := BuildFactView(b, "alpha", a, "", makeTestFact(), resolver)
	if view.AsOf.Branch != "agent/test" || view.AsOf.Commit != "abc123" {
		t.Errorf("as_of: %+v, want {agent/test, abc123}", view.AsOf)
	}
}

func TestBuildFactLinks_CommitAnchoredHasIncoming(t *testing.T) {
	b := hal.URLBuilder{Base: "https://k.example.com"}
	a := hal.Anchor{Branch: "main", Commit: "1234abc"}
	links := buildFactLinks(b, "alpha", a, "", "kb/e.md")

	if links["incoming"].Href == "" {
		t.Fatal("incoming link missing on commit-anchored view")
	}
	want := "https://k.example.com/repos/alpha/branches/main/commits/1234abc/facts/kb/e.md/incoming"
	if got := links["incoming"].Href; got != want {
		t.Errorf("incoming href: got %q, want %q", got, want)
	}
}
