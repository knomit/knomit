package fact

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCorpus_RoundTripIdentical ensures every fact file shipped in the repo
// parses, serializes, and produces byte-identical output. Adding the kind
// discriminator must not perturb any existing on-disk fact.
func TestCorpus_RoundTripIdentical(t *testing.T) {
	// Walk these roots relative to the test's working directory
	// (Go runs tests from the package directory). If a root does not
	// exist, it's silently skipped.
	roots := []string{"testdata"}

	for _, root := range roots {
		info, err := os.Stat(root)
		if err != nil || !info.IsDir() {
			continue
		}
		_ = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
			if err != nil || fi.IsDir() || !strings.HasSuffix(p, ".md") {
				return nil
			}
			raw, err := os.ReadFile(p)
			require.NoError(t, err)
			f, err := ParseFact(p, string(raw))
			require.NoErrorf(t, err, "ParseFact %s", p)
			out, err := SerializeFact(f)
			require.NoErrorf(t, err, "SerializeFact %s", p)
			require.Equalf(t, string(raw), out, "round-trip diff in %s", p)
			return nil
		})
	}
}
