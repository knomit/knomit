package config

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func issuerStrings(t *testing.T, c Config) ([]string, bool) {
	t.Helper()
	urls, derived := c.Issuers()
	out := make([]string, len(urls))
	for i, u := range urls {
		out[i] = u.String()
	}
	return out, derived
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestIssuers_DerivedFromLoopbackBind(t *testing.T) {
	for _, host := range []string{"localhost", "127.0.0.1", "::1", "LOCALHOST"} {
		c := Defaults()
		c.Host, c.Port = host, "19278"
		got, derived := issuerStrings(t, c)
		if !derived || !equalStrings(got, []string{"http://localhost:19278"}) {
			t.Errorf("host %q: Issuers = %v derived=%v, want [http://localhost:19278] derived", host, got, derived)
		}
		if w := c.IssuerWarning(); w != "" {
			t.Errorf("host %q: a loopback bind is unambiguous, want no warning, got %q", host, w)
		}
	}
}

func TestIssuers_SingleHostBind(t *testing.T) {
	for _, tc := range []struct{ host, want string }{
		{"box.lan", "http://box.lan:19278"},
		{"Box.LAN", "http://box.lan:19278"},
		{"10.0.0.5", "http://10.0.0.5:19278"},
		{"fd00::5", "http://[fd00::5]:19278"},
	} {
		c := Defaults()
		c.Host, c.Port = tc.host, "19278"
		got, derived := issuerStrings(t, c)
		if !derived || !equalStrings(got, []string{tc.want}) {
			t.Errorf("host %q: Issuers = %v derived=%v, want [%s] derived", tc.host, got, derived, tc.want)
		}
		if w := c.IssuerWarning(); w != "" {
			t.Errorf("host %q: want no warning, got %q", tc.host, w)
		}
	}
}

func TestIssuers_WildcardBindIsLocalhostAndWarns(t *testing.T) {
	for _, host := range []string{"0.0.0.0", "::", ""} {
		c := Defaults()
		c.Host, c.Port = host, "19278"
		got, derived := issuerStrings(t, c)
		if !derived || !equalStrings(got, []string{"http://localhost:19278"}) {
			t.Errorf("host %q: Issuers = %v derived=%v, want [http://localhost:19278] derived", host, got, derived)
		}
		w := c.IssuerWarning()
		if !strings.Contains(w, "bound to all interfaces with no external_addresses") {
			t.Errorf("host %q: warning = %q, want the all-interfaces warning", host, w)
		}
		// With an external address there is nothing to warn about.
		c.ExternalAddresses = []string{"https://kb.example"}
		if w := c.IssuerWarning(); w != "" {
			t.Errorf("host %q with external_addresses: want no warning, got %q", host, w)
		}
	}
}

func TestIssuers_ExplicitListWinsAndIsNormalised(t *testing.T) {
	c := Defaults()
	c.Host, c.Port = "127.0.0.1", "19278"
	// A default port collapses onto the portless spelling (Validate refuses
	// two entries that collapse together); Issuers itself just normalises.
	c.ExternalAddresses = []string{"HTTPS://KB.Example.COM:443", "http://Box.Lan:80", "http://box.lan:8080", "https://[FD00::5]:8443"}
	got, derived := issuerStrings(t, c)
	want := []string{"https://kb.example.com", "http://box.lan", "http://box.lan:8080", "https://[fd00::5]:8443"}
	if derived || !equalStrings(got, want) {
		t.Fatalf("Issuers = %v derived=%v, want %v not derived", got, derived, want)
	}
	if !equalStrings(c.ExternalAddresses, []string{"HTTPS://KB.Example.COM:443", "http://Box.Lan:80", "http://box.lan:8080", "https://[FD00::5]:8443"}) {
		t.Errorf("Issuers rewrote the configured list: %v", c.ExternalAddresses)
	}
}

func TestValidate_ExternalAddressesRefuseDuplicates(t *testing.T) {
	for _, list := range [][]string{
		{"https://kb.example", "https://kb.example"},
		{"https://kb.example", "HTTPS://KB.EXAMPLE"},
		{"https://kb.example", "https://kb.example:443"},
		{"http://box.lan:80", "http://box.lan"},
	} {
		c := Defaults()
		c.Home, c.Socket = t.TempDir(), "/tmp/x.sock"
		c.ExternalAddresses = list
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "external_addresses") || !strings.Contains(err.Error(), "twice") {
			t.Errorf("%v: Validate = %v, want a duplicate external_addresses error", list, err)
		}
	}
	c := Defaults()
	c.Home, c.Socket = t.TempDir(), "/tmp/x.sock"
	c.ExternalAddresses = []string{"https://kb.example", "http://kb.example", "https://kb.example:8443"}
	if err := c.Validate(); err != nil {
		t.Errorf("distinct addresses refused: %v", err)
	}
}

func TestMatchIssuer_NeverInventsAURL(t *testing.T) {
	c := Defaults()
	c.ExternalAddresses = []string{"https://kb.example", "http://box.lan:8080"}
	iss, _ := c.Issuers()
	for _, tc := range []struct {
		host, want string
		ok         bool
	}{
		{"kb.example", "https://kb.example", true},
		{"KB.Example", "https://kb.example", true},
		{"kb.example:443", "https://kb.example", true},
		{"box.lan:8080", "http://box.lan:8080", true},
		{"box.lan", "", false},
		{"evil.example", "", false},
		{"", "", false},
		{"localhost:19278", "", false},
	} {
		r := httptest.NewRequest("GET", "/x", nil)
		r.Host = tc.host
		u, ok := MatchIssuer(iss, r)
		if ok != tc.ok {
			t.Errorf("Host %q: ok = %v, want %v", tc.host, ok, tc.ok)
			continue
		}
		if ok && u.String() != tc.want {
			t.Errorf("Host %q: issuer = %s, want %s", tc.host, u, tc.want)
		}
		if ok {
			u.Path = "/mutated" // a copy: the caller cannot alter the set
		}
	}
	if iss[0].Path != "" || iss[1].Path != "" {
		t.Error("MatchIssuer handed out the set's own *url.URL")
	}
	if _, ok := MatchIssuer(nil, httptest.NewRequest("GET", "/x", nil)); ok {
		t.Error("an empty issuer set matched")
	}
}

// The derived default is a value Issuers returns, never something written
// into the configured list the fleet member record copies verbatim (the
// record side is pinned by repos.TestFleetRecord_RegisterCopiesAddressesVerbatim,
// whose box.lan bind would derive http://box.lan:19278).
func TestIssuers_DerivedDefaultNotInFleetRecord(t *testing.T) {
	c := Defaults()
	c.Host, c.Port = "box.lan", "19278"
	if got, derived := issuerStrings(t, c); !derived || len(got) != 1 {
		t.Fatalf("Issuers = %v derived=%v", got, derived)
	}
	if len(c.ExternalAddresses) != 0 {
		t.Fatalf("Issuers wrote the derived default into ExternalAddresses: %v", c.ExternalAddresses)
	}
	// Twice, through a pointer receiver path too: still nothing written.
	cp := &c
	cp.Issuers()
	if cp.ExternalAddresses != nil {
		t.Fatalf("ExternalAddresses = %v, want nil", cp.ExternalAddresses)
	}
}
