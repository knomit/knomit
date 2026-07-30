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
	// A Contains-only check can't tell "joined the existing table" apart from
	// "appended a second [log] table" — which BurntSushi rejects outright.
	if n := strings.Count(got, "[log]"); n != 1 {
		t.Errorf("expected exactly one [log] table, got %d:\n%s", n, got)
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

// Only the target table's key may change. A key of the SAME NAME in another
// table is a different setting: git.level must not be confused with
// log.level.
func TestSetStringDoesNotTouchASameNamedKeyInAnotherTable(t *testing.T) {
	src := "[git]\nlevel = \"trace\"\n\n[log]\nlevel = \"info\"\n"
	got := set(t, src, "log", "level", "debug")

	var decoded struct {
		Git struct {
			Level string `toml:"level"`
		} `toml:"git"`
		Log struct {
			Level string `toml:"level"`
		} `toml:"log"`
	}
	if _, err := toml.Decode(got, &decoded); err != nil {
		t.Fatalf("output does not parse: %v\n%s", err, got)
	}
	if decoded.Git.Level != "trace" {
		t.Errorf("git.level was disturbed: got %q:\n%s", decoded.Git.Level, got)
	}
	if decoded.Log.Level != "debug" {
		t.Errorf("log.level not updated: got %q:\n%s", decoded.Log.Level, got)
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
// able to terminate that string early. Asserted through the REAL parser, not
// through tomledit calling itself: re-running SetString on its own output
// only proves matchesKey can still find the line, not that the value survived
// as written — a quote() that did no escaping at all would still pass that
// check while producing a file BurntSushi rejects (or silently truncates).
func TestSetStringEscapesTheValue(t *testing.T) {
	hostile := `C:\logs\"odd".log`
	got := set(t, "", "log", "file", hostile)

	var decoded struct {
		Log struct {
			File string `toml:"file"`
		} `toml:"log"`
	}
	if _, err := toml.Decode(got, &decoded); err != nil {
		t.Fatalf("escaped value does not parse: %v\n%s", err, got)
	}
	if decoded.Log.File != hostile {
		t.Errorf("value mangled by escaping: got %q, want %q:\n%s", decoded.Log.File, hostile, got)
	}
}

// A `[table]` header may itself carry a trailing comment describing what the
// table is for. Failing to recognise it as a header (because it doesn't end
// in a bare "]") has two consequences: editing a key in that table appends a
// SECOND table of the same name instead of joining the first — which
// BurntSushi then refuses to parse at all — and the annotation on the header
// line must survive the edit.
func TestSetStringRecognisesATableHeaderWithATrailingComment(t *testing.T) {
	src := "[log] # logging settings\nlevel = \"info\"\n"
	got := set(t, src, "log", "level", "debug")

	if n := strings.Count(got, "[log]"); n != 1 {
		t.Errorf("expected exactly one [log] table, got %d:\n%s", n, got)
	}
	if !strings.Contains(got, "# logging settings") {
		t.Errorf("header comment lost:\n%s", got)
	}

	var decoded struct {
		Log struct {
			Level string `toml:"level"`
		} `toml:"log"`
	}
	if _, err := toml.Decode(got, &decoded); err != nil {
		t.Fatalf("output does not parse: %v\n%s", err, got)
	}
	if decoded.Log.Level != "debug" {
		t.Errorf("log.level not updated: got %q:\n%s", decoded.Log.Level, got)
	}
}

// Misreading an annotated table header as an ordinary line also means the
// root table never appears to close: a key physically inside the table could
// be mistaken for a root key and edited in place, while a genuinely new root
// key never gets created. This is the same failure
// TestSetStringPlacesARootKeyBeforeTheFirstTable exists to prevent, reached
// by a different route.
func TestSetStringRootKeyStopsAtACommentedTableHeader(t *testing.T) {
	src := "[log] # logging settings\nport = \"9418\"\n"
	got := set(t, src, "", "port", "20000")

	var decoded struct {
		Port string `toml:"port"`
		Log  struct {
			Port string `toml:"port"`
		} `toml:"log"`
	}
	if _, err := toml.Decode(got, &decoded); err != nil {
		t.Fatalf("output does not parse: %v\n%s", err, got)
	}
	if decoded.Port != "20000" {
		t.Errorf("root port not created: %+v:\n%s", decoded, got)
	}
	if decoded.Log.Port != "9418" {
		t.Errorf("log.port disturbed; the root port likely landed inside [log] instead:\n%s", got)
	}
}

// A value may carry its own trailing "# ..." annotation — this repo's own
// TOML uses exactly that style, e.g. tools/drone/drone.example.toml:30's
// `log_level = "info"  # trace | debug | info | warn | error`. Rewriting the
// value must not silently delete the annotation.
func TestSetStringPreservesATrailingInlineComment(t *testing.T) {
	src := "[log]\nlevel = \"info\"  # trace | debug | info | warn | error\n"
	got := set(t, src, "log", "level", "debug")

	if !strings.Contains(got, "# trace | debug | info | warn | error") {
		t.Errorf("inline comment lost:\n%s", got)
	}

	var decoded struct {
		Log struct {
			Level string `toml:"level"`
		} `toml:"log"`
	}
	if _, err := toml.Decode(got, &decoded); err != nil {
		t.Fatalf("output does not parse: %v\n%s", err, got)
	}
	if decoded.Log.Level != "debug" {
		t.Errorf("log.level not updated: got %q:\n%s", decoded.Log.Level, got)
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
