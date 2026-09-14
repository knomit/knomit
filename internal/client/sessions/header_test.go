package sessions

import (
	"net/http"
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

// A control character in a declared value would make http.Header.Set panic or
// the transport reject the request ("invalid header field value") — so a cwd
// containing a newline would break EVERY request the bridge makes, not just
// spoil one column. Cap strips them.
func TestCap_StripsControlCharacters(t *testing.T) {
	in := BridgeInfo{
		InstanceID: "abc", Transport: "stdio", PID: 1, ParentPID: 2,
		Host: "h", User: "u", Cwd: "/tmp/od\nd\rir\x00x\x7f", Branch: "agent/x", Version: "1",
	}
	encoded := in.Encode()
	for _, r := range encoded {
		if r < 0x20 || r == 0x7f {
			t.Fatalf("encoded header still carries control char %q: %q", r, encoded)
		}
	}
	// The transport must accept it.
	h := http.Header{}
	h.Set(ClientHeader, encoded)
	if got := h.Get(ClientHeader); got != encoded {
		t.Fatalf("header not round-tripped through http.Header: %q", got)
	}
	out, err := ParseClientHeader(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if out.Cwd != "/tmp/oddirx" {
		t.Fatalf("cwd = %q, want the control characters removed", out.Cwd)
	}
	if out.InstanceID != "abc" || out.PID != 1 || out.Branch != "agent/x" {
		t.Fatalf("other fields disturbed: %+v", out)
	}
}

// A doubled or trailing ';' must not swallow the rest of the header.
func TestParseClientHeader_SkipsEmptySegments(t *testing.T) {
	got, err := ParseClientHeader("id=abc;;transport=stdio;;;pid=7;")
	if err != nil {
		t.Fatal(err)
	}
	want := BridgeInfo{InstanceID: "abc", Transport: "stdio", PID: 7}
	if got != want {
		t.Fatalf("got %+v want %+v", got, want)
	}
}
