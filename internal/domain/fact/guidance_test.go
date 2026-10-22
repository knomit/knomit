package fact

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// F23: ontology-declared guidance, by path.

const guidanceOntology = `id: x
name: X
guidance:
  hypothesize: guidance/all-hypothesize.md
  review: guidance/all-review.md
validations:
  - name: root-rule
    message: Every fact needs a title.
    rule: "fact.title.length > 0"
topics:
  forecast:
    description: Forecasts.
    guidance:
      hypothesize: guidance/forecast-hypothesize.md
      review: guidance/forecast-review.md
    validations:
      - name: forecast-has-settlement
        message: A hypothesis needs a "Settlement:" line.
        rule: "fact.type !== 'hypothesis' || /\\nSettlement: /.test('\\n' + fact.body)"
    children:
      markets:
        description: Market forecasts.
        validations:
          - name: markets-rule
            message: Name the market.
            rule: "true"
        children:
          rates:
            description: Rates.
            guidance:
              hypothesize: guidance/rates-hypothesize.md
  plain:
    description: No guidance of its own.
`

func guidancePaths(rs []GuidanceRef) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Path)
	}
	return out
}

func TestGuidance_InheritanceAndOverride(t *testing.T) {
	o := mustParse(t, guidanceOntology)
	_, err := ParseNewOntology([]byte(guidanceOntology))
	require.NoError(t, err, "a valid guidance block is accepted for a new ontology")

	// Declared topic: root first, then its own.
	require.Equal(t, []string{"guidance/all-hypothesize.md", "guidance/forecast-hypothesize.md"},
		guidancePaths(o.GuidanceFor("forecast", GuidanceHypothesize)))
	// Declared child without a block inherits the parent's.
	require.Equal(t, []string{"guidance/all-hypothesize.md", "guidance/forecast-hypothesize.md"},
		guidancePaths(o.GuidanceFor("forecast/markets", GuidanceHypothesize)))
	// Undeclared grandchild inherits from its deepest declared ancestor.
	require.Equal(t, []string{"guidance/all-hypothesize.md", "guidance/forecast-hypothesize.md"},
		guidancePaths(o.GuidanceFor("forecast/markets/undeclared/deeper", GuidanceHypothesize)))
	// A child override wins for its key; the other key still inherits.
	require.Equal(t, []string{"guidance/all-hypothesize.md", "guidance/rates-hypothesize.md"},
		guidancePaths(o.GuidanceFor("Forecast/Markets/Rates/x", GuidanceHypothesize)))
	require.Equal(t, []string{"guidance/all-review.md", "guidance/forecast-review.md"},
		guidancePaths(o.GuidanceFor("forecast/markets/rates", GuidanceReview)))
	// A topic with no block, and an undeclared topic, get the root's only.
	require.Equal(t, []string{"guidance/all-hypothesize.md"}, guidancePaths(o.GuidanceFor("plain", GuidanceHypothesize)))
	require.Equal(t, []string{"guidance/all-hypothesize.md"}, guidancePaths(o.GuidanceFor("nope/x", GuidanceHypothesize)))
	require.Equal(t, []string{"guidance/all-hypothesize.md"}, guidancePaths(o.GuidanceFor("", GuidanceHypothesize)))

	r := o.GuidanceFor("forecast/markets/rates", GuidanceHypothesize)
	require.Equal(t, "", r[0].Scope)
	require.Equal(t, "forecast/markets/rates", r[1].Scope)
	require.Equal(t, ".knomit/guidance/rates-hypothesize.md", r[1].File)

	require.Equal(t, map[string][]string{
		"guidance/all-hypothesize.md":      {"root"},
		"guidance/all-review.md":           {"root"},
		"guidance/forecast-hypothesize.md": {"forecast"},
		"guidance/forecast-review.md":      {"forecast"},
		"guidance/rates-hypothesize.md":    {"forecast/markets/rates"},
	}, o.GuidanceDeclarations())
}

func TestGuidance_RootAndTopicSameFileOnce(t *testing.T) {
	o := mustParse(t, `id: x
name: X
guidance:
  review: guidance/r.md
topics:
  a:
    description: A.
    guidance:
      review: guidance/r.md
`)
	require.Equal(t, []string{"guidance/r.md"}, guidancePaths(o.GuidanceFor("a", GuidanceReview)))
}

func TestGuidance_DeclaredTopic(t *testing.T) {
	o := mustParse(t, guidanceOntology)
	require.Equal(t, "forecast/markets", o.DeclaredTopic("Forecast/markets/IGNORE-PREVIOUS/x"))
	require.Equal(t, "", o.DeclaredTopic("undeclared/x"))
	require.Equal(t, "", o.DeclaredTopic(""))
	var nilO *Ontology
	require.Equal(t, "", nilO.DeclaredTopic("forecast"))
	require.Nil(t, nilO.GuidanceFor("forecast", GuidanceReview))
}

func TestGuidance_ValidationsForWalk(t *testing.T) {
	o := mustParse(t, guidanceOntology)
	names := func(vs []Validation) []string {
		var out []string
		for _, v := range vs {
			out = append(out, v.Name)
		}
		return out
	}
	require.Equal(t, []string{"root-rule", "forecast-has-settlement", "markets-rule"},
		names(o.ValidationsFor("forecast/markets/undeclared")))
	require.Equal(t, []string{"root-rule", "forecast-has-settlement"}, names(o.ValidationsFor("forecast")))
	require.Equal(t, []string{"root-rule"}, names(o.ValidationsFor("plain")))
	require.Equal(t, []string{"root-rule"}, names(o.ValidationsFor("")))
}

// Every bad value is a warning on the open path and fatal for a new
// ontology, and resolves to no file for that key — failing closed: the
// parent's guidance is NOT inherited in its place.
func TestGuidance_BadValuesWarnRefuseAndFailClosed(t *testing.T) {
	cases := map[string]string{
		"escape":      `"../ontology.yaml"`,
		"absolute":    `"/etc/x"`,
		"dotdot":      `"guidance/../../x"`,
		"backslash":   `"guidance\\x.md"`,
		"not-clean":   `"guidance/./x.md"`,
		"outside-dir": `"skills/x.md"`,
		"dir-only":    `"guidance/"`,
		"empty":       `""`,
		"number":      `7`,
		"list":        `[guidance/x.md]`,
		"inline-text": "|\n            Write each hypothesis under forecast.\n            Body: Claim, Settlement, Window.",
		"too-long":    `"guidance/` + strings.Repeat("a", 260) + `.md"`,
	}
	for name, val := range cases {
		t.Run(name, func(t *testing.T) {
			y := `id: x
name: X
topics:
  forecast:
    description: F.
    guidance:
      hypothesize: guidance/parent.md
    children:
      child:
        description: C.
        guidance:
          hypothesize: ` + val + `
`
			o, diags := ValidateOntologyYAML([]byte(y))
			require.NotNil(t, o)
			var warned bool
			for _, d := range diags {
				require.False(t, d.IsError(), "never an error on the open path: %s", d.Message)
				if strings.Contains(d.Message, `guidance in topic "forecast/child"`) {
					warned = true
					require.LessOrEqual(t, len(d.Message), 400, "a refused paragraph is not echoed whole")
				}
			}
			require.True(t, warned, "a warning names the topic: %v", diags)

			o2, err := ParseOntology([]byte(y))
			require.NoError(t, err)
			_, err = ParseNewOntology([]byte(y))
			require.Error(t, err)
			require.Contains(t, err.Error(), "guidance")

			require.Empty(t, o2.GuidanceFor("forecast/child", GuidanceHypothesize),
				"a refused value resolves to nothing and shadows the parent")
			require.Equal(t, []string{"guidance/parent.md"}, guidancePaths(o2.GuidanceFor("forecast", GuidanceHypothesize)))
			require.NotContains(t, o2.GuidanceDeclarations(), "")
		})
	}
}

func TestGuidance_UnknownKeyAndNonMapping(t *testing.T) {
	for name, block := range map[string]string{
		"unknown-key": "\n  distill: guidance/d.md\n  review: guidance/r.md",
		"scalar":      " guidance/r.md",
		"list":        "\n  - guidance/r.md",
	} {
		t.Run(name, func(t *testing.T) {
			y := "id: x\nname: X\nguidance:" + block + "\ntopics:\n  a:\n    description: A.\n"
			o, err := ParseOntology([]byte(y))
			require.NoError(t, err, "a newer ontology still loads on this binary")
			_, err = ParseNewOntology([]byte(y))
			require.Error(t, err)
			if name == "unknown-key" {
				require.Contains(t, err.Error(), `unknown key "distill"`)
				require.Equal(t, []string{"guidance/r.md"}, guidancePaths(o.GuidanceFor("a", GuidanceReview)))
				require.Empty(t, o.GuidanceFor("a", "distill"))
			} else {
				require.Empty(t, o.GuidanceFor("a", GuidanceReview))
			}
		})
	}
}

func TestGuidance_SerializeRoundTripKeepsBlocks(t *testing.T) {
	y := `id: x
name: X
guidance:
  hypothesize: guidance/all.md
  distill: guidance/future.md
topics:
  a:
    description: A.
    guidance:
      review: guidance/a.md
`
	o := mustParse(t, y)
	out, err := o.Serialize()
	require.NoError(t, err)
	s := string(out)
	require.Contains(t, s, "hypothesize: guidance/all.md")
	require.Contains(t, s, "distill: guidance/future.md", "a newer knomit's key survives")
	require.Contains(t, s, "review: guidance/a.md")
	o2 := mustParse(t, s)
	require.Equal(t, o.GuidanceDeclarations(), o2.GuidanceDeclarations())
	require.Empty(t, o.SubsetDivergence(o2))
}

// A stored ontology that differs from a preset ONLY by a guidance block (root
// or node) is not safe to overwrite: the boot refresh would erase it.
func TestGuidance_DivergenceProtectsBlock(t *testing.T) {
	preset := mustParse(t, "id: x\nname: X\ntopics:\n  a:\n    description: A.\n")
	rootOnly := mustParse(t, "id: x\nname: X\nguidance:\n  review: guidance/r.md\ntopics:\n  a:\n    description: A.\n")
	nodeOnly := mustParse(t, "id: x\nname: X\ntopics:\n  a:\n    description: A.\n    guidance:\n      review: guidance/r.md\n")
	require.Equal(t, DivergenceGuidance, rootOnly.SubsetDivergence(preset))
	require.False(t, rootOnly.IsSubsetOf(preset))
	require.Equal(t, DivergenceGuidance, nodeOnly.SubsetDivergence(preset))
	require.Empty(t, preset.SubsetDivergence(rootOnly))
	require.Empty(t, rootOnly.SubsetDivergence(rootOnly))
}

func TestGuidance_ValidGuidancePath(t *testing.T) {
	require.True(t, ValidGuidancePath("guidance/x.md"))
	require.True(t, ValidGuidancePath("guidance/sub/x.md"))
	for _, bad := range []string{"guidance", "guidance/", "../guidance/x.md", "guidance//x.md", "guidance/x.md/", "./guidance/x.md", "x.md", "guidance/a\nb.md"} {
		require.Falsef(t, ValidGuidancePath(bad), "%q", bad)
	}
}
