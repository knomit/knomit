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
	offenders := productionSourceNaming(t,
		[]string{"SetTestFallbackSigner", "internal/testsupport/testsigner"},
		filepath.Join("internal", "store", "sign.go"),
		filepath.Join("internal", "testsupport", "testsigner", "testsigner.go"),
	)
	if len(offenders) > 0 {
		t.Fatalf("production source must not reference the test fallback signer:\n  %s", strings.Join(offenders, "\n  "))
	}
}

// TestFastDurabilityOnlyReferencedFromTests: store.SetTestFastDurability turns
// SQLite's fsyncs off (synchronous=OFF) for a whole test binary (#365). In a
// production knomit that would turn a power cut into a corrupt store or
// control.db, so the switch is set only from a test package's TestMain. Like
// the signer hook it refuses to work outside a test binary; this test keeps
// production SOURCE from naming it at all.
//
// The READ side, store.SyncDSNParam, is deliberately not a needle: production
// opens of control.db call it, and outside a test binary it returns "".
//
// Allowed non-test file: the switch's own definition.
func TestFastDurabilityOnlyReferencedFromTests(t *testing.T) {
	offenders := productionSourceNaming(t,
		[]string{"SetTestFastDurability"},
		filepath.Join("internal", "store", "test_durability.go"),
	)
	if len(offenders) > 0 {
		t.Fatalf("production source must not reference the test durability switch:\n  %s", strings.Join(offenders, "\n  "))
	}
}

// productionSourceNaming walks every non-test .go file in the module and
// returns "<file> references <needle>" for each needle a file contains,
// skipping the allowed files (module-root-relative paths).
func productionSourceNaming(t *testing.T, needles []string, allowed ...string) []string {
	t.Helper()
	root := filepath.Join("..", "..")
	allow := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		allow[a] = true
	}
	var offenders []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skipped by PATH at the module root, never by name: a name match
			// ("web") also exempted internal/web, the package the wizard fix
			// lives in.
			rel, _ := filepath.Rel(root, path)
			switch rel {
			case ".git", "web", "dist", ".claude":
				return filepath.SkipDir
			}
			if d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if allow[rel] {
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
	return offenders
}
