package config

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// The issuer set (unified auth plan, Task 3.0; design §15.2).
//
// There is no separate key: the existing top-level external_addresses list
// is ALSO the set of URLs this instance is an issuer at (OAuth discovery,
// login-link URLs, the browser cookie's Secure flag). Its F10 meaning is
// unchanged: the fleet member record still carries the configured list
// verbatim, never anything derived here (OPEN-14 records the overlap: an
// address that names only the mTLS Fleet port is an issuer that serves
// nothing on the main port, which is harmless — an issuer is only matched
// against the request Host and named in discovery).
//
// A token is valid on EVERY address in the set (they are one server); the
// set picks which configured address a request arrived on, never whether a
// credential counts. Nothing here derives a URL from a request's Host.

// IssuerWildcardWarning is logged once at boot when the bind is a wildcard
// and external_addresses is empty: the derived issuer is localhost, which
// nobody on another machine can use.
const IssuerWildcardWarning = "bound to all interfaces with no external_addresses: OAuth, browser login and git from other machines will fail until external_addresses lists the address they use"

// normalizeExternalAddress returns a's one spelling: scheme and host
// lower-cased, the scheme's default port (http 80, https 443) dropped, no
// trailing slash. a must already pass validateExternalAddress.
func normalizeExternalAddress(a string) (*url.URL, error) {
	if err := validateExternalAddress(a); err != nil {
		return nil, err
	}
	u, err := url.Parse(a)
	if err != nil {
		return nil, err
	}
	scheme := strings.ToLower(u.Scheme)
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	return &url.URL{Scheme: scheme, Host: joinHostPort(host, port)}, nil
}

// joinHostPort is net.JoinHostPort that leaves out an empty port, and
// brackets an IPv6 literal either way.
func joinHostPort(host, port string) string {
	if port != "" {
		return net.JoinHostPort(host, port)
	}
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}

// bindKind classifies the configured bind host.
func (c Config) bindKind() (loopback, wildcard bool) {
	h := strings.ToLower(strings.Trim(c.Host, "[]"))
	switch h {
	case "", "0.0.0.0", "::":
		return false, true
	case "localhost":
		return true, false
	}
	if ip := net.ParseIP(h); ip != nil {
		if ip.IsUnspecified() {
			return false, true
		}
		return ip.IsLoopback(), false
	}
	return false, false
}

// Issuers is the issuer set: each configured external_addresses entry,
// normalised (see normalizeExternalAddress), in the configured order, with
// derived false. When the list is empty ONE URL is derived from the bind,
// with derived true:
//
//   - a loopback host (localhost, 127.0.0.1, ::1) ⇒ http://localhost:<port>
//   - one non-wildcard host ⇒ http://<host>:<port>
//   - a wildcard (0.0.0.0, ::, empty) ⇒ http://localhost:<port>, and
//     IssuerWarning says why that is not enough.
//
// It never writes ExternalAddresses (the fleet record copies that verbatim)
// and never looks at any request. An entry Validate would refuse is skipped.
func (c Config) Issuers() (urls []*url.URL, derived bool) {
	if len(c.ExternalAddresses) > 0 {
		for _, a := range c.ExternalAddresses {
			if u, err := normalizeExternalAddress(a); err == nil {
				urls = append(urls, u)
			}
		}
		return urls, false
	}
	loopback, wildcard := c.bindKind()
	host := "localhost"
	if !loopback && !wildcard {
		host = strings.ToLower(strings.Trim(c.Host, "[]"))
	}
	return []*url.URL{{Scheme: "http", Host: joinHostPort(host, c.Port)}}, true
}

// IssuerWarning is the one WARN boot logs about the issuer set, or "" when
// there is nothing to say: only a wildcard bind with no external_addresses
// warrants one.
func (c Config) IssuerWarning() string {
	if len(c.ExternalAddresses) > 0 {
		return ""
	}
	if _, wildcard := c.bindKind(); wildcard {
		return IssuerWildcardWarning
	}
	return ""
}

// MatchIssuer picks the LISTED issuer whose host[:port] equals r's Host
// (case-insensitively; a default port in the Host matches the portless
// entry of its scheme). It is a selection among configured values, never a
// derivation: an unlisted Host answers false. The URL returned is a copy.
func MatchIssuer(issuers []*url.URL, r *http.Request) (*url.URL, bool) {
	host := strings.ToLower(r.Host)
	if host == "" {
		return nil, false
	}
	for _, u := range issuers {
		if u == nil {
			continue
		}
		want := strings.ToLower(u.Host)
		if host == want || host == defaultPortSpelling(u) {
			c := *u
			return &c, true
		}
	}
	return nil, false
}

// defaultPortSpelling is u's host with its scheme's default port spelled
// out ("kb.example:443"), or "" when u names a port of its own.
func defaultPortSpelling(u *url.URL) string {
	if u.Port() != "" {
		return ""
	}
	switch u.Scheme {
	case "http":
		return joinHostPort(strings.ToLower(u.Hostname()), "80")
	case "https":
		return joinHostPort(strings.ToLower(u.Hostname()), "443")
	}
	return ""
}

// validateIssuerSet is Validate's duplicate check: two entries that
// normalise to the same URL are one issuer listed twice.
func validateIssuerSet(addrs []string) error {
	seen := map[string]string{}
	for _, a := range addrs {
		u, err := normalizeExternalAddress(a)
		if err != nil {
			continue // the per-entry check reports it
		}
		if first, ok := seen[u.String()]; ok {
			return fmt.Errorf("config: external_addresses lists %s twice (%q and %q)", u, first, a)
		}
		seen[u.String()] = a
	}
	return nil
}
