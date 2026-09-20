package apppaths_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// e2e/helpers/appdirs.ts is a hand-written TypeScript copy of this package's
// per-OS rules: the e2e harness runs under Node, cannot call Go, and needs the
// real data root for one thing — locating the cached ONNX model to copy into
// each run's temp KNOMIT_HOME. A miss there is silent and costs 617MB per run,
// which is how the previous version (process.env.HOME, unset on Windows) went
// unnoticed.
//
// A comment saying "keep in step with internal/apppaths" enforces nothing, so
// this reads the file and pins the literals. It is a TEXT check, not a
// behavioural one — it cannot prove the two agree, only that nobody changed one
// side's spelling without touching the other. That is the failure worth
// catching: a rename here that leaves the harness silently pointing at the old
// directory. Same technique as tools/desktop's windows_test.go, which greps
// vite.config.ts for entry documents.
func TestAppDirsTS_MatchesThisPackage(t *testing.T) {
	path := filepath.Join("..", "..", "e2e", "helpers", "appdirs.ts")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	src := string(raw)

	// Each want is a substring the TS must contain for its resolution to match
	// ours. Spelled as the literal fragments rather than a parse, because a
	// parser here would be a second thing to keep in step.
	for _, want := range []string{
		`'knomit', 'home'`,         // Windows: <LOCALAPPDATA>\knomit\home — appDir + homeSubdir
		`'.knomit'`,                // non-Windows: ~/.knomit
		`process.env.LOCALAPPDATA`, // same variable apppaths_windows.go reads
		`'AppData', 'Local'`,       // same reconstruction when it is unset
		`homedir()`,                // os.UserHomeDir's Node equivalent
		`=== 'win32'`,              // the branch itself
	} {
		if !strings.Contains(src, want) {
			t.Errorf("%s does not contain %q — it has drifted from internal/apppaths", path, want)
		}
	}

	// `??` would treat an empty-but-set LOCALAPPDATA as a value and yield the
	// relative "knomit\home". The Go side treats empty as absent; so must this.
	if strings.Contains(src, "process.env.LOCALAPPDATA ??") {
		t.Error("appdirs.ts uses `??` on LOCALAPPDATA: an empty-but-set value would " +
			"resolve to a relative path. Use `||`, as apppaths_windows.go does.")
	}
}
