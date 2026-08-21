package antigravity

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "mcp_config.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func TestPluginBinding_RepoMode(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"mcpServers":{"knomit-repo-proj":{"command":"knomit-bridge","args":["--repo","proj"]}}}`)

	repo, lens, skip := pluginBinding(dir)
	if skip != "" || repo != "proj" || lens != "" {
		t.Errorf("got (%q,%q,%q), want (proj,\"\",\"\")", repo, lens, skip)
	}
}

func TestPluginBinding_LensMode(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"mcpServers":{"knomit-lens-eng":{"command":"knomit-bridge","args":["--lens","eng"]}}}`)

	repo, lens, skip := pluginBinding(dir)
	if skip != "" || repo != "" || lens != "eng" {
		t.Errorf("got (%q,%q,%q), want (\"\",eng,\"\")", repo, lens, skip)
	}
}

func TestPluginBinding_EqualsForms(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"mcpServers":{"knomit-repo-proj":{"command":"knomit-bridge","args":["--repo=proj"]}}}`)
	repo, _, skip := pluginBinding(dir)
	if skip != "" || repo != "proj" {
		t.Errorf("got (%q,%q), want (proj,\"\")", repo, skip)
	}
}

func TestPluginBinding_BothFlags_LensWins(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"mcpServers":{"k":{"command":"knomit-bridge","args":["--repo","proj","--lens","eng"]}}}`)
	repo, lens, skip := pluginBinding(dir)
	if skip != "" || repo != "" || lens != "eng" {
		t.Errorf("got (%q,%q,%q), want (\"\",eng,\"\")", repo, lens, skip)
	}
}

// REGRESSION: a lens entry whose value is missing must not let a SIBLING repo
// entry win. Before the fix, classifyArgs returned ("","") for the degenerate
// lens, pluginBinding skipped past it, and the hook bound to the raw repo —
// exactly the demotion this file's contract forbids. The Claude host refuses
// on the same input.
func TestPluginBinding_DegenerateLensWithSiblingRepo_SkipsNeverBindsRepo(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"mcpServers":{
		"knomit-lens-eng":{"command":"knomit-bridge","args":["--lens"]},
		"knomit-repo-other":{"command":"knomit-bridge","args":["--repo","other"]}
	}}`)

	// Run repeatedly: the old failure was map-order dependent.
	for i := 0; i < 20; i++ {
		repo, lens, skip := pluginBinding(dir)
		if repo != "" || lens != "" {
			t.Fatalf("run %d bound to (%q,%q); a degenerate lens must never bind", i, repo, lens)
		}
		if skip == "" {
			t.Fatalf("run %d returned no skip reason", i)
		}
	}
}

func TestPluginBinding_DegenerateLensAlone_SkipsLensUnusable(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"mcpServers":{"k":{"command":"knomit-bridge","args":["--lens"]}}}`)
	_, _, skip := pluginBinding(dir)
	if skip != skipLensUnusable {
		t.Errorf("skip = %q, want %q", skip, skipLensUnusable)
	}
}

// REGRESSION: two entries naming DIFFERENT scopes used to return whichever the
// map yielded first, so the binding varied between runs. It must be a stable
// skip instead.
func TestPluginBinding_TwoDifferentScopes_AlwaysAmbiguous(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"mcpServers":{
		"knomit-repo-alpha":{"command":"knomit-bridge","args":["--repo","alpha"]},
		"knomit-repo-beta":{"command":"knomit-bridge","args":["--repo","beta"]}
	}}`)

	for i := 0; i < 20; i++ {
		repo, _, skip := pluginBinding(dir)
		if skip != skipAmbiguousBinding {
			t.Fatalf("run %d: skip = %q (repo %q), want %q every time",
				i, skip, repo, skipAmbiguousBinding)
		}
	}
}

// Duplicated-but-consistent entries are not ambiguous — that is what an
// obvious merge produces, and disabling over it would fire on nothing.
func TestPluginBinding_TwoIdenticalScopes_NotAmbiguous(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"mcpServers":{
		"knomit-repo-alpha":{"command":"knomit-bridge","args":["--repo","alpha"]},
		"knomit-repo-alpha-dup":{"command":"knomit-bridge","args":["--repo","alpha"]}
	}}`)
	repo, _, skip := pluginBinding(dir)
	if skip != "" || repo != "alpha" {
		t.Errorf("got (%q,%q), want (alpha,\"\")", repo, skip)
	}
}

// REGRESSION: dropping the key tier made every wrapper script, dev build,
// renamed symlink, versioned binary and `go run` install silently dark.
func TestPluginBinding_UnrecognisedCommand_FallsBackToKeyTier(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"mcpServers":{"knomit-repo-alpha":{"command":"/usr/local/bin/knomit-bridge-dev","args":["--repo","alpha"]}}}`)
	repo, _, skip := pluginBinding(dir)
	if skip != "" || repo != "alpha" {
		t.Errorf("got (%q,%q), want (alpha,\"\") — key tier should rescue a wrapper command", repo, skip)
	}
}

// A command match is proof and must not be diluted by a server that merely
// borrowed the namespace.
func TestPluginBinding_KeyTierIgnoredWhenACommandMatches(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"mcpServers":{
		"knomit-repo-real":{"command":"knomit-bridge","args":["--repo","real"]},
		"knomit-notes":{"command":"some-other-tool","args":["--repo","bogus"]}
	}}`)
	repo, _, skip := pluginBinding(dir)
	if skip != "" || repo != "real" {
		t.Errorf("got (%q,%q), want (real,\"\")", repo, skip)
	}
}

func TestPluginBinding_MissingFile_NoBinding(t *testing.T) {
	if _, _, skip := pluginBinding(t.TempDir()); skip != skipNoBinding {
		t.Errorf("skip = %q, want %q", skip, skipNoBinding)
	}
}

func TestPluginBinding_MalformedJSON_NoBinding(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{not json`)
	if _, _, skip := pluginBinding(dir); skip != skipNoBinding {
		t.Errorf("skip = %q, want %q", skip, skipNoBinding)
	}
}

func TestPluginBinding_NoKnomitServer_NoBinding(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"mcpServers":{"other":{"command":"something-else","args":[]}}}`)
	if _, _, skip := pluginBinding(dir); skip != skipNoBinding {
		t.Errorf("skip = %q, want %q", skip, skipNoBinding)
	}
}

// REGRESSION: init validates names at WRITE time; this file is hand-editable
// afterwards, and the value lands in an API path. A traversal-shaped name used
// to reach the server as /api/v1/repos/../lenses/eng.
func TestPluginBinding_InvalidScopeOnRead_Rejected(t *testing.T) {
	for _, bad := range []string{"../lenses/eng", "a?b", "a#b", "UPPER", "a b", "a/b"} {
		t.Run(bad, func(t *testing.T) {
			dir := t.TempDir()
			writeConfig(t, dir, `{"mcpServers":{"k":{"command":"knomit-bridge","args":["--repo",`+jsonQuote(bad)+`]}}}`)
			repo, _, skip := pluginBinding(dir)
			if skip != skipInvalidScope {
				t.Errorf("repo %q accepted (skip=%q); want %q", repo, skip, skipInvalidScope)
			}
		})
	}
}

func TestPluginBinding_InvalidLensOnRead_Rejected(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"mcpServers":{"k":{"command":"knomit-bridge","args":["--lens","../evil"]}}}`)
	if _, _, skip := pluginBinding(dir); skip != skipInvalidScope {
		t.Errorf("skip = %q, want %q", skip, skipInvalidScope)
	}
}

// jsonQuote renders s as a JSON string literal so hostile names can be
// embedded in the fixture config verbatim.
func jsonQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestResolveWriteRepo_RepoMode_NoServerNeeded(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"mcpServers":{"k":{"command":"knomit-bridge","args":["--repo","proj"]}}}`)

	repo, skip := resolveWriteRepo(dir)
	if skip != "" || repo != "proj" {
		t.Errorf("got (%q,%q), want (proj,\"\")", repo, skip)
	}
}

func TestResolveWriteRepo_LensMode_ResolvesWriteRepo(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"mcpServers":{"k":{"command":"knomit-bridge","args":["--lens","eng"]}}}`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/hal+json")
		w.Write([]byte(`{"name":"eng","write":{"uid":"u1","name":"writerepo"},"reads":[]}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("KNOMIT_BASE_URL", srv.URL)

	repo, skip := resolveWriteRepo(dir)
	if skip != "" || repo != "writerepo" {
		t.Errorf("got (%q,%q), want (writerepo,\"\")", repo, skip)
	}
}

// A server-supplied write-repo name becomes a URL path segment too, so it is
// validated on the same principle as the configured names.
func TestResolveWriteRepo_LensMode_ServerReturnsHostileName_Rejected(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"mcpServers":{"k":{"command":"knomit-bridge","args":["--lens","eng"]}}}`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"name":"eng","write":{"uid":"u1","name":"../../etc"},"reads":[]}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("KNOMIT_BASE_URL", srv.URL)

	repo, skip := resolveWriteRepo(dir)
	if skip != skipInvalidScope {
		t.Errorf("got (%q,%q), want skip %q", repo, skip, skipInvalidScope)
	}
}

func TestResolveWriteRepo_LensMode_ServerDown_SkipsUnresolved(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"mcpServers":{"k":{"command":"knomit-bridge","args":["--lens","eng"]}}}`)
	t.Setenv("KNOMIT_BASE_URL", "http://127.0.0.1:1")

	repo, skip := resolveWriteRepo(dir)
	if skip != skipLensUnresolved || repo != "" {
		t.Errorf("got (%q,%q), want (\"\",%q)", repo, skip, skipLensUnresolved)
	}
}

func TestResolveWriteRepo_NoConfig_SkipsNoBinding(t *testing.T) {
	repo, skip := resolveWriteRepo(t.TempDir())
	if skip != skipNoBinding || repo != "" {
		t.Errorf("got (%q,%q), want (\"\",%q)", repo, skip, skipNoBinding)
	}
}
