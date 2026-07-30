package tomledit_test

import (
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"knomit/tools/desktop/internal/tomledit"
)

func set(t *testing.T, src, table, key, value string) string {
	t.Helper()
	out, err := tomledit.SetString([]byte(src), table, key, value)
	if err != nil {
		t.Fatalf("SetString(%q,%q): %v", table, key, err)
	}
	return string(out)
}

// The whole reason this package exists instead of marshalling the Config
// struct: people hand-edit knomit.toml, and a settings dialog must not eat
// their comments or the keys the struct does not model.
func TestSetStringPreservesCommentsAndUnknownKeys(t *testing.T) {
	src := `# my knomit config
[log]
# keep this comment
level = "info"
some_future_key = "untouched"
`
	got := set(t, src, "log", "level", "debug")

	if !strings.Contains(got, `level = "debug"`) {
		t.Errorf("level not updated:\n%s", got)
	}
	for _, want := range []string{"# my knomit config", "# keep this comment", `some_future_key = "untouched"`} {
		if !strings.Contains(got, want) {
			t.Errorf("lost %q:\n%s", want, got)
		}
	}
}

func TestSetStringAddsAKeyToAnExistingTable(t *testing.T) {
	got := set(t, "[log]\nlevel = \"info\"\n", "log", "format", "console")

	if !strings.Contains(got, `format = "console"`) {
		t.Errorf("key not added:\n%s", got)
	}
	if !strings.Contains(got, `level = "info"`) {
		t.Errorf("existing key lost:\n%s", got)
	}
}

func TestSetStringAddsAMissingTable(t *testing.T) {
	got := set(t, "port = \"19278\"\n", "log", "level", "debug")

	if !strings.Contains(got, "[log]") {
		t.Errorf("table not created:\n%s", got)
	}
	if !strings.Contains(got, `level = "debug"`) {
		t.Errorf("key not written:\n%s", got)
	}
}

// A root key written after a table header would silently become a key OF that
// table — the file would still parse, and mean something entirely different.
func TestSetStringPlacesARootKeyBeforeTheFirstTable(t *testing.T) {
	got := set(t, "[log]\nlevel = \"info\"\n", "", "port", "20000")

	portAt := strings.Index(got, "port =")
	tableAt := strings.Index(got, "[log]")
	if portAt < 0 {
		t.Fatalf("root key not written:\n%s", got)
	}
	if portAt > tableAt {
		t.Errorf("root key landed inside [log]; it would parse as log.port:\n%s", got)
	}
}

func TestSetStringUpdatesAnExistingRootKey(t *testing.T) {
	got := set(t, "port = \"19278\"\n\n[log]\nlevel = \"info\"\n", "", "port", "20000")

	if !strings.Contains(got, `port = "20000"`) {
		t.Errorf("root key not updated:\n%s", got)
	}
	if strings.Contains(got, `"19278"`) {
		t.Errorf("old value left behind:\n%s", got)
	}
}

// Only the target table's key may change. A key of the same name in another
// table is a different setting.
func TestSetStringDoesNotTouchASameNamedKeyInAnotherTable(t *testing.T) {
	src := "[git]\nport = \"9418\"\n\n[log]\nlevel = \"info\"\n"
	got := set(t, src, "log", "level", "debug")

	if !strings.Contains(got, `port = "9418"`) {
		t.Errorf("git.port was disturbed:\n%s", got)
	}
}

func TestSetStringIsIdempotent(t *testing.T) {
	once := set(t, "[log]\nlevel = \"info\"\n", "log", "level", "debug")
	twice := set(t, once, "log", "level", "debug")

	if once != twice {
		t.Errorf("not idempotent:\nfirst:\n%s\nsecond:\n%s", once, twice)
	}
}

func TestSetStringHandlesAnEmptyFile(t *testing.T) {
	got := set(t, "", "log", "level", "debug")

	if !strings.Contains(got, "[log]") || !strings.Contains(got, `level = "debug"`) {
		t.Errorf("empty file not populated:\n%s", got)
	}
}

// The value goes into a TOML string; a quote or backslash in it must not be
// able to terminate that string early.
func TestSetStringEscapesTheValue(t *testing.T) {
	got := set(t, "", "log", "file", `C:\logs\"odd".log`)

	out, err := tomledit.SetString([]byte(got), "log", "file", "again")
	if err != nil {
		t.Fatalf("result did not round-trip: %v\n%s", err, got)
	}
	if !strings.Contains(string(out), `"again"`) {
		t.Errorf("round-trip lost the key:\n%s", out)
	}
}

// Whatever this package writes must be loadable by the parser knomit actually
// uses, or the next launch fails on a file the settings dialog produced.
func TestSetStringOutputParsesWithBurntSushi(t *testing.T) {
	src := "# comment\nport = \"19278\"\n\n[log]\nlevel = \"info\"\n"
	out := set(t, src, "log", "format", "console")
	out = set(t, out, "", "port", "20000")

	var got struct {
		Port string `toml:"port"`
		Log  struct {
			Level  string `toml:"level"`
			Format string `toml:"format"`
		} `toml:"log"`
	}
	if _, err := toml.Decode(out, &got); err != nil {
		t.Fatalf("output does not parse: %v\n%s", err, out)
	}
	if got.Port != "20000" || got.Log.Level != "info" || got.Log.Format != "console" {
		t.Errorf("round-trip mismatch: %+v\n%s", got, out)
	}
}
