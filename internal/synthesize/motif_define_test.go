package synthesize

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// ── the blindness guard ───────────────────────────────────────────────────

// The payload must carry NAMES ONLY. Blindness is the mechanism that makes the
// generic register achievable rather than merely requested — a writer who never
// saw the carriers cannot name the systems they are about — so it has to be
// enforced by what ships, not by the prompt asking nicely.
func TestMotifDefine_PayloadCarriesNoCarrierContent(t *testing.T) {
	ctx := context.Background()
	env := motifVocabEnv(t, 4)
	sess, err := env.svc.Pipeline().CreatePipelineSession(ctx, "review", env.branch)
	require.NoError(t, err)
	require.NoError(t, planMotifDefineWork(ctx, env.deps(), sess, env.branch))

	item, err := env.svc.Pipeline().NextPipelineWorkItem(ctx, sess.ID)
	require.NoError(t, err)
	require.NotNil(t, item)
	require.Equal(t, motifDefineStepType, item.StepType)

	// The fixture's carrier titles are "Carrier N" and its bodies "body".
	// Neither may appear anywhere in what the model is shown.
	require.NotContains(t, item.FactsJSON, "Carrier",
		"a carrier title reached the definition payload — the pass is no longer blind, "+
			"and a definition written from carriers describes what those facts are about "+
			"rather than the mechanism")

	var entries []motifDefinePayloadEntry
	require.NoError(t, json.Unmarshal([]byte(item.FactsJSON), &entries))
	require.NotEmpty(t, entries)
	for _, e := range entries {
		require.NotEmpty(t, e.Name)
		require.NotEmpty(t, e.ClusterKey,
			"the cluster key must survive the payload round-trip, or the answer cannot "+
				"be routed back and every refresh silently stores nothing")
	}
}

// The prompt must state the blindness WITH its reason, and the register rule
// mechanically rather than stylistically.
func TestMotifDefinePrompt_StatesBlindnessAndRegisterWithReasons(t *testing.T) {
	body, err := os.ReadFile("prompts/large/motif_define_user.txt")
	require.NoError(t, err)
	text := string(body)

	require.Contains(t, text, "You are given the NAMES ONLY")
	require.Contains(t, text, "and that is deliberate",
		"blindness must be explained, not merely imposed — a model told why a "+
			"constraint exists honours it better than one told a rule")
	require.Contains(t, text, "still fits a fact about some entirely different system")

	// The register rule, and every false-generic class it must name.
	for _, want := range []string{"product", "company", "tool", "system", "component", "protocol", "library"} {
		require.Containsf(t, text, want, "the register rule must name %q", want)
	}
	require.Contains(t, text, "definition of that thing rather than of the mechanism",
		"the register rule must be justified mechanically — a definition naming a "+
			"product has become a definition OF that product and stops matching")

	// The tautology ban, with its worked negative example.
	require.Contains(t, text, "a fallback that is silent")

	// The escape hatch.
	require.Contains(t, text, "A wrong definition is worse than a missing one")
}

// ── response handling ─────────────────────────────────────────────────────

func defineOffered() []motifDefineItem {
	return []motifDefineItem{
		{Name: "silent-fallback", clusterKey: "fallback-silent"},
		{Name: "config-drift", clusterKey: "config-drift"},
	}
}

func TestMotifDefineDecode_RejectsAnUnofferedName(t *testing.T) {
	res := motifDefineResult{Definitions: []motifDefinition{
		{Name: "invented-mechanism", Definition: "Something."},
	}}
	err := validateMotifDefinitions(res, defineOffered())
	require.Error(t, err)
	require.Contains(t, err.Error(), "not offered")
}

func TestMotifDefineDecode_RejectsADuplicate(t *testing.T) {
	res := motifDefineResult{Definitions: []motifDefinition{
		{Name: "config-drift", Definition: "One."},
		{Name: "config-drift", Definition: "Two."},
	}}
	err := validateMotifDefinitions(res, defineOffered())
	require.Error(t, err)
	require.Contains(t, err.Error(), "more than once")
}

// An empty definition is a legitimate answer, not a malformed one: the prompt
// asks for one rather than a guess when a name cannot be defined from itself.
func TestMotifDefineDecode_EmptyDefinitionIsAllowed(t *testing.T) {
	res := motifDefineResult{Definitions: []motifDefinition{
		{Name: "config-drift", Definition: ""},
	}}
	require.NoError(t, validateMotifDefinitions(res, defineOffered()))
}

func TestMotifDefineDecode_WrongEnvelopeKeyIsLoud(t *testing.T) {
	_, err := parseMotifDefineResponse(`{"decisions":[{"name":"x","definition":"y"}]}`)
	require.Error(t, err)
	_, err = parseMotifDefineResponse(`{"definitions":[]}`)
	require.NoError(t, err, "an empty list is a legitimate 'nothing to author'")
}

// ── apply ─────────────────────────────────────────────────────────────────

// An empty definition must leave the cluster QUEUED rather than storing an
// empty sentence that would read as defined.
func TestMotifDefineApply_EmptyDefinitionLeavesTheClusterQueued(t *testing.T) {
	ctx := context.Background()
	env := motifVocabEnv(t, 3)
	targets, err := env.svc.Motifs().ClustersNeedingDefinition(ctx, env.branch)
	require.NoError(t, err)
	require.NotEmpty(t, targets)

	offered := []motifDefineItem{{Name: targets[0].Name, clusterKey: targets[0].ClusterKey}}
	res := motifDefineResult{Definitions: []motifDefinition{{Name: targets[0].Name, Definition: "   "}}}
	require.NoError(t, applyMotifDefinitions(ctx, env.deps(), env.branch, res, offered))

	after, err := env.svc.Motifs().ClustersNeedingDefinition(ctx, env.branch)
	require.NoError(t, err)
	var stillQueued bool
	for _, c := range after {
		if c.ClusterKey == targets[0].ClusterKey {
			stillQueued = true
		}
	}
	require.True(t, stillQueued,
		"an empty answer must not count as defined — the cluster has to come back around")
}

// Routing is by CLUSTER KEY, not by name. The representative can flip between
// an item being rendered and answered, and storing by name would then write the
// sentence against a cluster nobody asked about.
func TestMotifDefineApply_RoutesByClusterKeyNotName(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	// "write-atomic" leads on df initially, so the representative NAME
	// ("write-atomic") differs from the cluster KEY (the sorted stemmed form,
	// "atomic-write"). That difference is the whole point of the fixture: an
	// earlier version had "atomic-write" lead, making name and key the
	// identical string, so routing by either stored to the same place and the
	// test passed with the routing sabotaged.
	env.writeFactWithMotifs("kb/a.md", "A", "body", []string{"write-atomic"})
	env.writeFactWithMotifs("kb/b.md", "B", "body", []string{"write-atomic"})
	env.writeFactWithMotifs("kb/c.md", "C", "body", []string{"atomic-write"})
	require.NoError(t, env.svc.Motifs().RebuildAliases(ctx, env.branch))

	targets, err := env.svc.Motifs().ClustersNeedingDefinition(ctx, env.branch)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	require.Equal(t, "write-atomic", targets[0].Name)
	require.NotEqual(t, targets[0].Name, targets[0].ClusterKey,
		"precondition: name and key must differ, or this test cannot tell them apart")
	offered := []motifDefineItem{{Name: targets[0].Name, clusterKey: targets[0].ClusterKey}}

	// Usage shifts so the representative flips BEFORE the answer arrives.
	env.writeFactWithMotifs("kb/d.md", "D", "body", []string{"atomic-write"})
	env.writeFactWithMotifs("kb/e.md", "E", "body", []string{"atomic-write"})
	require.NoError(t, env.svc.Motifs().RebuildAliases(ctx, env.branch))

	res := motifDefineResult{Definitions: []motifDefinition{
		{Name: targets[0].Name, Definition: "A write becomes visible in full or not at all."},
	}}
	require.NoError(t, applyMotifDefinitions(ctx, env.deps(), env.branch, res, offered))

	def, ok, err := env.svc.Motifs().Definition(ctx, env.branch, targets[0].ClusterKey)
	require.NoError(t, err)
	require.True(t, ok,
		"the definition must land on the cluster that was ASKED about, even though its "+
			"representative spelling changed while the item was outstanding")
	require.Contains(t, def, "visible in full")
}

// ── the register conformance test ─────────────────────────────────────────

// TestMotifDefine_RegisterRejectsCarrierEntityNames plants a corpus whose
// carriers name a product, then asserts the stored definition does not.
//
// SCOPE, stated so a green run is not over-read: this checks the DETECTABLE
// half. A test can verify that a definition contains no entity name from a
// planted fixture; it cannot verify that a definition is genuinely general,
// because "generic" is not a property a string carries. The prompt is the guard
// for the rest, and whether a given model honours it is an eval question for
// the §8 harness, not a conformance one.
func TestMotifDefine_RegisterRejectsCarrierEntityNames(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	env.writeFactWithMotifs("kb/a.md", "Postgres connection pool exhausts under retry storms",
		"body", []string{"pool-exhaustion"})
	require.NoError(t, env.svc.Motifs().RebuildAliases(ctx, env.branch))

	targets, err := env.svc.Motifs().ClustersNeedingDefinition(ctx, env.branch)
	require.NoError(t, err)
	require.Len(t, targets, 1)

	// The blindness guard is what makes this achievable: the payload never
	// carried "Postgres", so a compliant writer could not have used it.
	sess, err := env.svc.Pipeline().CreatePipelineSession(ctx, "review", env.branch)
	require.NoError(t, err)
	require.NoError(t, planMotifDefineWork(ctx, env.deps(), sess, env.branch))
	item, err := env.svc.Pipeline().NextPipelineWorkItem(ctx, sess.ID)
	require.NoError(t, err)
	require.NotContains(t, strings.ToLower(item.FactsJSON), "postgres",
		"the carrier's entity reached the payload — the register rule then depends on "+
			"the model declining to use what it was handed")

	offered, err := motifDefineItemsFromPayload(item.FactsJSON)
	require.NoError(t, err)
	res := motifDefineResult{Definitions: []motifDefinition{
		{Name: offered[0].Name, Definition: "A bounded resource is fully consumed, so new work cannot proceed."},
	}}
	require.NoError(t, applyMotifDefinitions(ctx, env.deps(), env.branch, res, offered))

	def, ok, err := env.svc.Motifs().Definition(ctx, env.branch, targets[0].ClusterKey)
	require.NoError(t, err)
	require.True(t, ok)
	require.NotContains(t, strings.ToLower(def), "postgres")
}

// ── §3.3 metrics reaching their surfaces ──────────────────────────────────

// The reflect payload carries recurrence and mint-to-link (§3.3). Reflect is
// where the corpus reasons about its own habits, and "are we naming the same
// mechanisms as each other" is one.
func TestMotifVocabulary_ReachesTheReflectPayload(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	env.writeFactWithMotifs("kb/a.md", "A", "body", []string{"silent-fallback"})
	env.writeFactWithMotifs("kb/b.md", "B", "body", []string{"silent-fallback"})
	env.writeFactWithMotifs("kb/c.md", "C", "body", []string{"config-drift"})
	require.NoError(t, env.svc.Motifs().RebuildAliases(ctx, env.branch))

	section := motifVocabularySection(ctx, env.deps(), env.branch)
	require.Contains(t, section, "motif clusters")
	require.Contains(t, section, "Recurrence")
	require.Contains(t, section, "Mint-to-link")

	content, err := RenderReflectWorkItem([]byte(`[{"path":"kb/hyp/a.md"}]`), "kb", "", section)
	require.NoError(t, err)
	require.Contains(t, content.Prompt, "Recurrence",
		"the reflect prompt must carry the vocabulary metrics when there is a vocabulary")
}

// A corpus with no motif vocabulary gets NO section — not a section of zeroes.
// "0 clusters, recurrence 0%" invites the model to reason about a mechanism the
// corpus is not using.
func TestMotifVocabulary_AbsentSectionOnAMotiflessCorpus(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 3) // plain facts, no motifs
	require.NoError(t, env.svc.Motifs().RebuildAliases(ctx, env.branch))

	section := motifVocabularySection(ctx, env.deps(), env.branch)
	require.Empty(t, section)

	content, err := RenderReflectWorkItem([]byte(`[{"path":"kb/hyp/a.md"}]`), "kb", "", section)
	require.NoError(t, err)
	require.NotContains(t, content.Prompt, "Recurrence")
	require.NotContains(t, content.Prompt, "motif clusters",
		"a motif-free corpus must see no motif section at all")
}

// The metrics also land in the session's health lines, alongside every other
// producer's — appended, per TestHealthRecorders_NeverDestroyExistingLines.
func TestMotifVocabulary_ReachesTheHealthLines(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	env.writeFactWithMotifs("kb/a.md", "A", "body", []string{"silent-fallback"})
	env.writeFactWithMotifs("kb/b.md", "B", "body", []string{"silent-fallback"})
	require.NoError(t, env.svc.Motifs().RebuildAliases(ctx, env.branch))

	sess, err := env.svc.Pipeline().CreatePipelineSession(ctx, "review", env.branch)
	require.NoError(t, err)
	sess.Health = []string{"a line from another producer"}
	require.NoError(t, planMotifDefineWork(ctx, env.deps(), sess, env.branch))

	joined := strings.Join(sess.Health, "\n")
	require.Contains(t, joined, "motif vocabulary:")
	require.Contains(t, joined, "recurrence")
	require.Contains(t, joined, "a line from another producer",
		"the vocabulary lines must be appended, not substituted")
}
