package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// F25 — three roots. kb/ holds facts, .knomit/ is the system and is closed to
// every fact tool (with every other dot path), artifacts/ holds agents'
// working files. These tests drive every MCP door against each root.

// systemAreas are the .knomit/ folders an agent could write through the fact
// tools before F25: the ones knomit reads as configuration (skills, recipes,
// triggers, guidance once F23 lands) and an ordinary state area (runs).
var systemAreas = []string{"guidance", "skills", "recipes", "triggers", "runs"}

// threeRootsRepo is a repo whose agent branch carries, written by git (the
// store directly, as a person's push would), one file in each .knomit/ area,
// a hidden draft, and one ordinary fact — so "refused" can never be "not
// found".
func threeRootsRepo(t *testing.T) (context.Context, *repos.RepoInstance) {
	t.Helper()
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithBranch(repos.WithRepoInstance(context.Background(), ri), "agent/test")
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		for _, a := range systemAreas {
			writeRaw(t, context.Background(), svc, ".knomit/"+a+"/x.md", "system file in "+a)
		}
		writeRaw(t, context.Background(), svc, "kb/.drafts/x.md", "a hidden draft")
		writeRaw(t, context.Background(), svc, ".github/x.md", "foreign")
		writeRaw(t, context.Background(), svc, "kb/architecture/real.md", "a real fact")
	}))
	return ctx, ri
}

func closedPaths() []string {
	out := []string{"kb/.drafts/x.md", ".github/x.md"}
	for _, a := range systemAreas {
		out = append(out, ".knomit/"+a+"/x.md")
	}
	return out
}

// T1 + T2 (closed, still closed): every write door refuses every dot path —
// learn `path`, learn `retract`, update, retract — and the branch tip does
// not move.
func TestThreeRoots_WriteDoorsRefuseDotPaths(t *testing.T) {
	ctx, ri := threeRootsRepo(t)
	for _, p := range closedPaths() {
		before := headOf(t, ri, "agent/test")

		r := learnAtPath(t, ctx, p, "t", "b")
		require.Truef(t, r.IsError, "learn path %s", p)
		require.Containsf(t, resultText(t, r), "closed to the fact tools", "learn path %s", p)

		r = callTool(t, LearnHandler(), ctx, map[string]any{
			"moment_name": "m",
			"facts": []any{map[string]any{"topic": "architecture", "category": "x/y", "title": "Carrier",
				"body": "b", "confidence": 0.5, "sources": 1}},
			"retract": []any{p},
		})
		require.Truef(t, r.IsError, "learn retract %s", p)
		require.Containsf(t, resultText(t, r), "closed to the fact tools", "learn retract %s", p)

		r = callTool(t, UpdateHandler(), ctx, map[string]any{
			"file": p, "moment_name": "m", "updates": map[string]any{"body": "INJECTED"},
		})
		require.Truef(t, r.IsError, "update %s", p)
		require.Containsf(t, resultText(t, r), "closed to the fact tools", "update %s", p)

		r = callTool(t, RetractHandler(), ctx, map[string]any{"file": p, "moment_name": "m"})
		require.Truef(t, r.IsError, "retract %s", p)
		require.Containsf(t, resultText(t, r), "closed to the fact tools", "retract %s", p)

		require.Equalf(t, before, headOf(t, ri, "agent/test"), "no door may move the tip for %s", p)
	}
	// The seeded system files are untouched.
	for _, a := range systemAreas {
		require.Equal(t, "system file in "+a, artifactBody(t, ri, ".knomit/"+a+"/x.md"))
	}
}

// T4 (reads, amended 2026-10-06): explain — plain, at a commit, and with a
// history_cursor — and query by path refuse every dot path EXCEPT a file
// under .knomit/ named by its exact path, which explain returns raw
// (read-only, user ruling "reopen reads by path"). query by path refuses
// .knomit/ too: it is never listed. The same reads on kb/ still work.
// Sabotage: drop the IsSystemFilePath branch in resolveExplainTarget → the
// .knomit/ reads are refused → red; route every dot path to it → the
// kb/.drafts and .github reads succeed → red.
func TestThreeRoots_ReadDoorsRefuseDotPaths(t *testing.T) {
	ctx, ri := threeRootsRepo(t)
	head := headOf(t, ri, "agent/test")

	// A real history cursor, issued for the kb fact, re-addressed at each
	// closed path: the refusal must come before any lookup.
	text, isErr := callExplain(t, ctx, map[string]any{"file": "kb/architecture/real.md"})
	require.False(t, isErr, text)
	hc, ok := decodeHistoryCursorFromExplain(t, ctx, "kb/architecture/real.md")

	for _, p := range closedPaths() {
		system := strings.HasPrefix(p, fact.PrivateRoot+"/")

		text, isErr := callExplain(t, ctx, map[string]any{"file": p})
		if system {
			require.Falsef(t, isErr, "explain %s: %s", p, text)
			require.Containsf(t, text, `"kind":"system_file"`, "explain %s", p)
			require.Containsf(t, text, "system file in ", "explain %s returns the raw content", p)
		} else {
			require.Truef(t, isErr, "explain %s: %s", p, text)
			require.Containsf(t, text, "closed to the fact tools", "explain %s", p)
		}

		text, isErr = callExplain(t, ctx, map[string]any{"file": p, "commit": head})
		if system {
			require.Falsef(t, isErr, "explain@commit %s: %s", p, text)
			require.Containsf(t, text, `"commit":"`+head+`"`, "explain@commit %s", p)
		} else {
			require.Truef(t, isErr, "explain@commit %s: %s", p, text)
			require.Containsf(t, text, "closed to the fact tools", "explain@commit %s", p)
		}

		if ok {
			c := hc
			c.Path = p
			text, isErr = callExplain(t, ctx, map[string]any{"file": p, "history_cursor": encodeHistoryCursor(c)})
			require.Truef(t, isErr, "explain history_cursor %s: %s", p, text)
			if system {
				require.Containsf(t, text, "has no history_cursor", "explain history_cursor %s", p)
			} else {
				require.Containsf(t, text, "closed to the fact tools", "explain history_cursor %s", p)
			}
		}

		dir := p[:strings.LastIndex(p, "/")+1]
		r := callTool(t, QueryHandler(), ctx, map[string]any{"path": dir})
		require.Truef(t, r.IsError, "query path %s", dir)
		require.Containsf(t, resultText(t, r), "is private", "query path %s", dir)
	}
	require.True(t, ok, "the kb fact must yield a history cursor, or the cursor half proved nothing")

	// A malformed .knomit/ path is not a system file: refused as before.
	for _, p := range []string{".knomit/runs/../runs/x.md", ".knomit//runs/x.md", ".knomit/runs/"} {
		text, isErr := callExplain(t, ctx, map[string]any{"file": p})
		require.Truef(t, isErr, "explain %s: %s", p, text)
		require.Containsf(t, text, "closed to the fact tools", "explain %s", p)
	}

	r := callTool(t, QueryHandler(), ctx, map[string]any{"path": "kb/architecture"})
	require.False(t, r.IsError, resultText(t, r))
	require.Contains(t, resultText(t, r), "kb/architecture/real.md", "the positive control: kb/ reads still work")
}

// decodeHistoryCursorFromExplain returns a decoded history cursor for file,
// asking explain for one page of history at a time until it issues one.
func decodeHistoryCursorFromExplain(t *testing.T, ctx context.Context, file string) (historyCursor, bool) {
	t.Helper()
	// One write is not enough for a second page; add revisions until explain
	// hands out a cursor.
	ri := repos.RepoFromContext(ctx)
	for i := 0; i < 30; i++ {
		text, isErr := callExplain(t, ctx, map[string]any{"file": file})
		require.False(t, isErr, text)
		if idx := strings.Index(text, `"history_cursor":"`); idx >= 0 {
			rest := text[idx+len(`"history_cursor":"`):]
			tok := rest[:strings.Index(rest, `"`)]
			c, ok := decodeHistoryCursor(tok)
			return c, ok
		}
		require.NoError(t, ri.WithRead(func(svc *store.Service) {
			writeRaw(t, context.Background(), svc, file, "revision "+strings.Repeat("x", i+1))
		}))
	}
	return historyCursor{}, false
}

// T5 (open): an artifact is written, updated, explained with history and
// retracted through the same doors, at the repo root — not under kb/.
func TestThreeRoots_ArtifactRoundTrip(t *testing.T) {
	ctx, ri := threeRootsRepo(t)

	r := learnAtPath(t, ctx, "artifacts/runs/x", "run state", "run 1")
	require.False(t, r.IsError, resultText(t, r))
	require.Contains(t, resultText(t, r), "artifacts/runs/x.md")
	require.Equal(t, "run 1", artifactBody(t, ri, "artifacts/runs/x.md"))
	require.False(t, factExistsOn(t, ri, "agent/test", "kb/artifacts/runs/x.md"), "never placed under the ontology root")

	r = callTool(t, UpdateHandler(), ctx, map[string]any{
		"file": "artifacts/runs/x", "moment_name": "m", "updates": map[string]any{"body": "run 2"},
	})
	require.False(t, r.IsError, resultText(t, r))
	require.Equal(t, "run 2", artifactBody(t, ri, "artifacts/runs/x.md"))

	text, isErr := callExplain(t, ctx, map[string]any{"file": "artifacts/runs/x.md"})
	require.False(t, isErr, text)
	require.Contains(t, text, "run 2")
	require.Contains(t, text, `"history"`)

	// A context is refused: no ontology applies to an artifact (F22).
	r = callTool(t, UpdateHandler(), ctx, map[string]any{
		"file": "artifacts/runs/x.md", "moment_name": "m", "updates": map[string]any{"context": map[string]any{"task": "t-1"}},
	})
	require.True(t, r.IsError)
	require.Contains(t, resultText(t, r), "context is not allowed here")

	r = callTool(t, RetractHandler(), ctx, map[string]any{"file": "artifacts/runs/x.md", "moment_name": "m"})
	require.False(t, r.IsError, resultText(t, r))
	require.False(t, factExistsOn(t, ri, "agent/test", "artifacts/runs/x.md"))

	// Malformed artifact paths: too shallow, a dot segment below the area,
	// an empty segment, "..". Refused by every door.
	for _, p := range []string{"artifacts/x.md", "artifacts/runs/.git/x.md", "artifacts//x.md", "artifacts/runs/../../kb/x.md"} {
		before := headOf(t, ri, "agent/test")
		r = learnAtPath(t, ctx, p, "t", "b")
		require.Truef(t, r.IsError, "learn %s", p)
		r = callTool(t, UpdateHandler(), ctx, map[string]any{"file": p, "moment_name": "m", "updates": map[string]any{"body": "x"}})
		require.Truef(t, r.IsError, "update %s", p)
		r = callTool(t, RetractHandler(), ctx, map[string]any{"file": p, "moment_name": "m"})
		require.Truef(t, r.IsError, "retract %s", p)
		require.Equalf(t, before, headOf(t, ri, "agent/test"), "tip moved for %s", p)
	}
}

// T6 (not a fact): after an artifact write, knomit_query does not return it
// by any filter, and knomit_changes does not list it — while a kb fact
// written in the same run is returned by both (the positive control).
func TestThreeRoots_ArtifactIsNotAFact(t *testing.T) {
	ctx, ri := threeRootsRepo(t)

	r := learnAtPath(t, ctx, "artifacts/runs/needle", "needle artifact", "needle body")
	require.False(t, r.IsError, resultText(t, r))
	r = callTool(t, LearnHandler(), ctx, map[string]any{
		"moment_name": "m",
		"facts": []any{map[string]any{"topic": "architecture", "category": "needle/x", "title": "needle fact",
			"body": "needle body", "confidence": 0.5, "sources": 1}},
	})
	require.False(t, r.IsError, resultText(t, r))

	for _, args := range []map[string]any{
		{"sort": "recent"},
		{"path": "artifacts"},
		{"path": "artifacts/runs"},
		{"type": []any{"observation", "policy"}},
	} {
		r := callTool(t, QueryHandler(), ctx, args)
		require.False(t, r.IsError, resultText(t, r))
		require.NotContainsf(t, resultText(t, r), "artifacts/runs/needle", "query %v", args)
	}
	r = callTool(t, QueryHandler(), ctx, map[string]any{"sort": "recent"})
	require.Contains(t, resultText(t, r), "kb/architecture/needle/x/", "the positive control: the kb fact is indexed")

	out, text, isErr := callChanges(t, repos.NewBindingOfRepo(ri, "agent/test"), map[string]any{})
	require.False(t, isErr, text)
	var sawKB bool
	for _, c := range out.Changes {
		require.NotContains(t, c.Path, "artifacts/", "knomit_changes must not list an artifact")
		if strings.HasPrefix(c.Path, "kb/architecture/needle/") {
			sawKB = true
		}
	}
	require.True(t, sawKB, "the positive control: changes lists the kb fact")
}

// artifactBody reads the body of the fact-format file at path on the agent branch.
func artifactBody(t *testing.T, ri *repos.RepoInstance, path string) string {
	t.Helper()
	var body string
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		res, err := svc.Facts().ReadFact(context.Background(), "agent/test", path, nil)
		require.NoError(t, err, path)
		f, err := fact.ParseFact(path, res.Content)
		require.NoError(t, err, path)
		body = strings.TrimSpace(f.Body)
	}))
	return body
}
