package archtest

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFallbackSignerOnlyReferencedFromTests: store.SetTestFallbackSigner lets a
// test binary commit through a Service that has no signer. Production must
// never reach it: a signer-less production Service fails with ErrNoSigner at
// its first write, and that is the whole point of F09 PR 2. The hook already
// refuses to work outside a test binary (testing.Testing); this test keeps
// production SOURCE from even naming it or its installer package.
//
// Allowed non-test files: the hook's own definition (internal/store/sign.go)
// and the installer package (internal/testsupport/testsigner), which is itself
// kept out of every shipped binary by TestHarnessNotLinkedIntoShippedBinaries.
func TestFallbackSignerOnlyReferencedFromTests(t *testing.T) {
	root := filepath.Join("..", "..")
	allowed := map[string]bool{
		filepath.Join("internal", "store", "sign.go"):                           true,
		filepath.Join("internal", "testsupport", "testsigner", "testsigner.go"): true,
	}
	needles := []string{"SetTestFallbackSigner", "internal/testsupport/testsigner"}
	var offenders []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "dist", "web", ".claude":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if allowed[rel] {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for _, n := range needles {
			if strings.Contains(string(b), n) {
				offenders = append(offenders, rel+" references "+n)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Fatalf("production source must not reference the test fallback signer:\n  %s", strings.Join(offenders, "\n  "))
	}
}
