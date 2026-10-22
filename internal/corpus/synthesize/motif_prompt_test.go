package synthesize

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The motif prompts live in this package (prompts/large/motif_*_user.txt,
// rendered by RenderMotifAliasWorkItem and RenderMotifDefineWorkItem), so the
// tests that read them stay beside them; the decode/apply/selection tests
// moved with their code into the motif child.

// TestMotifAliasPrompt_NamesEveryNearMissConfusion — the prompt must name each
// confusion class explicitly. A generic "be careful" does not discriminate
// them; these are the adjacent-family failures §12-E3 measured.
//
// This checks the PROMPT, not a model. Whether a given model resists each
// confusion is an eval question (§8 harness); whether we asked it to is a
// conformance question, and it is the half we control.
func TestMotifAliasPrompt_NamesEveryNearMissConfusion(t *testing.T) {
	body, err := os.ReadFile("prompts/large/motif_alias_user.txt")
	require.NoError(t, err)
	text := strings.ToLower(string(body))

	for _, want := range []string{
		"cause and its effect",
		"family and one member",
		"co-occur",
		"broader",
		"problem",
		"remedy",
	} {
		require.Containsf(t, text, want,
			"the prompt must name the %q confusion — a generic warning does not "+
				"discriminate the adjacent-family failures this task actually hits", want)
	}
}

// The asymmetry must be stated as a COST, not just as a rule. Telling a model
// to be conservative produces mild conservatism; telling it why the two errors
// differ in kind holds up better — and it is true, which is the part that
// matters.
func TestMotifAliasPrompt_StatesTheAsymmetryAsACost(t *testing.T) {
	body, err := os.ReadFile("prompts/large/motif_alias_user.txt")
	require.NoError(t, err)
	text := string(body)
	require.Contains(t, text, "DEFAULT TO \"different\"")
	require.Contains(t, text, "no cost to leaving two names separate")
	require.Contains(t, text, "nothing downstream can tell it happened")
	require.Contains(t, text, "READ THE CARRIER TITLES")
}

// The named mechanism is the merge TEST, not a field filled in afterwards.
func TestMotifAliasPrompt_MakesTheMechanismSentenceTheTest(t *testing.T) {
	body, err := os.ReadFile("prompts/large/motif_alias_user.txt")
	require.NoError(t, err)
	require.Contains(t, string(body), "If you cannot write that sentence, the answer is \"different\"")
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
