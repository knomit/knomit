package serveraddr

import (
	"net/url"
	"strings"
	"testing"

	"knomit/internal/platform/hostguard"
)

func TestParse_TCP(t *testing.T) {
	for raw, want := range map[string]string{
		"http://localhost:19278":  "http://localhost:19278",
		"http://127.0.0.1:19310/": "http://127.0.0.1:19310",
		"https://kb.example.com":  "https://kb.example.com",
		"HTTP://h:1":              "http://h:1",
		"http://[::1]:19278":      "http://[::1]:19278",
	} {
		a, err := Parse(raw)
		if err != nil {
			t.Errorf("Parse(%q): %v", raw, err)
			continue
		}
		if a.Base != want || a.IsLocal() || a.Raw != raw {
			t.Errorf("Parse(%q) = %+v, want base %q and no local listener", raw, a, want)
		}
	}
}

func TestParse_Malformed(t *testing.T) {
	for _, raw := range []string{
		"",
		"localhost:19278",           // no scheme
		"ftp://h:1",                 // wrong scheme
		"http://",                   // no host
		"http://:19278",             // no host name
		"http://h:1/api",            // a path
		"http://h:1?x=1",            // a query
		"http://h:1#frag",           // a fragment
		"http://u:p@h:1",            // credentials
		localScheme + "://",         // no listener
		localScheme + "://relative", // not absolute
		otherLocalScheme + ":///x",  // the other platform's form
		"clade",                     // a mistyped subcommand
	} {
		if a, err := Parse(raw); err == nil {
			t.Errorf("Parse(%q) = %+v, want an error", raw, a)
		}
	}
}

// The placeholder host every local-listener request carries must be one the
// server's Host checks accept, or the server would refuse the request even
// over the socket.
func TestLocalBase_HostIsAcceptedByTheServer(t *testing.T) {
	u, err := url.Parse(LocalBase)
	if err != nil {
		t.Fatal(err)
	}
	if !hostguard.LoopbackHostOK(u.Host, nil) {
		t.Fatalf("hostguard refuses the placeholder host %q", u.Host)
	}
	if strings.HasSuffix(LocalBase, "/") {
		t.Fatal("LocalBase must not end in a slash: request paths are appended to it")
	}
}

func TestForLocal_RoundTrips(t *testing.T) {
	for _, p := range exampleListeners() {
		raw := ForLocal(p)
		a, err := Parse(raw)
		if err != nil {
			t.Fatalf("Parse(ForLocal(%q) = %q): %v", p, raw, err)
		}
		if a.Local != p || a.Base != LocalBase {
			t.Fatalf("ForLocal(%q) = %q parsed back to %+v", p, raw, a)
		}
	}
}

func TestForTCP(t *testing.T) {
	for in, want := range map[string]string{
		":19278":          "http://127.0.0.1:19278",
		"0.0.0.0:19278":   "http://127.0.0.1:19278",
		"[::]:19278":      "http://127.0.0.1:19278",
		"127.0.0.1:19310": "http://127.0.0.1:19310",
		"[::1]:5":         "http://[::1]:5",
		"kb.example:80":   "http://kb.example:80",
	} {
		got := ForTCP(in)
		if got != want {
			t.Errorf("ForTCP(%q) = %q, want %q", in, got, want)
		}
		if _, err := Parse(got); err != nil {
			t.Errorf("ForTCP(%q) = %q does not parse back: %v", in, got, err)
		}
	}
}
