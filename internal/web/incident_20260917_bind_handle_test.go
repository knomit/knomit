package web

// The 2026-09-17 incident, reproduced end to end.
//
// Two Claude Cowork sessions ran at the same time — a scheduled task bound to a
// lens and an interactive crawl bound to a repo. Claude Desktop opens ONE
// connection per configured MCP server and shares it across all Cowork
// sessions, so both flowed through one knomit-bridge process, one stdio pipe
// and one Mcp-Session-Id. Binding was keyed on that id with an upsert, so each
// job's knomit_bind overwrote the other's; the bridge log shows the binds
// alternating seven times in half an hour, and four knomit_learn calls ran
// against the wrong knowledge base.
//
// Those four calls FAILED, and that is the near miss rather than the safeguard:
// they failed only because the two ontologies happened to share no topic
// ("unknown topic technology" / "unknown topic incidents"). So this test gives
// both repos the SAME ontology. Every write here is valid in either repo, which
// means a write aimed at the wrong one lands silently — exactly the outcome the
// real incident came one shared topic away from.
//
// Everything runs through the real router at /api/v1/mcp with a real Manager
// and two real write repos, on ONE MCP session id, with the two jobs' calls
// interleaved.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/repos"
)

// incidentServer stands up the real API router over a Manager holding two
// WRITABLE repos that share an ontology. Both halves matter: two subscriptions
// or one read-only repo would make a misrouted write fail for the wrong reason,
// and two different ontologies would reproduce the near miss instead of the
// incident.
func incidentServer(t *testing.T) http.Handler {
	t.Helper()
	m := repos.New(context.Background(), repos.Deps{
		Cfg:         config.Config{Home: t.TempDir(), OntologyRoot: "kb"},
		AgentBranch: "agent/test",
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	// newE2EMount gives both repos fact.CodeOntology(), so `architecture` is a
	// valid topic in BOTH. A write routed to the wrong one succeeds.
	newE2EMount(t, m, "jobA-repo", false)
	newE2EMount(t, m, "jobB-repo", false)

	store := newClientSessionsStore(t)
	m.SetClientSessions(store)

	s := &Server{Manager: m, ClientSessions: store, OntologyRoot: "kb"}
	s.buildMCPHandler()
	return s.NewAPIRouter()
}

// rpcAt posts one JSON-RPC message to an arbitrary mount and returns the decoded
// response plus the session id the server used, threading the id the way a real
// client does.
func rpcAt(t *testing.T, h http.Handler, mount, sid, body string) (map[string]any, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, mount, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	payload := rec.Body.String()
	// A streamable-HTTP server may answer as SSE; take the first data: line.
	if strings.HasPrefix(strings.TrimSpace(payload), "event:") || strings.Contains(payload, "\ndata: ") {
		sc := bufio.NewScanner(strings.NewReader(payload))
		for sc.Scan() {
			if rest, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				payload = rest
				break
			}
		}
	}
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(payload), &out), "payload: %s", payload)

	got := rec.Header().Get("Mcp-Session-Id")
	if got == "" {
		got = sid
	}
	return out, got
}

// callToolAt invokes one tool on an arbitrary mount, returning the result text
// and whether it was an error result.
func callToolAt(t *testing.T, h http.Handler, mount, sid, name, args string) (string, bool) {
	t.Helper()
	body := fmt.Sprintf(
		`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, name, args)
	resp, _ := rpcAt(t, h, mount, sid, body)

	result, ok := resp["result"].(map[string]any)
	require.True(t, ok, "no result in %v", resp)
	isErr, _ := result["isError"].(bool)

	var sb strings.Builder
	content, _ := result["content"].([]any)
	for _, c := range content {
		if cm, ok := c.(map[string]any); ok {
			if s, ok := cm["text"].(string); ok {
				sb.WriteString(s)
			}
		}
	}
	return sb.String(), isErr
}

// bindHandle binds repo and returns the opaque handle knomit_bind minted.
//
// The checks on the bind RESULT are non-fatal (assert, not require) on purpose.
// On the session-keyed implementation there is no handle to return, and a fatal
// check here would stop the test at "the field is missing" — true, but not the
// bug. Letting it run on carries an empty handle into the calls below, where an
// unknown `binding` argument is ignored and every call is served from whatever
// was bound LAST. That is the incident, and a reviewer watching this fail on dev
// should see it stated in those terms, not as a missing JSON key.
func bindHandle(t *testing.T, h http.Handler, sid, repo string) string {
	t.Helper()
	text, isErr := callToolAt(t, h, unscopedMount, sid, "knomit_bind", fmt.Sprintf(`{"repo":%q}`, repo))
	require.False(t, isErr, "bind %s: %s", repo, text)

	// The handle is the FIRST key of the result envelope, before the mounts,
	// and the instructions that follow it are prose — so decode just the
	// leading JSON object.
	var envelope struct {
		Binding string `json:"binding"`
		Name    string `json:"name"`
	}
	require.NoError(t, json.NewDecoder(strings.NewReader(text)).Decode(&envelope), "bind result: %s", text)
	assert.Equal(t, repo, envelope.Name, "knomit_bind must name what it bound")
	assert.NotEmpty(t, envelope.Binding,
		"knomit_bind must return an opaque handle; without one nothing in a later call says which job it belongs to")
	assert.NotContains(t, envelope.Binding, repo, "a handle must not be derivable from the name")
	return envelope.Binding
}

// learnArgs builds a one-fact knomit_learn argument object for a handle.
func learnArgs(handle, category, title string) string {
	return fmt.Sprintf(
		`{"binding":%q,"moment_name":"incident","facts":[{"topic":"architecture","category":%q,"title":%q,"body":"written during the interleaving test"}]}`,
		handle, category, title)
}

// learnResult is the part of a knomit_learn result this test reads: WHERE the
// bytes went, and WHICH file they became.
type learnResult struct {
	Commits []struct {
		File string `json:"file"`
	} `json:"commits"`
	WrittenTo struct {
		Repo   string `json:"repo"`
		Branch string `json:"branch"`
	} `json:"written_to"`
}

// learnInto runs one knomit_learn through handle and asserts, BY CONTENT, that
// the commit landed in wantRepo. Returns the fact's path.
func learnInto(t *testing.T, h http.Handler, sid, handle, wantRepo, category, title string) string {
	t.Helper()
	text, isErr := callToolAt(t, h, unscopedMount, sid, "knomit_learn", learnArgs(handle, category, title))
	require.False(t, isErr, "learn through the handle for %q failed: %s", wantRepo, text)

	var res learnResult
	require.NoError(t, json.Unmarshal([]byte(text), &res), "learn result: %s", text)
	require.Equal(t, wantRepo, res.WrittenTo.Repo,
		"THE INCIDENT: this write was issued with the handle for %q but landed in %q — "+
			"another job's bind redirected it", wantRepo, res.WrittenTo.Repo)
	require.Len(t, res.Commits, 1, "learn result: %s", text)
	require.NotEmpty(t, res.Commits[0].File)
	return res.Commits[0].File
}

const (
	unscopedMount = "/mcp"
	// The URL-scoped mount for jobA-repo. The branch segment is escaped because
	// "agent/test" contains a slash.
	urlScopedMount = "/repos/jobA-repo/branches/agent%2Ftest/mcp"
)

// The exact refusals. An agent that cannot tell "you forgot the handle" from
// "that handle is not one of mine" from "this endpoint does not take one"
// retries the wrong repair forever, so these are asserted as whole strings, not
// as substrings and not merely as isError.
const (
	wantMissingHandle = "knomit_learn requires a `binding` argument: the opaque handle knomit_bind returned. " +
		"Call knomit_bind with a repo or lens name, then pass its `binding` value on this and every " +
		"later tool call. There is no session fallback — the handle is the only thing that says " +
		"which knowledge base this call is for."
	wantUnknownHandle     = "unknown binding handle — call knomit_bind"
	wantBindingOnURLScope = "this endpoint is bound by its URL; do not pass binding"
)

// TestIncident20260917_TwoCoworkJobsOneMCPSession is the regression test for the
// incident. It is RED on the session-keyed implementation and green on the
// per-bind handle.
func TestIncident20260917_TwoCoworkJobsOneMCPSession(t *testing.T) {
	h := incidentServer(t)

	// ONE initialize, ONE session id, for everything below — the shared Claude
	// Desktop connection.
	initResp, sid := rpcAt(t, h, unscopedMount, "",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"cowork","version":"1.0"}}}`)
	require.NotEmpty(t, sid, "server must mint a session id")
	instr, _ := initResp["result"].(map[string]any)["instructions"].(string)
	require.Contains(t, instr, "knomit_bind")

	// Job A binds, then job B binds — on the SAME session id. This is the
	// overwrite: under session-keyed binding, B's bind silently redirected A.
	handleA := bindHandle(t, h, sid, "jobA-repo")
	handleB := bindHandle(t, h, sid, "jobB-repo")
	assert.NotEqual(t, handleA, handleB, "each bind must mint its own handle")

	// ---- the interleaving, in order ----

	// 1. Job A writes, AFTER job B has bound. This is the call that went to the
	//    wrong repo.
	fileA := learnInto(t, h, sid, handleA, "jobA-repo", "jobs/a", "job A fact")

	// 2. Job B writes.
	fileB := learnInto(t, h, sid, handleB, "jobB-repo", "jobs/b", "job B fact")

	// Neither write is visible in the other repo — "landed in mine" and "did
	// not also land in yours" are different claims, and only the pair rules out
	// a fan-out.
	requireFactAbsent(t, h, sid, handleB, fileA, "job A's fact must not exist in job B's repo")
	requireFactAbsent(t, h, sid, handleA, fileB, "job B's fact must not exist in job A's repo")

	// 3. A query through A's handle sees A's corpus and only A's.
	//    A path filter, not a text search: this fixture has no embedder, so a
	//    semantic query would return nothing and pass vacuously.
	text, isErr := callToolAt(t, h, unscopedMount, sid, "knomit_query",
		fmt.Sprintf(`{"binding":%q,"path":"kb/architecture/jobs/"}`, handleA))
	require.False(t, isErr, text)
	require.Contains(t, text, "job A fact")
	require.NotContains(t, text, "job B fact", "job A must not read job B's knowledge base")

	// 4. An explain through B's handle resolves B's own fact.
	text, isErr = callToolAt(t, h, unscopedMount, sid, "knomit_explain",
		fmt.Sprintf(`{"binding":%q,"file":%q}`, handleB, fileB))
	require.False(t, isErr, "job B must be able to explain its own fact: %s", text)
	require.Contains(t, text, "job B fact")
	require.NotContains(t, text, "job A fact")

	// 5. Job A binds AGAIN — the event that, under the old scheme, would have
	//    stolen the session out from under job B.
	handleA2 := bindHandle(t, h, sid, "jobA-repo")
	require.NotEqual(t, handleA, handleA2, "a second bind mints a second handle")

	// 6. Job B writes again, carrying the handle it has held all along. The
	//    re-bind of A must have changed nothing for it.
	fileB2 := learnInto(t, h, sid, handleB, "jobB-repo", "jobs/b", "job B second fact")
	requireFactAbsent(t, h, sid, handleA, fileB2,
		"job A's re-bind must not have pulled job B's later write into job A's repo")

	// And A's ORIGINAL handle is still A's: a second bind adds a handle, it does
	// not retire the first.
	_ = learnInto(t, h, sid, handleA, "jobA-repo", "jobs/a", "job A later fact")
	_ = learnInto(t, h, sid, handleA2, "jobA-repo", "jobs/a", "job A newest fact")

	// Both jobs' corpora are intact and disjoint at the end.
	text, isErr = callToolAt(t, h, unscopedMount, sid, "knomit_query",
		fmt.Sprintf(`{"binding":%q,"path":"kb/architecture/jobs/"}`, handleB))
	require.False(t, isErr, text)
	require.Contains(t, text, "job B fact")
	require.Contains(t, text, "job B second fact")
	require.NotContains(t, text, "job A", "job B's repo must hold none of job A's work")
}

// requireFactAbsent asserts that path does not resolve through handle. It is the
// "and not in the other repo" half: knomit_explain answers about ONE binding's
// corpus, so a failure here is the fact genuinely not being there.
//
// It asserts the NOT-FOUND reason, not merely isError. An error-shaped result is
// not evidence of absence — a refused handle, a malformed argument or an
// unavailable store would all satisfy `isError` and make this half of the test
// pass while proving nothing. "could not read <path>" is the answer that only a
// binding which genuinely lacks the fact can give, and naming the path in the
// assertion rules out its being some other fact's failure.
func requireFactAbsent(t *testing.T, h http.Handler, sid, handle, path, msg string) {
	t.Helper()
	text, isErr := callToolAt(t, h, unscopedMount, sid, "knomit_explain",
		fmt.Sprintf(`{"binding":%q,"file":%q}`, handle, path))
	require.True(t, isErr, "%s — but it resolved: %s", msg, text)
	require.Contains(t, text, "could not read",
		"%s — the call failed, but not because the fact is missing: %s", msg, text)
	require.Contains(t, text, path,
		"%s — the not-found is about some other path: %s", msg, text)
}

// The negative half: the three ways a caller can get the handle wrong, each
// asserted on its EXACT message.
func TestIncident20260917_HandleRefusals(t *testing.T) {
	h := incidentServer(t)
	_, sid := rpcAt(t, h, unscopedMount, "",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"cowork","version":"1.0"}}}`)
	handleA := bindHandle(t, h, sid, "jobA-repo")

	// No handle at all on the unscoped mount. There is no session fallback, so
	// this must refuse even though this very session has just bound.
	text, isErr := callToolAt(t, h, unscopedMount, sid, "knomit_learn",
		`{"moment_name":"incident","facts":[{"topic":"architecture","category":"jobs/x","title":"t","body":"b"}]}`)
	require.True(t, isErr, "a learn with no handle must not run: %s", text)
	require.Equal(t, wantMissingHandle, text)

	// A well-formed handle nobody minted: same shape as a real one, 32 URL-safe
	// characters, so this tests the LOOKUP and not the parsing.
	const forged = "Zm9yZ2VkLWhhbmRsZS0yNC1ieXRlcyEh"
	require.Len(t, forged, 32)
	require.NotEqual(t, handleA, forged)
	text, isErr = callToolAt(t, h, unscopedMount, sid, "knomit_learn",
		learnArgs(forged, "jobs/x", "t"))
	require.True(t, isErr, "a forged handle must not run: %s", text)
	require.Equal(t, wantUnknownHandle, text)
	require.NotContains(t, text, handleA, "the refusal must not echo a live handle")

	// A REAL handle on the URL-scoped mount, which is bound by its path. Refused
	// on presence: ignoring it would let the caller believe it selected a repo
	// while the URL served whichever one the path names.
	_, sidURL := rpcAt(t, h, urlScopedMount, "",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"cowork","version":"1.0"}}}`)
	text, isErr = callToolAt(t, h, urlScopedMount, sidURL, "knomit_learn",
		learnArgs(handleA, "jobs/x", "t"))
	require.True(t, isErr, "a handle on a URL-scoped mount must be refused: %s", text)
	require.Equal(t, wantBindingOnURLScope, text)

	// ...and the same mount works normally without one, so the refusal is about
	// the argument and not about the mount.
	text, isErr = callToolAt(t, h, urlScopedMount, sidURL, "knomit_learn",
		`{"moment_name":"incident","facts":[{"topic":"architecture","category":"jobs/url","title":"url fact","body":"b"}]}`)
	require.False(t, isErr, "the URL-scoped mount must still work with no handle: %s", text)
	require.Contains(t, text, `"repo":"jobA-repo"`)
}
