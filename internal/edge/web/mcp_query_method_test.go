package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// chi v5.4.0 registers the HTTP QUERY method on every Mount, so a QUERY to an
// MCP mount now reaches mcpDispatch instead of being answered 405 by chi. The
// transport (mcp-go StreamableHTTPServer.ServeHTTP) serves only POST, GET and
// DELETE and answers every other method http.NotFound. This pins that: a QUERY
// carrying a valid session id and a JSON-RPC body is refused 404 with the
// transport's plain-text body, is not executed, and is not an SSE stream.
//
// SABOTAGE: have the MCP handler treat QUERY as POST -> status 200 and a
// jsonrpc body -> red.
func TestMCPMounts_QueryMethodIsRefused404(t *testing.T) {
	h, _ := experimentServer(t)
	for _, mount := range []string{unscopedMount, urlScopedMount} {
		t.Run(mount, func(t *testing.T) {
			sid := initAt(t, h, mount)
			req := asOwner(httptest.NewRequest("QUERY", mount,
				strings.NewReader(`{"jsonrpc":"2.0","id":7,"method":"tools/list"}`)))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set("Mcp-Session-Id", sid)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			require.Equal(t, http.StatusNotFound, rec.Code, "body: %s", rec.Body.String())
			require.Equal(t, "404 page not found\n", rec.Body.String())
			require.NotContains(t, rec.Header().Get("Content-Type"), "text/event-stream")
			require.NotContains(t, rec.Body.String(), "jsonrpc")
		})
	}
}
