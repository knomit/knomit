package sessions

import (
	"context"
	"net"
)

// HTTPInfo is what the SERVER observed about a request's connection, as
// opposed to what the client declared about itself in X-Knomit-Client.
type HTTPInfo struct {
	// RemoteIP is the host part of RemoteAddr. The port is deliberately
	// absent: a client opens a new TCP connection — and gets a new ephemeral
	// port — several times within one MCP session, so including it would
	// split one session into many identities.
	RemoteIP  string
	UserAgent string
}

type httpInfoKey struct{}

// WithHTTPInfo stashes the observed connection details in ctx.
//
// It exists because mcp-go does NOT put the *http.Request in the context it
// hands to hooks: the initialize hook is the only place the declared
// clientInfo exists, and without this it would have no way to see the ip and
// User-Agent of the very request carrying it. Wired via
// mcpserver.WithHTTPContextFunc where the streamable HTTP server is built.
func WithHTTPInfo(ctx context.Context, remoteAddr, userAgent string) context.Context {
	return context.WithValue(ctx, httpInfoKey{}, HTTPInfo{
		RemoteIP:  RemoteIP(remoteAddr),
		UserAgent: userAgent,
	})
}

// HTTPInfoFromContext returns what WithHTTPInfo stashed, or the zero value
// for a transport that has no HTTP request behind it (stdio, and tests).
func HTTPInfoFromContext(ctx context.Context) HTTPInfo {
	info, _ := ctx.Value(httpInfoKey{}).(HTTPInfo)
	return info
}

// RemoteIP strips the port from a RemoteAddr; a value with no port is
// returned as is. One definition, shared by the HTTP dispatch recorder and
// the initialize hook, because the two derive the SAME instance id from it
// and must not disagree about what the input is.
func RemoteIP(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}
