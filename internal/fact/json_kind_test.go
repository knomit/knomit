package fact

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFact_JSON_RoundTrip_Pragmatic(t *testing.T) {
	f := NewFact("kb/p.md")
	f.Title = "Rotate secrets quarterly"
	f.Body = "Policy body."
	f.Kind = Pragmatic
	f.Type = Policy
	f.Domain = []string{"security"}
	f.Confidence = 0.9
	f.Entities = []string{}
	f.Refs = []string{}

	out, err := json.Marshal(f)
	require.NoError(t, err)
	require.Contains(t, string(out), `"kind":"pragmatic"`)
	require.Contains(t, string(out), `"type":"policy"`)

	var got Fact
	require.NoError(t, json.Unmarshal(out, &got))
	require.Equal(t, Pragmatic, got.Kind)
	require.Equal(t, Policy, got.Type)
	require.Equal(t, f.Title, got.Title)
}

func TestFact_JSON_OmitsKindWhenEpistemic(t *testing.T) {
	f := NewFact("kb/e.md")
	f.Title = "Login latency spike"
	f.Kind = Epistemic
	f.Type = Observation
	f.Domain = []string{}
	f.Entities = []string{}
	f.Refs = []string{}

	out, err := json.Marshal(f)
	require.NoError(t, err)
	require.NotContains(t, string(out), `"kind"`,
		"epistemic is the default; kind should be omitempty in JSON")
}

func TestFact_JSON_UnmarshalDefaultsMissingKind(t *testing.T) {
	const in = `{"path":"kb/x.md","title":"X","body":"b","type":"observation","domain":[],"entities":[],"refs":[],"confidence":0,"sources":0}`
	var f Fact
	require.NoError(t, json.Unmarshal([]byte(in), &f))
	require.Equal(t, Epistemic, f.Kind,
		"unmarshalling a fact with no kind should default to epistemic")
}
