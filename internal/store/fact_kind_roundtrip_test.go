package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// pragmaticFactBody builds a serialized pragmatic policy fact body for tests.
// Uses fact.SerializeFact so the YAML matches what the parser expects.
func pragmaticFactBody(title, body string, domains, entities []string) string {
	f := fact.NewFact("placeholder.md")
	f.Title = title
	f.Body = body
	f.Kind = fact.Pragmatic
	f.Type = fact.Policy
	f.Confidence = 0.9
	f.Sources = 1
	f.Domain = domains
	f.Entities = entities
	out, err := fact.SerializeFact(f)
	if err != nil {
		panic(err)
	}
	return out
}

// TestFactRecord_KindRoundTripsThroughSQL_Pragmatic regresses the storage
// backbone for fact.Kind: a pragmatic/policy fact written via WriteFact must
// be retrievable via GetByPath with Kind preserved end-to-end (fact.Fact →
// FactRecord → SQL row → FactWithBody.Kind).
func TestFactRecord_KindRoundTripsThroughSQL_Pragmatic(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/a"))

	ctx := context.Background()
	const path = "kb/policy/use-tls.md"
	_, err = svc.Facts().WriteFact(ctx, "agent/a", path,
		pragmaticFactBody("Use TLS", "All cross-host traffic must use TLS 1.3+.",
			[]string{"security"}, []string{"Anthropic"}),
		"add", "")
	require.NoError(t, err)

	got, err := svc.Search().GetByPath(ctx, "agent/a", path)
	require.NoError(t, err)
	require.NotNil(t, got, "fact must be readable via path")
	require.Equal(t, "pragmatic", got.Kind, "Kind must round-trip through SQL")
	require.Equal(t, "policy", got.Type, "Type must round-trip alongside Kind")
}

// TestFactRecord_KindRoundTripsThroughSQL_EpistemicDefault regresses the
// defaulting boundary: an epistemic fact (the default kind) must surface as
// "epistemic" on read, never as the empty string. This catches a regression
// where Kind=="" would leak through the in-memory FactRecord even though the
// SQL column DEFAULT papers over it on INSERT.
func TestFactRecord_KindRoundTripsThroughSQL_EpistemicDefault(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/a"))

	ctx := context.Background()
	const path = "kb/obs/x.md"
	_, err = svc.Facts().WriteFact(ctx, "agent/a", path,
		testFactBody("X", 0.9, nil),
		"add", "")
	require.NoError(t, err)

	got, err := svc.Search().GetByPath(ctx, "agent/a", path)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "epistemic", got.Kind, "default kind must canonicalize to \"epistemic\" on read")
	require.Equal(t, "observation", got.Type)
}
