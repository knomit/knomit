package fact

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// A fact whose title is empty or whitespace serialises as a bare "# " heading,
// which ParseFact refuses — so every reader would reject what the writer
// accepted. SerializeFact is the gate every write path shares, so it refuses.
func TestSerializeFact_RefusesBlankTitle(t *testing.T) {
	for _, title := range []string{"", "   ", "\t\n"} {
		f := NewFact("kb/x/y/aaaaaaaa.md")
		f.Title = title
		f.Body = "body"
		f.Type = Observation
		_, err := SerializeFact(f)
		require.Error(t, err, "title %q", title)
		require.True(t, errors.Is(err, ErrTitleRequired), "title %q: %v", title, err)
	}

	f := NewFact("kb/x/y/aaaaaaaa.md")
	f.Title = "A title"
	f.Type = Observation
	_, err := SerializeFact(f)
	require.NoError(t, err)
}

func TestValidateTitle(t *testing.T) {
	require.ErrorIs(t, ValidateTitle(""), ErrTitleRequired)
	require.ErrorIs(t, ValidateTitle(" \t "), ErrTitleRequired)
	require.NoError(t, ValidateTitle("x"))
}
