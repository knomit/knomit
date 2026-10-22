package archtest

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFallbackSignerOnlyReferencedFromTests: storetest.SetFallbackSigner lets a
// test binary commit through a Service that has no signer. Production must
// never reach it: a signer-less production Service fails with ErrNoSigner at
// its first write, and that is the whole point of F09 PR 2. The hook already
// refuses to work outside a test binary (testing.Testing); this test keeps
// production SOURCE from even naming it or its installer package.
//
// Allowed non-test files: the hook's own definition (the store's vcs child,
// internal/engine/store/internal/vcs/sign.go), its one door for tests outside the
// store (storetest.SetFallbackSigner, internal/engine/store/storetest/storetest.go,
// which is itself kept out of every shipped binary and out of production
// imports by TestHarnessNotLinkedIntoShippedBinaries and
// TestTestSeamsNotReachedFromProduction), and the installer package
// (internal/testsupport/testsigner).
func TestFallbackSignerOnlyReferencedFromTests(t *testing.T) {
	def := filepath.Join("internal", "engine", "store", "internal", "vcs", "sign.go")
	fwd := filepath.Join("internal", "engine", "store", "storetest", "storetest.go")
	requireNaming(t, def, "SetTestFallbackSigner")
	requireNaming(t, fwd, "SetTestFallbackSigner")
	offenders := productionSourceNaming(t,
		[]string{"SetTestFallbackSigner", "internal/testsupport/testsigner"},
		def, fwd,
		filepath.Join("internal", "testsupport", "testsigner", "testsigner.go"),
	)
	if len(offenders) > 0 {
		t.Fatalf("production source must not reference the test fallback signer:\n  %s", strings.Join(offenders, "\n  "))
	}
}

// TestFallbackAgentIDOnlyInstalledFromTests: storetest.SetFallbackAgentID lets
// a test binary author commits as a fixture id derived from a branch name.
// Production never derives an agent id from a name (D18): a store given no id
// fails with ErrNoAgentID. The setter refuses to work outside a test binary;
// this keeps production SOURCE from naming it (or the fallback's installer)
// at all. There is no exported READ side: since E7a repos.Manager.AgentID
// answers Deps.AgentID alone, and the fallback is read only by
// vcs.Repo.OwnAgentID inside the store.
func TestFallbackAgentIDOnlyInstalledFromTests(t *testing.T) {
	// The setter lives in the store's vcs child; tests outside the store reach
	// it through storetest.SetFallbackAgentID (storetest.go).
	def := filepath.Join("internal", "engine", "store", "internal", "vcs", "agent_id.go")
	fwd := filepath.Join("internal", "engine", "store", "storetest", "storetest.go")
	requireNaming(t, def, "SetTestFallbackAgentID")
	requireNaming(t, fwd, "SetTestFallbackAgentID")
	offenders := productionSourceNaming(t,
		[]string{"SetTestFallbackAgentID", "FixtureAgentID"},
		def, fwd,
		filepath.Join("internal", "testsupport", "testsigner", "testsigner.go"),
	)
	if len(offenders) > 0 {
		t.Fatalf("production source must not install the test fallback agent id:\n  %s", strings.Join(offenders, "\n  "))
	}
}

// TestFastDurabilityOnlyReferencedFromTests: storetest.SetFastDurability turns
// SQLite's fsyncs off (synchronous=OFF) for a whole test binary (#365). In a
// production knomit that would turn a power cut into a corrupt store or
// control.db, so the switch is set only from a test package's TestMain. Like
// the signer hook it refuses to work outside a test binary; this test keeps
// production SOURCE from naming it at all.
//
// The READ side, store.SyncDSNParam, is deliberately not a needle: production
// opens of control.db call it, and outside a test binary it returns "".
//
// The needles are the setter and the atomic it writes (testFastDurability),
// so production cannot bypass the setter within package store either.
//
// Allowed non-test file: the switch's own definition. Every needle must
// appear THERE, or a rename would leave this test passing on nothing.
func TestFastDurabilityOnlyReferencedFromTests(t *testing.T) {
	needles := []string{"SetTestFastDurability", "testFastDurability"}
	// The switch lives in the store's sqlite child (the atomic is unexported
	// there, so nothing else in package store can reach it); the parent only
	// forwards the setter under its public name for the test packages that call
	// storetest.SetFastDurability. Both files may name it; every needle must
	// appear in the definition, and the forwarder must still name the setter.
	def := filepath.Join("internal", "engine", "store", "internal", "sqlite", "test_durability.go")
	fwd := filepath.Join("internal", "engine", "store", "storetest", "storetest.go")
	src, err := os.ReadFile(filepath.Join("..", "..", def))
	if err != nil {
		t.Fatalf("read the switch's definition file: %v", err)
	}
	for _, n := range needles {
		if !strings.Contains(string(src), n) {
			t.Fatalf("%s no longer contains %q: the switch was renamed or moved, so this guard checks nothing — update its needles", def, n)
		}
	}
	fsrc, err := os.ReadFile(filepath.Join("..", "..", fwd))
	if err != nil {
		t.Fatalf("read the switch's storetest door: %v", err)
	}
	if !strings.Contains(string(fsrc), "SetTestFastDurability") {
		t.Fatalf("%s no longer calls SetTestFastDurability: the switch's door moved, so this guard's allowlist is stale", fwd)
	}
	offenders := productionSourceNaming(t, needles, def, fwd)
	if len(offenders) > 0 {
		t.Fatalf("production source must not reference the test durability switch:\n  %s", strings.Join(offenders, "\n  "))
	}
}

// requireNaming fails unless the module-root-relative file names needle: an
// allowlisted file that no longer holds the hook means it was renamed or moved
// and the guard would check nothing.
func requireNaming(t *testing.T, file, needle string) {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "..", file))
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	if !strings.Contains(string(src), needle) {
		t.Fatalf("%s no longer contains %q: the hook was renamed or moved, so this guard checks nothing — update its allowlist", file, needle)
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
