// Package serveraddr is the ONE spelling of a knomit server's address, shared
// by everything that names one: `kb` (its server argument and KNOMIT_SERVER,
// for the MCP proxy and the hooks alike) and the server itself, which hands
// its own address to the processes it starts (a recipe's `exec` child).
//
// An address is either a TCP URL, `http://host:port` or `https://host:port`,
// or the server's local authenticated listener: `unix:///absolute/path.sock`
// on unix, `npipe:////./pipe/<name>` (the spelling Docker uses) on Windows.
// Keeping parse and format in one package is what stops the server writing an
// address the bridge then reads differently.
package serveraddr

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// EnvVar is the environment variable that names the server for every call
// `kb` makes. It replaced KNOMIT_HOME-implies-the-server, which it never did.
const EnvVar = "KNOMIT_SERVER"

// LocalBase is the URL prefix requests are built on when the address is a
// local listener. The host is a PLACEHOLDER — the transport dials the socket
// or pipe whatever the URL says — chosen because every server-side Host check
// accepts it (internal/platform/hostguard), although a request arriving over
// the local listener is judged by its peer credentials before any Host check.
const LocalBase = "http://localhost"

// Addr is a parsed server address.
type Addr struct {
	// Raw is the address exactly as given.
	Raw string
	// Base is the URL prefix for requests: scheme://host[:port] for a TCP
	// address, LocalBase for a local one. Never has a trailing slash.
	Base string
	// Local is the local listener to dial (a unix socket path, or a pipe name
	// on Windows); "" for a TCP address.
	Local string
}

// IsLocal reports whether the address names a local listener.
func (a Addr) IsLocal() bool { return a.Local != "" }

// Parse reads an address. It never guesses: a value that is not one of the
// forms above is an error, so a typo fails loudly instead of quietly reaching
// some other server. The error does not name where the value came from —
// callers add that ("KNOMIT_SERVER", "the server argument").
func Parse(raw string) (Addr, error) {
	if raw == "" {
		return Addr{}, errors.New("empty server address")
	}
	if rest, ok := cutScheme(raw, localScheme); ok {
		path, err := parseLocal(rest)
		if err != nil {
			return Addr{}, fmt.Errorf("server address %q: %w", raw, err)
		}
		return Addr{Raw: raw, Base: LocalBase, Local: path}, nil
	}
	if _, ok := cutScheme(raw, otherLocalScheme); ok {
		return Addr{}, fmt.Errorf("server address %q: %s:// is not a local listener on this platform; use %s",
			raw, otherLocalScheme, localForm)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return Addr{}, fmt.Errorf("server address %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return Addr{}, fmt.Errorf("server address %q: want http://host:port, https://host:port or %s", raw, localForm)
	}
	switch {
	case u.Host == "" || u.Hostname() == "":
		return Addr{}, fmt.Errorf("server address %q: no host", raw)
	case u.User != nil:
		return Addr{}, fmt.Errorf("server address %q: credentials do not belong in the address", raw)
	case u.Path != "" && u.Path != "/", u.RawQuery != "", u.Fragment != "", u.Opaque != "":
		return Addr{}, fmt.Errorf("server address %q: only scheme://host:port, no path, query or fragment", raw)
	}
	return Addr{Raw: raw, Base: u.Scheme + "://" + u.Host}, nil
}

// ForLocal is the address of a local listener at path, as Parse reads it back.
func ForLocal(path string) string { return formatLocal(path) }

// ForTCP is the http address of a TCP listener bound at hostport. A wildcard
// or empty host (":19278", "0.0.0.0:19278", "[::]:19278") is a bind address,
// not a dialable one, so it becomes 127.0.0.1.
func ForTCP(hostport string) string {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return "http://" + hostport
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// cutScheme strips "<scheme>://" from raw, matching the scheme
// case-insensitively as URLs do.
func cutScheme(raw, scheme string) (string, bool) {
	p := scheme + "://"
	if len(raw) >= len(p) && strings.EqualFold(raw[:len(p)], p) {
		return raw[len(p):], true
	}
	return "", false
}
