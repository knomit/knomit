package mcp

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// TestFactSchema_DescriptionsAreComplete is what makes internal/fact the
// single source of truth for the classification vocabulary rather than merely
// one of several copies of it.
//
// internal/fact owns WHICH values exist; internal/mcp owns HOW they are
// described to an LLM tool-caller. That split only holds if adding a value on
// one side is forced to surface on the other. Without this test, a new leaf
// type added to fact.AllEpistemicTypes() would silently ship with an empty
// gloss: the enum would accept it, the description would never mention it, and
// no agent would ever choose it — exactly the drift this design replaced.
func TestFactSchema_DescriptionsAreComplete(t *testing.T) {
	t.Run("types", func(t *testing.T) {
		for _, ty := range allTypes() {
			doc, ok := factTypeDocs[ty]
			require.Truef(t, ok && strings.TrimSpace(doc.Gloss) != "",
				"leaf type %q exists in internal/fact but has no description in internal/mcp.\n"+
					"Add an entry to factTypeDocs in internal/mcp/factschema.go:\n"+
					"\tfact.%s: {\"<one-clause gloss of what this type is for>\", \"<illustrative example, or \\\"\\\">\"},\n"+
					"The gloss reaches the knomit_learn/knomit_update JSON schema; the aside reaches "+
					"the server instructions. Descriptions deliberately do NOT live in internal/fact.",
				ty, strings.ToUpper(string(ty)[:1])+string(ty)[1:])
		}
		// And the converse: a description for a type internal/fact no longer
		// knows about is dead prose that would still be shown to agents.
		known := make(map[fact.Type]bool, len(allTypes()))
		for _, ty := range allTypes() {
			known[ty] = true
		}
		for ty := range factTypeDocs {
			require.Truef(t, known[ty],
				"factTypeDocs documents %q, which is not in fact.AllEpistemicTypes()+AllPragmaticTypes().\n"+
					"Either the type was removed from internal/fact (delete this entry) or it was never "+
					"registered there (add it to the authoritative set).", ty)
		}
	})

	t.Run("origins", func(t *testing.T) {
		for _, o := range fact.AllOrigins() {
			require.NotEmptyf(t, strings.TrimSpace(factOriginDocs[o]),
				"origin %q exists in fact.AllOrigins() but has no description in internal/mcp.\n"+
					"Add an entry to factOriginDocs in internal/mcp/factschema.go describing which "+
					"PIPELINE mints this origin (not where the information came from).", o)
		}
		known := make(map[fact.Origin]bool, len(fact.AllOrigins()))
		for _, o := range fact.AllOrigins() {
			known[o] = true
		}
		for o := range factOriginDocs {
			require.Truef(t, known[o],
				"factOriginDocs documents %q, which is not in fact.AllOrigins(). Delete the entry "+
					"or register the origin in internal/fact.", o)
		}
	})

	t.Run("kinds", func(t *testing.T) {
		// allKinds cannot be derived — Kind.Validate hard-codes the set — so
		// pin the two against each other from both directions.
		for _, k := range allKinds {
			require.NoErrorf(t, k.Validate(), "allKinds lists %q but fact.Kind.Validate rejects it", k)
			require.NotEmptyf(t, strings.TrimSpace(factKindDocs[k]),
				"kind %q has no description in factKindDocs (internal/mcp/factschema.go)", k)
		}
		require.Len(t, allKinds, 2,
			"fact.Kind is a closed two-member set; if a third kind was added, extend allKinds "+
				"and factKindDocs in internal/mcp/factschema.go")
	})
}

// TestFactSchema_TypeEnumIsMachineEnforced pins the behaviour change this
// refactor introduced: `type` used to be prose-only in both the knomit_learn
// and knomit_update schemas while `kind` and `origin` carried enums, so a
// typo'd leaf type reached the server and failed later in fact validation.
// It is now rejected at the protocol layer.
func TestFactSchema_TypeEnumIsMachineEnforced(t *testing.T) {
	for _, prop := range []map[string]any{
		typeProperty(fact.DefaultEpistemicType), // knomit_learn (has a default)
		typeProperty(""),                        // knomit_update (deliberately has none)
	} {
		enum, ok := prop["enum"].([]string)
		require.True(t, ok, "`type` must carry a machine-enforced enum, not prose alone")
		require.ElementsMatch(t, enumValues(allTypes()), enum,
			"the `type` enum must be exactly internal/fact's leaf types")
	}
}

// TestFactSchema_DefaultsAreCallerSupplied verifies the one axis on which the
// two consumers legitimately differ: knomit_learn mints a fact and declares
// the value it will assume, knomit_update patches one and must not — a
// "default" there would read as "omit this field and it resets".
func TestFactSchema_DefaultsAreCallerSupplied(t *testing.T) {
	learnKind, learnType := kindProperty(fact.DefaultKind), typeProperty(fact.DefaultEpistemicType)
	require.Equal(t, string(fact.DefaultKind), learnKind["default"])
	require.Equal(t, string(fact.DefaultEpistemicType), learnType["default"])

	updateKind, updateType := kindProperty(""), typeProperty("")
	require.NotContains(t, updateKind, "default", "knomit_update must not advertise a kind default")
	require.NotContains(t, updateType, "default", "knomit_update must not advertise a type default")

	// The descriptions still agree on the vocabulary itself.
	require.Equal(t, updateKind["enum"], learnKind["enum"])
	require.Equal(t, updateType["enum"], learnType["enum"])
}

// TestFactSchema_DescriptionsMentionEveryValue guards the rendered prose, not
// just the tables: an agent picks a value by reading the description, so a
// value present in the enum but absent from the sentence is unreachable in
// practice.
func TestFactSchema_DescriptionsMentionEveryValue(t *testing.T) {
	typeDesc := typeProperty(fact.DefaultEpistemicType)["description"].(string)
	for _, ty := range allTypes() {
		require.Containsf(t, typeDesc, string(ty), "`type` description must name %q", ty)
	}
	originDesc := originProperty()["description"].(string)
	for _, o := range fact.AllOrigins() {
		require.Containsf(t, originDesc, string(o), "`origin` description must name %q", o)
	}
	kindDesc := kindProperty(fact.DefaultKind)["description"].(string)
	for _, k := range allKinds {
		require.Containsf(t, kindDesc, string(k), "`kind` description must name %q", k)
	}
}
