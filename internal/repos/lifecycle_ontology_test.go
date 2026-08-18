package repos

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// Finding 2 regression: resolveOntology's extraction (sharing initLocal's
// ontology-field-reading logic with initInitialize) originally reordered the
// precedence from "gated on Mode" to "gated on which field is non-empty",
// which silently changed two reachable initLocal behaviours. This test pins
// the first: mode=preset with BOTH ontology_preset and ontology_yaml set
// must still use the PRESET — the original switch's `case Mode == "custom"`
// arm meant a preset request never even looked at ontology_yaml, regardless
// of whether the field happened to be populated (e.g. a client that reuses
// one request struct across mode changes without clearing it).
func TestCreate_PresetMode_IgnoresOntologyYAMLWhenBothFieldsSet(t *testing.T) {
	m := newLifecycleManager(t)

	// A validly-parseable YAML with a DIFFERENT id ("general") than the
	// "code" preset ("source-code"), so the assertion below distinguishes
	// which one actually got used.
	defaultYAML, err := fact.DefaultOntology().Serialize()
	require.NoError(t, err)

	ri, err := m.Create(context.Background(), CreateSpec{
		Name: "presetwins", Mode: "preset",
		OntologyPreset: "code",
		OntologyYAML:   string(defaultYAML),
	}, nil)
	require.NoError(t, err)
	require.Equal(t, "source-code", ri.Ontology().ID,
		"mode=preset must use the preset even when ontology_yaml is also set")
}

// Finding 2 regression, second reachable behaviour: mode=custom with an
// EMPTY ontology_yaml must still be a hard, surfaced error — the original
// switch's `case Mode == "custom"` arm always called ParseOntology(""),
// which fails validation ("id is required" and friends). A field-gated
// rewrite that only parses when OntologyYAML != "" would instead fall
// through silently to fact.DefaultOntology(), creating a repo the caller
// never asked for the ontology of, with no indication anything went wrong.
func TestCreate_CustomMode_EmptyOntologyYAMLIsHardError(t *testing.T) {
	m := newLifecycleManager(t)

	_, err := m.Create(context.Background(), CreateSpec{
		Name: "customempty", Mode: "custom", OntologyYAML: "",
	}, nil)
	require.Error(t, err, "mode=custom with an empty ontology_yaml must fail, not silently default")
	require.Nil(t, m.Get("customempty"), "a failed custom-mode create must leave no repo registered")
}
