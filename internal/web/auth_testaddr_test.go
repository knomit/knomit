package web

import "net/http"

// fromLoopback is FORM A of the two sanctioned auth test fixes (plan
// 2026-09-22-f19-phase1-auth-core-socket, Task 7 step 1): it puts a LOOPBACK
// address on the request, so the request runs as the anonymous loopback
// principal holding [auth].loopback_default — exactly what a local operator
// held before authentication existed.
//
// It is needed because httptest.NewRequest defaults RemoteAddr to
// 192.0.2.1:1234, which is TEST-NET-1 documentation space and deliberately
// NOT loopback, so a mutating request built that way carries no principal at
// all and writeGate refuses it.
//
// This is Form A, not a third form: the only thing it changes is the address
// ON THE REQUEST. It does not touch the server, the gate, or isLoopback.
// Widening isLoopback to make these tests pass would hand every LAN caller
// the anonymous principal's permissions — never do that.
//
// It also gives the request the Host a browser on this machine sends. That
// is still the same form: httptest.NewRequest defaults Host to "example.com",
// which since #281 is what a DNS-rebound page looks like, and a loopback peer
// naming it is refused with 421 before any principal exists. A request that
// already carries a Host of its own keeps it.
func fromLoopback(r *http.Request) *http.Request {
	r.RemoteAddr = "127.0.0.1:1"
	if r.Host == "example.com" {
		r.Host = "localhost"
	}
	return r
}
