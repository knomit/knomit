package fact

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func expiresFixture(expires string) Fact {
	f := NewFact("kb/decisions/x/y.md")
	f.Title = "A title"
	f.Body = "A body."
	f.Type = Hypothesis
	f.Domain = []string{"x"}
	f.Entities = []string{}
	f.Refs = []string{}
	f.Confidence = 0.5
	f.Sources = 1
	f.Expires = expires
	return f
}

// TestExpires_RoundTrip: a fact carrying expires serializes it NORMALISED to
// UTC with an explicit Z at whole seconds (maintainer ruling 2026-09-28: all
// times are UTC), parses back to that string, and a second serialization is
// byte-identical. Sabotage: write the value as given (the +02:00 case keeps
// its offset).
func TestExpires_RoundTrip(t *testing.T) {
	for in, want := range map[string]string{
		"2026-10-01T00:00:00Z":      "2026-10-01T00:00:00Z",
		"2026-10-01T02:00:00+02:00": "2026-10-01T00:00:00Z",
		"2026-09-30T19:00:00-05:00": "2026-10-01T00:00:00Z",
		"2026-10-01T00:00:00.5Z":    "2026-10-01T00:00:00Z",
	} {
		out, err := SerializeFact(expiresFixture(in))
		require.NoError(t, err, in)
		require.Contains(t, out, "\nexpires: \""+want+"\"\n", "%s: expires must be written as a quoted UTC string", in)
		require.NotContains(t, out, "+02:00")

		back, err := ParseFact("kb/decisions/x/y.md", out)
		require.NoError(t, err)
		require.Equal(t, want, back.Expires)
		require.Empty(t, back.ExpiresWarnings)

		again, err := SerializeFact(back)
		require.NoError(t, err)
		require.Equal(t, out, again, "round trip must be byte-identical")
	}
}

// TestExpires_ParseKeepsAnOlderOffset: the READ side is lenient and keeps what
// a file says, so a fact written with an offset by an older build still parses
// to the same instant; the next serialization rewrites it to Z.
func TestExpires_ParseKeepsAnOlderOffset(t *testing.T) {
	src := "---\ntype: hypothesis\ndomain: [x]\nconfidence: 0.5\nsources: 1\nentities: []\nrefs: []\nexpires: \"2026-10-01T02:00:00+02:00\"\n---\n# T\n\nb\n"
	f, err := ParseFact("kb/a/b/c.md", src)
	require.NoError(t, err)
	require.Equal(t, "2026-10-01T02:00:00+02:00", f.Expires, "read: as written")
	at, ok := f.ExpiresAt()
	require.True(t, ok)
	require.True(t, at.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)))
	out, err := SerializeFact(f)
	require.NoError(t, err)
	require.Contains(t, out, "\nexpires: \"2026-10-01T00:00:00Z\"\n", "write: normalised")
}

// TestExpires_AbsentMeansNever: no expires is no key on disk, an empty field
// after parse, and never expired at any time — no default anywhere
// (principles/anti-patterns/corpus-property-constants).
func TestExpires_AbsentMeansNever(t *testing.T) {
	f := expiresFixture("")
	out, err := SerializeFact(f)
	require.NoError(t, err)
	require.NotContains(t, out, "expires")

	back, err := ParseFact("kb/decisions/x/y.md", out)
	require.NoError(t, err)
	require.Equal(t, "", back.Expires)
	_, ok := back.ExpiresAt()
	require.False(t, ok)
	require.False(t, back.IsExpired(time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)))
}

// TestExpires_SerializeRefusesNonRFC3339: the write side is strict. Date-only
// is the case that matters: yaml.v3 decodes it into time.Time without error,
// which is why the field is a string validated explicitly.
func TestExpires_SerializeRefusesNonRFC3339(t *testing.T) {
	for _, v := range []string{"2026-10-01", "tomorrow", "2026-10-01 00:00:00", "2026-10-01T00:00:00", "1790000000"} {
		_, err := SerializeFact(expiresFixture(v))
		require.Error(t, err, v)
		require.True(t, errors.Is(err, ErrInvalidExpires), "want ErrInvalidExpires for %q, got %v", v, err)
	}
}

// TestExpires_ParseIsLenient: reading a malformed value never makes the fact
// unloadable (the refs/motifs rule): it is dropped and reported.
func TestExpires_ParseIsLenient(t *testing.T) {
	src := "---\ntype: hypothesis\ndomain: [x]\nconfidence: 0.5\nsources: 1\nentities: []\nrefs: []\nexpires: 2026-10-01\n---\n# T\n\nb\n"
	f, err := ParseFact("kb/a/b/c.md", src)
	require.NoError(t, err)
	require.Equal(t, "", f.Expires)
	require.Len(t, f.ExpiresWarnings, 1)
	require.Contains(t, f.ExpiresWarnings[0], "2026-10-01")

	// Unquoted RFC 3339 (what a human types) is accepted on read.
	src = strings.Replace(src, "expires: 2026-10-01", "expires: 2026-10-01T00:00:00Z", 1)
	f, err = ParseFact("kb/a/b/c.md", src)
	require.NoError(t, err)
	require.Equal(t, "2026-10-01T00:00:00Z", f.Expires)
}

// TestExpires_IsExpiredBoundary: expired means expires <= now.
func TestExpires_IsExpiredBoundary(t *testing.T) {
	f := expiresFixture("2026-10-01T02:00:00+02:00") // == 2026-10-01T00:00:00Z
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	require.True(t, f.IsExpired(at))
	require.False(t, f.IsExpired(at.Add(-time.Second)))
	require.True(t, f.IsExpired(at.Add(time.Second)))
}

// TestFact_JSON_RoundTrip_Expires: MarshalJSON/UnmarshalJSON list their
// fields by hand; this pins that expires is among them.
func TestFact_JSON_RoundTrip_Expires(t *testing.T) {
	b, err := json.Marshal(expiresFixture("2026-10-01T00:00:00Z"))
	require.NoError(t, err)
	require.Contains(t, string(b), `"expires":"2026-10-01T00:00:00Z"`)

	var back Fact
	require.NoError(t, json.Unmarshal(b, &back))
	require.Equal(t, "2026-10-01T00:00:00Z", back.Expires)

	b, err = json.Marshal(expiresFixture(""))
	require.NoError(t, err)
	require.NotContains(t, string(b), "expires", "absent expires is omitted")
}

// TestFactToJS_ExposesExpires: rules can read the value as written.
func TestFactToJS_ExposesExpires(t *testing.T) {
	rules, err := compileRules("p", []Validation{
		{Name: "must-expire", Message: "needs expires", Rule: "typeof fact.expires === 'string' && fact.expires !== ''"},
	})
	require.NoError(t, err)
	ok, err := evaluateRule(rules[0], expiresFixture("2026-10-01T00:00:00Z"))
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = evaluateRule(rules[0], expiresFixture(""))
	require.NoError(t, err)
	require.False(t, ok)
}
