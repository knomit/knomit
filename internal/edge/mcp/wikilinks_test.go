package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The wiki-link rule (fact.CheckWikiLinks / CheckWikiLinkEdit) at the two MCP
// write tools. A fact body links nothing: refs are the only link channel.

const wikiFactPath = "kb/architecture/modules/wiki0001.md"

func wikiLearnItem(body string) map[string]any {
	return map[string]any{"topic": "architecture", "category": "modules", "title": "Wiki " + body[:min(len(body), 20)],
		"body": body, "confidence": 0.8, "sources": 1}
}

func TestLearn_WikiLinkInProseRefused(t *testing.T) {
	svc, ctx, emb := newPrinciplesTestRepo(t)
	head := headCommit(t, svc)

	res, err := LearnHandler(emb)(ctx, learnItems(wikiLearnItem("The cause is in [[f6db3a49]].")))
	require.NoError(t, err)
	require.True(t, res.IsError, "a [[...]] in prose must be refused")
	msg := resultText(t, res)
	require.Contains(t, msg, "fact 0: wiki link in fact body: the body contains [[f6db3a49]] (line 1)")
	require.Contains(t, msg, "in refs")
	require.Equal(t, head, headCommit(t, svc), "a refused learn must not commit")
}

func TestLearn_WikiLinkInCodeAccepted(t *testing.T) {
	_, ctx, emb := newPrinciplesTestRepo(t)
	for name, body := range map[string]string{
		"fenced": "The client table is an array of tables:\n\n```toml\n[[oauth.client]]\nid = \"x\"\n```",
		"inline": "Match blanks with `[[:space:]]` in the grep pattern.",
	} {
		t.Run(name, func(t *testing.T) {
			res, err := LearnHandler(emb)(ctx, learnItems(wikiLearnItem(body)))
			require.NoError(t, err)
			require.False(t, res.IsError, resultText(t, res))
		})
	}
}

func TestLearn_ArtifactMayCarryWikiLinks(t *testing.T) {
	_, ctx, emb := newPrinciplesTestRepo(t)
	res, err := LearnHandler(emb)(ctx, learnItems(map[string]any{
		"path": "artifacts/crawl/state.md", "title": "Crawl state",
		"body": "Skipped pages whose links look like [[link]].",
	}))
	require.NoError(t, err)
	require.False(t, res.IsError, "an artifact is not a fact: "+resultText(t, res))
}

// seedLegacyWikiFact writes a fact that predates the rule, with two [[...]]
// in prose, straight to the store (learn would refuse it now).
func seedLegacyWikiFact(t *testing.T) (func(map[string]any) (bool, string), func() string) {
	t.Helper()
	svc, ctx, _ := newPrinciplesTestRepo(t)
	writeRaw(t, ctx, svc, wikiFactPath, "Legacy see [[a1b2c3d4]].\n\nAlso [[some-slug]].")
	call := func(args map[string]any) (bool, string) {
		args["file"] = wikiFactPath
		if _, ok := args["moment_name"]; !ok {
			args["moment_name"] = "wiki"
		}
		res := callUpdate(t, ctx, args)
		return res.IsError, resultText(t, res)
	}
	return call, func() string { return readBody(t, svc, wikiFactPath) }
}

func bodyArgs(body string) map[string]any {
	return map[string]any{"updates": map[string]any{"body": body}}
}

func TestUpdate_WikiLinkEditRule_FullBody(t *testing.T) {
	call, body := seedLegacyWikiFact(t)

	isErr, msg := call(bodyArgs("Legacy see [[a1b2c3d4]].\n\nAlso [[some-slug]] and [[new-one]]."))
	require.True(t, isErr, "adding one must be refused")
	require.Contains(t, msg, "wiki link in fact body: the edit adds [[new-one]] (line 3)")
	require.Contains(t, msg, "in refs")
	require.Equal(t, "Legacy see [[a1b2c3d4]].\n\nAlso [[some-slug]].", body())

	isErr, msg = call(bodyArgs("Legacy, reworded: see [[a1b2c3d4]].\n\nAlso [[some-slug]]."))
	require.False(t, isErr, "keeping existing ones must be accepted: "+msg)

	isErr, msg = call(bodyArgs("Legacy, cleaned: see the fact in refs."))
	require.False(t, isErr, "removing them must be accepted: "+msg)
	require.Equal(t, "Legacy, cleaned: see the fact in refs.", body())
}

func TestUpdate_WikiLinkEditRule_Ops(t *testing.T) {
	call, body := seedLegacyWikiFact(t)
	start := body()

	isErr, msg := call(map[string]any{"ops": []any{replace("Also [[some-slug]].", "Also [[some-slug]], [[x]].")}})
	require.True(t, isErr, "str_replace adding one must be refused")
	require.Contains(t, msg, "the edit adds [[x]]")

	isErr, msg = call(map[string]any{"ops": []any{map[string]any{"op": "append", "text": "See [[y]]."}}})
	require.True(t, isErr, "append adding one must be refused")
	require.Contains(t, msg, "the edit adds [[y]]")
	require.Equal(t, start, body(), "a refused edit must write nothing")

	isErr, msg = call(map[string]any{"ops": []any{map[string]any{"op": "append", "text": "Code is fine: `[[y]]`."}}})
	require.False(t, isErr, "append in an inline span must be accepted: "+msg)

	isErr, msg = call(map[string]any{"ops": []any{replace("Legacy see [[a1b2c3d4]].", "Legacy see it.")}})
	require.False(t, isErr, "str_replace removing one must be accepted: "+msg)
	require.NotContains(t, body(), "[[a1b2c3d4]]")

	isErr, msg = call(map[string]any{"ops": []any{replace("Also [[some-slug]].", "Also [[some-slug]], still.")}})
	require.False(t, isErr, "str_replace keeping one must be accepted: "+msg)
}

func TestUpdate_WikiLinkEditRule_FrontmatterOnlyUntouched(t *testing.T) {
	call, _ := seedLegacyWikiFact(t)
	isErr, msg := call(map[string]any{"updates": map[string]any{"confidence": 0.5}})
	require.False(t, isErr, "an update that does not touch the body is never judged: "+msg)
}

func TestUpdate_ArtifactMayGainWikiLinks(t *testing.T) {
	svc, ctx, _ := newPrinciplesTestRepo(t)
	writeSlot(t, ctx, svc, "state")
	res := callUpdate(t, ctx, opsArgs(opsSlot, map[string]any{"op": "append", "text": "saw [[link]]"}))
	require.False(t, res.IsError, resultText(t, res))
}

// The rule is in every description an agent reads before writing a body.
func TestWikiLinkRule_InServedDescriptions(t *testing.T) {
	learnBody := learnToolSchemaProperties()["body"].(map[string]any)["description"].(string)
	require.Contains(t, learnBody, "There is no inline link syntax: link another fact only by putting its path in refs.")
	require.Contains(t, learnBody, "`[[...]]` outside an inline code span or fenced code block is refused.")

	updBody := updateToolSchemaProperties()["body"].(map[string]any)["description"].(string)
	require.Contains(t, updBody, "There is no inline link syntax: link another fact only by putting its path in refs.")
	require.Contains(t, updBody, "An edit that adds `[[...]]` outside an inline code span or fenced code block is refused")

	require.Contains(t, opsDescription, "one that adds `[[...]]` outside code: link facts through refs.")

	// The served tools/list bytes carry the same text.
	raw, err := json.Marshal(updateTool())
	require.NoError(t, err)
	require.Contains(t, string(raw), "adds `[[...]]` outside code")
	raw, err = json.Marshal(learnTool())
	require.NoError(t, err)
	require.Contains(t, string(raw), "outside an inline code span or fenced code block is refused")
}

func TestWikiLinkRule_InServerInstructions(t *testing.T) {
	text := ProfileInstructions("code", "kb", nil)
	require.True(t, strings.Contains(text, "**Link only through refs**"), "the Writing Fact Bodies section must state the rule")
	require.Contains(t, text, "`[[...]]` outside an inline code span or fenced code block is refused on write")
}
