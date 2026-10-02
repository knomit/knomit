//go:build !windows

package serveraddr

import "testing"

// exampleListeners are socket paths a server may really listen on: the
// default under a data root (which may contain a space, as macOS's
// "Application Support" does, or a '%'), and the /tmp fallback for a long one.
func exampleListeners() []string {
	return []string{
		"/Users/me/.knomit/knomit.sock",
		"/Users/me/Library/Application Support/knomit/knomit.sock",
		"/tmp/knomit-501/1a2b3c4d.sock",
		"/home/me/odd%20name/knomit.sock",
	}
}

// unix://<path> is read LITERALLY: the three slashes are scheme + an absolute
// path, nothing is percent-decoded, and the host part never swallows a
// segment.
func TestParse_UnixIsLiteral(t *testing.T) {
	for raw, want := range map[string]string{
		"unix:///Users/me/.knomit/knomit.sock":   "/Users/me/.knomit/knomit.sock",
		"unix:///a/b%20c.sock":                   "/a/b%20c.sock",
		"UNIX:///x.sock":                         "/x.sock",
		"unix:///Users/me/Application Support/s": "/Users/me/Application Support/s",
	} {
		a, err := Parse(raw)
		if err != nil {
			t.Errorf("Parse(%q): %v", raw, err)
			continue
		}
		if a.Local != want || a.Base != LocalBase {
			t.Errorf("Parse(%q) = local %q base %q, want %q %q", raw, a.Local, a.Base, want, LocalBase)
		}
	}
	if got := ForLocal("/x/knomit.sock"); got != "unix:///x/knomit.sock" {
		t.Errorf("ForLocal = %q", got)
	}
}
