//go:build windows

package serveraddr

import "testing"

func exampleListeners() []string {
	return []string{`\\.\pipe\knomit-0123456789abcdef`, `\\.\pipe\custom`}
}

func TestParse_NamedPipe(t *testing.T) {
	a, err := Parse("npipe:////./pipe/knomit-abc")
	if err != nil {
		t.Fatal(err)
	}
	if a.Local != `\\.\pipe\knomit-abc` || a.Base != LocalBase {
		t.Fatalf("Parse = %+v", a)
	}
	if got := ForLocal(`\\.\pipe\knomit-abc`); got != "npipe:////./pipe/knomit-abc" {
		t.Fatalf("ForLocal = %q", got)
	}
	for _, bad := range []string{"npipe:////./pipe/", "npipe://C:/x", "unix:///x.sock"} {
		if a, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) = %+v, want an error", bad, a)
		}
	}
}
