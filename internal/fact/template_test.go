package fact

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTemplatePathAllowed(t *testing.T) {
	for p, want := range map[string]bool{
		"README.md":                         true,
		".knomit/ontology.yaml":             true,
		".knomit/skills/work-task/SKILL.md": true,
		"readme.md":                         false,
		"LICENSE":                           false,
		".gitmodules":                       false,
		"kb/x/y.md":                         false,
		"artifacts/a/b.md":                  false,
		"docs/README.md":                    false,
		".knomit/":                          false,
		".knomitx/a":                        false,
	} {
		require.Equal(t, want, TemplatePathAllowed(p), p)
	}
}

const templateFactBody = `---
type: reference
domain: [templates]
confidence: 0.9
sources: 1
context: {part: template, template: mission}
---
# The mission template creates a mission repo

Template folder: ` + "`.knomit/templates/mission/`" + `.
`

// The description is the fact's title; a fact of another part is not a
// template description; a context name that disagrees with the folder is an
// error, never silently re-filed.
func TestParseTemplateFact(t *testing.T) {
	tf, ok, err := ParseTemplateFact("kb/templates/mission/a.md", "mission", templateFactBody)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, TemplateFact{Name: "mission", Path: "kb/templates/mission/a.md", Title: "The mission template creates a mission repo"}, tf)

	_, ok, err = ParseTemplateFact("kb/templates/other/a.md", "other", templateFactBody)
	require.Error(t, err)
	require.False(t, ok)

	file := `---
type: reference
confidence: 0.9
context: {part: file, template: mission}
---
# claims.js
`
	_, ok, err = ParseTemplateFact("kb/templates/mission/b.md", "mission", file)
	require.NoError(t, err)
	require.False(t, ok)
}
