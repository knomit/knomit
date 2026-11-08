package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/domain/fact"
)

// The wiki-link rule (fact.CheckWikiLinks / CheckWikiLinksUpdate) at the two MCP
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

// The update rule judges the RESULTING body only: a fact that already holds
// [[...]] cannot be updated, on any field, until the update removes them.

func TestUpdate_WikiLinkRule_FullBody(t *testing.T) {
	call, body := seedLegacyWikiFact(t)
	start := body()

	isErr, msg := call(bodyArgs("Legacy, reworded: see [[a1b2c3d4]].\n\nAlso [[some-slug]]."))
	require.True(t, isErr, "keeping existing ones must be refused")
	require.Contains(t, msg, "wiki link in fact body: the body contains [[a1b2c3d4]] (line 1), [[some-slug]] (line 3). An update must leave no [[...]] outside code, including ones the fact already had.")
	require.Contains(t, msg, "in refs")
	require.Equal(t, start, body(), "a refused update writes nothing")

	isErr, _ = call(bodyArgs("Legacy, reworded: see [[other]]."))
	require.True(t, isErr, "replacing them with another must be refused")

	isErr, msg = call(bodyArgs("Legacy, reworded: see the fact in refs, and `[[a1b2c3d4]]` in code."))
	require.False(t, isErr, "removing them (code is fine) must be accepted: "+msg)
	require.Equal(t, "Legacy, reworded: see the fact in refs, and `[[a1b2c3d4]]` in code.", body())
}

func TestUpdate_WikiLinkRule_Ops(t *testing.T) {
	call, body := seedLegacyWikiFact(t)
	start := body()

	isErr, msg := call(map[string]any{"ops": []any{replace("Also [[some-slug]].", "Also, plainly.")}})
	require.True(t, isErr, "str_replace leaving one of them must be refused")
	require.Contains(t, msg, "the body contains [[a1b2c3d4]] (line 1).")

	isErr, msg = call(map[string]any{"ops": []any{map[string]any{"op": "append", "text": "More prose."}}})
	require.True(t, isErr, "append to a body that keeps them must be refused")
	require.Contains(t, msg, "including ones the fact already had")

	isErr, _ = call(map[string]any{"ops": []any{replace("[[a1b2c3d4]]", "[[b]]"), replace("Also [[some-slug]].", "Also.")}})
	require.True(t, isErr, "replacing one with another must be refused")
	require.Equal(t, start, body(), "a refused update writes nothing")

	isErr, msg = call(map[string]any{"ops": []any{
		replace("Legacy see [[a1b2c3d4]].", "Legacy see it."),
		replace("Also [[some-slug]].", "Also `[[some-slug]]`."),
		map[string]any{"op": "append", "text": "Code is fine: `[[y]]`."},
	}})
	require.False(t, isErr, "ops that remove them all must be accepted: "+msg)
	require.NotContains(t, body(), "see [[a1b2c3d4]]")
}

func TestUpdate_WikiLinkRule_FrontmatterOnlyRefused(t *testing.T) {
	call, body := seedLegacyWikiFact(t)
	start := body()
	for name, updates := range map[string]map[string]any{
		"refs only":       {"refs": []any{}},
		"confidence only": {"confidence": 0.5},
	} {
		isErr, msg := call(map[string]any{"updates": updates})
		require.True(t, isErr, "%s: an update of a fact holding [[...]] is refused whatever it changes", name)
		require.Contains(t, msg, "An update must leave no [[...]] outside code, including ones the fact already had.")
	}
	require.Equal(t, start, body())
}

// A learn that dedup-merges into an existing fact rewrites it: when the body
// kept is the existing one and it holds [[...]], the learn is refused.
func TestLearn_DedupMergeIntoLegacyFactRefused(t *testing.T) {
	svc, ctx, emb := newPrinciplesTestRepo(t)
	item := func(body string, conf float64) map[string]any {
		return map[string]any{"topic": "architecture", "category": "modules", "title": "Dedup target",
			"body": body, "confidence": conf, "sources": 1}
	}
	// The stub embedder embeds by length, so equal-length texts match exactly.
	res, err := LearnHandler(emb)(ctx, learnItems(item("clean body, length 28 chars", 0.9)))
	require.NoError(t, err)
	require.False(t, res.IsError, resultText(t, res))
	path := mergedFactPath(t, res)
	// Plant [[x]] as a fact written before the rule would hold it, keeping the
	// title and the length so the stub embedding is unchanged.
	f, err := fact.ParseFact(path, readContent(t, svc, path))
	require.NoError(t, err)
	f.Body = "legacy [[x]], len 28 chars!"
	content, err := fact.SerializeFact(f)
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, "agent/test", path, content, "plant legacy body", "")
	require.NoError(t, err)

	res, err = LearnHandler(emb)(ctx, learnItems(item("other body, length 28 chars", 0.5)))
	require.NoError(t, err)
	require.True(t, res.IsError, "the merge keeps the existing body, which holds [[x]]: "+resultText(t, res))
	require.Contains(t, resultText(t, res), "dedup-merge into "+path)
	require.Contains(t, resultText(t, res), "the body contains [[x]] (line 1)")
}

func TestUpdate_ArtifactMayGainWikiLinks(t *testing.T) {
	svc, ctx, _ := newPrinciplesTestRepo(t)
	writeSlot(t, ctx, svc, "state")
	res := callUpdate(t, ctx, opsArgs(opsSlot, map[string]any{"op": "append", "text": "saw [[link]]"}))
	require.False(t, res.IsError, resultText(t, res))
}

// The rule is in every description an agent reads before writing a body,
// in the wording the user approved (2026-11-08; the update strings are the
// drafts for the resulting-body rule), byte for byte.
const (
	approvedLearnBodyTail = "There is no link syntax in a body: every reference, whether another fact's path or an external source such as a URL, goes in refs. `[[...]]` outside an inline code span or fenced code block is refused."
	approvedUpdateBody    = "New body text. There is no link syntax in a body: every reference, whether another fact's path or an external source such as a URL, goes in refs. A body that holds `[[...]]` outside an inline code span or fenced code block is refused, including ones the fact already had: an update to such a fact must remove them or move them to refs."
	approvedOpsTail       = "The body's leading and trailing whitespace is not stored. An edit that leaves an unclosed ``` fence in a body whose fences were balanced is refused, and so is any update that leaves `[[...]]` outside code in the body, even ones it already held: every reference, to a fact or to an external source, goes in refs."
	approvedInstructions  = "- **All references go in refs** — a body has no link syntax. Cite another fact by its path and an external source by its URL in refs, never in the body text. `[[...]]` outside an inline code span or fenced code block is refused on write.\n"
)

func TestWikiLinkRule_InServedDescriptions(t *testing.T) {
	learnBody := learnToolSchemaProperties()["body"].(map[string]any)["description"].(string)
	require.True(t, strings.HasSuffix(learnBody, " "+approvedLearnBodyTail), learnBody)
	require.True(t, strings.HasPrefix(learnBody, "Fact body in natural language. State compound conditions in full"), "the text before the rule is unchanged")
	require.Contains(t, learnBody, "external source")

	updBody := updateToolSchemaProperties()["body"].(map[string]any)["description"].(string)
	require.Equal(t, approvedUpdateBody, updBody)
	require.Contains(t, updBody, "external source")

	require.True(t, strings.HasSuffix(opsDescription, "\n\n"+approvedOpsTail), opsDescription)
	require.Contains(t, opsDescription, "external source")

	// The served tools/list bytes carry the same text.
	for _, tool := range []any{learnTool(), updateTool()} {
		raw, err := json.Marshal(tool)
		require.NoError(t, err)
		require.Contains(t, string(raw), "external source")
	}
}

func TestWikiLinkRule_InServerInstructions(t *testing.T) {
	text := ProfileInstructions("code", "kb", nil)
	require.Contains(t, text, "- **Anchor high-confidence rules in refs**")
	require.Contains(t, text, approvedInstructions, "the Writing Fact Bodies section must state the rule, as approved")
	require.Contains(t, text, "external source")
}
