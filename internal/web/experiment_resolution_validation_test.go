package web

// A {body} resolution is a FACT WRITE. These tests pin that both doors onto
// `commit` — the MCP tool and the REST endpoint — put it through the same
// chain a knomit_update goes through, and reject the same things for the same
// reasons.
//
// The two surfaces are asserted SEPARATELY and against the same table. REST
// shipped with no validation at all while MCP had half of it, which is exactly
// the failure a single shared table catches: a body accepted at one door and
// refused at the other means the two disagree about what commit means.
//
// Each case asserts WHICH check rejected it, not merely that something did.
// "rejected" alone would pass if every body died at the first parse, and the
// last two cases exist precisely because they discriminate between links in
// the chain: a malformed ref dies in serialization, while a well-formed ref to
// a fact that does not exist dies in the refs GATE. A implementation running
// only one of those passes one case and fails the other.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/repos"
	"knomit/internal/store"
)

const resolutionRepo = "jobA-repo"

// conflictedFact seeds one fact on the agent branch, forks an experiment, and
// edits that same path on both sides, leaving a commit that must refuse.
// Returns the experiment name and the conflicting path.
func conflictedFact(t *testing.T, m *repos.Manager, expName string) string {
	t.Helper()
	ctx := context.Background()
	ri := m.Get(resolutionRepo)
	require.NotNil(t, ri)

	const path = "kb/architecture/demo/aaaaaaaa.md"
	body := func(line string) string {
		return "---\ntype: observation\nconfidence: 0.9\n---\n# demo fact\n\n" + line + "\n"
	}

	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		_, err := svc.Facts().WriteFact(ctx, "agent/test", path, body("original"), "seed", "learn")
		require.NoError(t, err)

		_, err = svc.Experiments().OpenExperiment(ctx, expName, "", "agent/test")
		require.NoError(t, err)

		// Both sides change the same path relative to the fork point.
		_, err = svc.Facts().WriteFact(ctx, store.ExperimentBranch(expName), path,
			body("changed in the experiment"), "exp edit", "update")
		require.NoError(t, err)
		_, err = svc.Facts().WriteFact(ctx, "agent/test", path,
			body("changed on the agent branch"), "agent edit", "update")
		require.NoError(t, err)
	}))
	return path
}

// commitViaREST posts a resolutions body to the commit endpoint.
func commitViaREST(t *testing.T, h http.Handler, expName, path, factBody string) (int, string) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"resolutions": map[string]any{path: map[string]string{"body": factBody}},
	})
	require.NoError(t, err)
	req := fromLoopback(httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/repos/%s/experiments/%s/commit", resolutionRepo, expName),
		strings.NewReader(string(payload))))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// commitViaMCP calls knomit_experiment commit with the same resolution.
func commitViaMCP(t *testing.T, h http.Handler, expName, path, factBody string) (string, bool) {
	t.Helper()
	mount := "/repos/" + resolutionRepo + "/branches/" + strings.ReplaceAll(
		store.ExperimentBranch(expName), "/", ":") + "/mcp"
	sid := initAt(t, h, mount)
	args, err := json.Marshal(map[string]any{
		"action":      "commit",
		"name":        expName,
		"resolutions": map[string]any{path: map[string]string{"body": factBody}},
	})
	require.NoError(t, err)
	_, text, isErr := callExperiment(t, h, mount, sid, string(args))
	return text, isErr
}

// The table. `want` is a substring unique to the check that must do the
// rejecting — naming the LINK in the chain, not just the outcome.
var badResolutionBodies = []struct {
	name string
	body string
	want string
}{
	{
		name: "plain prose is caught by the parser",
		body: "just some prose, no frontmatter at all\n",
		want: "frontmatter",
	},
	{
		name: "an empty title heading is caught by the parser",
		// The shape gotchas/mcp/learn/empty-title-unvalidated records: it
		// commits, then query never lists it and explain cannot read it.
		body: "---\ntype: observation\n---\n# \n\nbody text\n",
		want: "title",
	},
	{
		name: "a malformed kb:// ref is caught on the way out, not on the way in",
		// ParseFact accepts this; SerializeFact rejects it. A chain that
		// stopped after parse+validate would let it through.
		body: "---\ntype: observation\nrefs: ['kb://NOTHEXATALL/kb/x/00000000.md']\n---\n# demo fact\n\nbody\n",
		want: "ref",
	},
	{
		name: "a well-formed ref to a fact that does not exist is caught by the refs gate",
		// Serialization is happy with this one — it is syntactically valid.
		// Only the gate knows the target is not there.
		body: "---\ntype: observation\nrefs: ['kb/does/not/exist/00000000.md']\n---\n# demo fact\n\nbody\n",
		want: "resolv",
	},
}

func TestResolutionBody_REST_RunsTheFullUpdateChain(t *testing.T) {
	for _, tc := range badResolutionBodies {
		t.Run(tc.name, func(t *testing.T) {
			h, m := experimentServer(t)
			path := conflictedFact(t, m, "restbad")

			code, body := commitViaREST(t, h, "restbad", path, tc.body)

			require.NotEqual(t, http.StatusNoContent, code,
				"REST accepted a body that no other write path would accept: %s", body)
			require.Contains(t, strings.ToLower(body), tc.want,
				"the rejection must name the check that did it; got: %s", body)

			// And nothing was committed: the experiment survives a refusal.
			ri := m.Get(resolutionRepo)
			require.NoError(t, ri.WithRead(func(svc *store.Service) {
				_, ok, err := svc.Experiments().GetExperiment(context.Background(), "restbad")
				require.NoError(t, err)
				require.True(t, ok, "a rejected resolution must not delete the experiment")
			}))
		})
	}
}

func TestResolutionBody_MCP_RunsTheFullUpdateChain(t *testing.T) {
	for _, tc := range badResolutionBodies {
		t.Run(tc.name, func(t *testing.T) {
			h, m := experimentServer(t)
			path := conflictedFact(t, m, "mcpbad")

			text, isErr := commitViaMCP(t, h, "mcpbad", path, tc.body)

			require.True(t, isErr, "MCP accepted a body that no other write path would accept: %s", text)
			require.Contains(t, strings.ToLower(text), tc.want,
				"the rejection must name the check that did it; got: %s", text)
		})
	}
}

// TestResolutionBody_GoodBodyIsAcceptedAndSerialized: the chain must not be so
// strict that nothing gets through, and what LANDS is the serialized fact
// rather than the caller's raw bytes — the same choice knomit_update makes.
func TestResolutionBody_GoodBodyIsAcceptedAndSerialized(t *testing.T) {
	h, m := experimentServer(t)
	path := conflictedFact(t, m, "goodbody")

	// Deliberately not already in serialized form: trailing whitespace and a
	// missing final newline. If the raw bytes were committed, the stored fact
	// would carry them.
	raw := "---\ntype: observation\nconfidence: 0.9\n---\n# demo fact\n\nboth readings, reconciled   "
	code, body := commitViaREST(t, h, "goodbody", path, raw)
	require.Equal(t, http.StatusNoContent, code, "a valid body must be accepted: %s", body)

	ri := m.Get(resolutionRepo)
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		ctx := context.Background()
		_, ok, err := svc.Experiments().GetExperiment(ctx, "goodbody")
		require.NoError(t, err)
		require.False(t, ok, "a resolved commit deletes the experiment")

		res, err := svc.Facts().ReadFact(ctx, "agent/test", path, nil)
		require.NoError(t, err)
		require.Contains(t, res.Content, "both readings, reconciled")
		require.NotContains(t, res.Content, "changed on the agent branch")
		require.NotContains(t, res.Content, "changed in the experiment")
		require.True(t, strings.HasSuffix(res.Content, "\n"),
			"the committed bytes are SerializeFact's output, which ends in a newline — "+
				"the raw body did not")
	}))
}
