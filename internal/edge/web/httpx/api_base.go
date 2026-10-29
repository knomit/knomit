package httpx

import "strings"

// APIBase is the URL prefix for the API router.
const APIBase = "/api/v1"

// UnscopedMCPPath is the unscoped MCP endpoint, relative to the API router.
// The router serves it, and every path under it (UnscopedMCPPattern), as the
// unscoped endpoint, where the client picks a repo or lens with knomit_bind.
const UnscopedMCPPath = "/mcp"

// UnscopedMCPPattern is the chi pattern for the paths under UnscopedMCPPath.
const UnscopedMCPPattern = UnscopedMCPPath + "/*"

// IsUnscopedMCP reports whether p, a path on the OAuth listener (it starts
// with APIBase), is one the router serves as the unscoped MCP endpoint:
// APIBase+UnscopedMCPPath itself or any path under it at a "/" boundary. The
// bearer edge gives a token with such an audience the whole API's reach, and
// both sides are built on the constants above, so what the router serves
// unscoped is exactly what the reach treats as unscoped.
func IsUnscopedMCP(p string) bool {
	root := APIBase + UnscopedMCPPath
	return p == root || strings.HasPrefix(p, root+"/")
}
