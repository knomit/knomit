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
