package sessions

import (
	"strings"
	"testing"
)

func TestClientHeader_RoundTrip(t *testing.T) {
	in := BridgeInfo{
		InstanceID: "3f9a1c2b4d5e6f70", Transport: "stdio", ParentApp: "claude",
		Host: "h1v302", User: "pba", Cwd: `/home/pba/my "odd";dir=x`, Branch: "agent/h1v302-8215ac8f",
		Version: "1.4.0", PID: 48213, ParentPID: 48200,
	}
	out, err := ParseClientHeader(in.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("round trip\n got %+v\nwant %+v", out, in)
	}
}

func TestParseClientHeader_Cases(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
		want BridgeInfo
	}{
		{"empty", "", false, BridgeInfo{}},
		{"bare pairs", "id=abc;transport=stdio;pid=12", true, BridgeInfo{InstanceID: "abc", Transport: "stdio", PID: 12}},
		{"unknown key ignored", "id=abc;future=1", true, BridgeInfo{InstanceID: "abc"}},
		{"quoted with escapes", `id=abc;cwd="a\"b;c=d"`, true, BridgeInfo{InstanceID: "abc", Cwd: `a"b;c=d`}},
		{"missing value", "id=;transport=stdio", true, BridgeInfo{Transport: "stdio"}},
		{"bad pid", "id=abc;pid=x", false, BridgeInfo{}},
		{"unterminated quote", `id="abc`, false, BridgeInfo{}},
		{"no equals", "abc", false, BridgeInfo{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseClientHeader(c.in)
			if (err == nil) != c.ok {
				t.Fatalf("ok=%v err=%v", c.ok, err)
			}
			if c.ok && got != c.want {
				t.Fatalf("got %+v want %+v", got, c.want)
			}
		})
	}
}

func TestParseClientHeader_CapsLongValues(t *testing.T) {
	long := strings.Repeat("x", 1000)
	got, err := ParseClientHeader("id=abc;cwd=" + long)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Cwd) != MaxFieldLen+len(truncationSuffix) || !strings.HasSuffix(got.Cwd, truncationSuffix) {
		t.Fatalf("cwd not capped: len=%d", len(got.Cwd))
	}
}

func TestDeriveInstanceID(t *testing.T) {
	a := DeriveInstanceID("h", "u", "/cwd", "claude", "1", "2")
	b := DeriveInstanceID("h", "u", "/cwd", "claude", "1", "2")
	c := DeriveInstanceID("h", "u", "/cwd", "claude", "3", "2")
	if a != b || a == c || len(a) != 16 {
		t.Fatalf("a=%s b=%s c=%s", a, b, c)
	}
	// Field boundaries matter: "ab","c" != "a","bc".
	if DeriveInstanceID("ab", "c") == DeriveInstanceID("a", "bc") {
		t.Fatal("separator must prevent boundary collisions")
	}
}
