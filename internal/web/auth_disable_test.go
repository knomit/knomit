package web

import "testing"

// withoutAuthForTests is FORM B of the two sanctioned auth test fixes (plan
// 2026-09-22-f19-phase1-auth-core-socket, Task 7 step 1). It makes the
// server attach the anonymous principal whatever the remote address is, and
// makes writeGate a no-op.
//
// Prefer FORM A (fromLoopback, a loopback address on the request). Reach for
// this only when Form A cannot work:
//
//   - the test ASSERTS on a non-loopback RemoteAddr, so changing it would
//     break the thing under test, or
//   - the test drives NewAPIRouter() directly rather than Handler(), so
//     r.URL.Path carries no /api/v1 prefix and mcpRoutePattern — anchored on
//     APIBase — cannot exempt the MCP routes. That is a property of the test
//     harness, not of production, where the API router is always mounted
//     under APIBase and the exemption matches.
//
// Never fix a collateral failure by widening isLoopback.
func withoutAuthForTests(t *testing.T, s *Server) *Server {
	t.Helper()
	s.authDisabled = true
	return s
}
