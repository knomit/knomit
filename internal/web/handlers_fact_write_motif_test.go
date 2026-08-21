package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	knomitfact "knomit/internal/fact"
)

// putFactContent renders a PUT body carrying the given raw motifs line, so a
// test can send bytes no writer would produce — which is exactly what the raw
// editor lets a client do.
func putFactContent(motifsLine string) string {
	return "---\\ntype: observation\\ndomain: [ai]\\nconfidence: 0.9\\nsources: 1\\nentities: [Anthropic]\\n" +
		motifsLine + "refs: []\\n---\\n\\n# Test Fact\\n\\nBody text.\\n"
}

// putFact drives a PUT through the real router and returns the bytes the
// handler handed to git.
func putFact(t *testing.T, content string) (int, string) {
	t.Helper()
	writer := &stubFactWriter{writeHash: "abc123"}
	s := &Server{
		Manager:   newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{factWriter: writer},
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut,
		"/repos/alpha/branches/agent:test/facts/know/ai/test.md",
		strings.NewReader(`{"content":"`+content+`"}`))
	req.Header.Set("Content-Type", "application/json")
	s.NewAPIRouter().ServeHTTP(rec, req)
	return rec.Code, writer.lastWriteContent
}

// TestFactPut_SubjectMotifIsStripped is the Phase-1 acceptance property on the
// REST write path.
//
// The handler stores the client's bytes VERBATIM unless a gate changed
// something, and ParseFact is lenient by design — so without a motif gate here,
// a subject-restating motif typed into the raw editor reaches disk,
// fact_motifs, and TokenDF, having flowed through no write gate at all. That is
// a genuine hole in MN4's "every write path": this path calls neither
// SerializeFact nor any motif helper, so a grep for helper NAMES cannot see it.
func TestFactPut_SubjectMotifIsStripped(t *testing.T) {
	// "anthropic-ai" is entities ∪ domain for this fixture.
	code, written := putFact(t, putFactContent("motifs: [anthropic-ai, silent-fallback]\\n"))
	require.Equal(t, http.StatusOK, code, "the strip is silent — it must not fail the request")

	f, err := knomitfact.ParseFact("know/ai/test.md", unescape(written))
	require.NoError(t, err)
	require.Equal(t, []string{"silent-fallback"}, f.Motifs,
		"a subject motif must not reach disk through the raw editor")
}

// TestFactPut_MalformedMotifIsNotStored — ParseFact drops a malformed motif
// from the parsed fact, but the handler was storing the CLIENT's bytes, which
// still carried it. The corpus must never gain one this way.
func TestFactPut_MalformedMotifIsNotStored(t *testing.T) {
	code, written := putFact(t, putFactContent("motifs: [onlyoneword, silent-fallback]\\n"))
	require.Equal(t, http.StatusOK, code)
	require.NotContains(t, written, "onlyoneword",
		"a motif the gate rejects must not be committed verbatim")
}

// TestFactPut_CleanContentIsStoredVerbatim — the gate must stay a no-op when it
// has nothing to do. The handler's whole reason for storing client bytes
// unchanged is that a PUT needing no rewrite must not be silently reformatted,
// and a motif gate that reserialized unconditionally would break that.
func TestFactPut_CleanContentIsStoredVerbatim(t *testing.T) {
	content := putFactContent("motifs: [silent-fallback]\\n")
	code, written := putFact(t, content)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, unescape(content), written,
		"content the gate does not change must reach git byte-for-byte")
}

// TestFactPut_MotiflessContentIsStoredVerbatim — the overwhelmingly common
// case, and the one a regression here would hit hardest.
func TestFactPut_MotiflessContentIsStoredVerbatim(t *testing.T) {
	content := putFactContent("")
	code, written := putFact(t, content)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, unescape(content), written)
}

// unescape turns the \n sequences used in JSON test bodies into real newlines.
func unescape(s string) string { return strings.ReplaceAll(s, `\n`, "\n") }
