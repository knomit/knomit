package web

import (
	"net"
	"strings"
)

// loopbackHostOK reports whether a Host header names this machine the way a
// browser on it would, so that a loopback peer sending it may be the
// anonymous principal (#281). It admits:
//
//   - an absent Host (HTTP/1.0) — no browser sends one;
//   - localhost, in any case;
//   - any IP literal, bracketed or not. DNS rebinding needs a NAME the
//     attacker answers DNS for; an IP-literal Host cannot come from it, and
//     admitting all of them keeps a same-host proxy addressed by IP (a
//     Tailscale 100.x address, say) working with no configuration;
//   - a name in listed, which the caller has already lower-cased (the
//     effective [auth].loopback_hosts, bind host included).
//
// It compares the NAME, never the port. The port is not what defeats
// rebinding, and vite's dev proxy forwards Host: localhost:5173 to a server
// on another port. The 3b approval gate's ownHost asks a stricter question —
// same-origin proof, arrival port included — and every Host it admits also
// passes here.
//
// Exact matches only. "localhost.attacker.example", "app.localhost",
// "127.0.0.1.nip.io" and "localhost." are all names someone else can answer
// DNS for, or that this rule has no reason to trust.
func loopbackHostOK(host string, listed []string) bool {
	if host == "" {
		return true
	}
	name := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		name = h
	} else if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		name = host[1 : len(host)-1] // "[::1]" with no port
	}
	if net.ParseIP(name) != nil {
		return true
	}
	name = strings.ToLower(name)
	if name == "localhost" {
		return true
	}
	for _, l := range listed {
		if name == l {
			return true
		}
	}
	return false
}
