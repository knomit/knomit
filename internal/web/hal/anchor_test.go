package hal

import "testing"

func TestEncodeBranch_SlashToColon(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"agent/test", "agent:test"},
		{"main", "main"},
		{"agent/user/feature-x", "agent:user:feature-x"},
	}
	for _, c := range cases {
		if got := EncodeBranch(c.in); got != c.want {
			t.Errorf("EncodeBranch(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDecodeBranch_ColonToSlash(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"agent:test", "agent/test"},
		{"main", "main"},
		{"agent:user:feature-x", "agent/user/feature-x"},
	}
	for _, c := range cases {
		if got := DecodeBranch(c.in); got != c.want {
			t.Errorf("DecodeBranch(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestAnchor_IsHEAD(t *testing.T) {
	head := Anchor{Branch: "agent/test"}
	if !head.IsHEAD() {
		t.Error("empty commit must be HEAD")
	}
	commit := Anchor{Branch: "agent/test", Commit: "abc123"}
	if commit.IsHEAD() {
		t.Error("non-empty commit must not be HEAD")
	}
}

func TestURLBuilder_APIRoot(t *testing.T) {
	b := URLBuilder{Base: "/api/v1-new"}
	if got := b.APIRoot(); got != "/api/v1-new" {
		t.Errorf("got %q", got)
	}
	if got := b.Repos(); got != "/api/v1-new/repos" {
		t.Errorf("got %q", got)
	}
	if got := b.Repo("alpha"); got != "/api/v1-new/repos/alpha" {
		t.Errorf("got %q", got)
	}
	if got := b.Branches("alpha"); got != "/api/v1-new/repos/alpha/branches" {
		t.Errorf("got %q", got)
	}
}

func TestURLBuilder_BranchEncodesSlash(t *testing.T) {
	b := URLBuilder{Base: "/api/v1-new"}
	a := Anchor{Branch: "agent/test"}
	if got := b.Branch("alpha", a); got != "/api/v1-new/repos/alpha/branches/agent:test" {
		t.Errorf("got %q", got)
	}
}

func TestURLBuilder_FactAtHEAD(t *testing.T) {
	b := URLBuilder{Base: "/api/v1-new"}
	a := Anchor{Branch: "agent/test"}
	got := b.Fact("alpha", a, "know/ai/ml/abc12345.md")
	want := "/api/v1-new/repos/alpha/branches/agent:test/facts/know/ai/ml/abc12345.md"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestURLBuilder_FactAtCommit_CarriesShaSegment(t *testing.T) {
	b := URLBuilder{Base: "/api/v1-new"}
	a := Anchor{Branch: "agent/test", Commit: "abc123"}
	got := b.Fact("alpha", a, "know/ai/ml/abc12345.md")
	want := "/api/v1-new/repos/alpha/branches/agent:test/commits/abc123/facts/know/ai/ml/abc12345.md"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestURLBuilder_FactSubResources(t *testing.T) {
	b := URLBuilder{Base: "/api/v1-new"}
	a := Anchor{Branch: "agent/test"}
	path := "know/a.md"
	if got := b.FactIncoming("alpha", a, path); got != "/api/v1-new/repos/alpha/branches/agent:test/facts/know/a.md/incoming" {
		t.Errorf("incoming: %q", got)
	}
	if got := b.FactOutgoing("alpha", a, path); got != "/api/v1-new/repos/alpha/branches/agent:test/facts/know/a.md/outgoing" {
		t.Errorf("outgoing: %q", got)
	}
	if got := b.FactCommits("alpha", a, path); got != "/api/v1-new/repos/alpha/branches/agent:test/facts/know/a.md/commits" {
		t.Errorf("commits: %q", got)
	}
}
