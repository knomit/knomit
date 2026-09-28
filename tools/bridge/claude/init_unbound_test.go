package claude

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"knomit/tools/bridge/knomitapi"
)

// ---------------------------------------------------------------------------
// Flagless init scaffolds an UNBOUND server (#341). The directory name may
// appear in the KEY, never in args, and the hooks bind from args alone.
// ---------------------------------------------------------------------------

// namedDir returns a fresh directory with the given basename.
func namedDir(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRunInit_NoFlags_WritesUnboundEntry(t *testing.T) {
	dir := namedDir(t, "ingestion")
	chdir(t, dir)

	if err := runInit(nil); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	servers := parseMcpServers(t, mustRead(t, filepath.Join(dir, ".mcp.json")))
	srv, ok := servers["knomit-repo-ingestion"]
	if !ok {
		t.Fatalf("no knomit-repo-ingestion key; got %v", servers)
	}
	if srv.Command != "kb" {
		t.Errorf("command = %q, want kb", srv.Command)
	}
	if srv.Args == nil || len(srv.Args) != 0 {
		t.Errorf("args = %#v, want [] — the directory name must never reach args", srv.Args)
	}
}

// The producer/consumer test: runInit's own output, read by the hooks, is
// unbound — even though its key looks like a repo binding.
func TestRunInit_NoFlags_ScaffoldedConfigIsUnboundToHooks(t *testing.T) {
	dir := namedDir(t, "ingestion")
	chdir(t, dir)
	if err := runInit(nil); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	repo, lens, unbound, ambiguous := mcpBinding(dir)
	if !unbound || ambiguous || repo != "" || lens != "" {
		t.Errorf("mcpBinding = (%q, %q, unbound=%v, ambiguous=%v), want unbound with no target",
			repo, lens, unbound, ambiguous)
	}
	if got := mustSkipReason(t, dir); got != skipUnbound {
		t.Errorf("skip reason = %q, want %q", got, skipUnbound)
	}
}

func TestRunInit_NoFlags_NeverFailsOnTheDirectoryName(t *testing.T) {
	for _, name := range []string{"My Project", strings.Repeat("d", 60)} {
		t.Run(name, func(t *testing.T) {
			dir := namedDir(t, name)
			chdir(t, dir)
			if err := runInit(nil); err != nil {
				t.Fatalf("runInit in %q: %v", name, err)
			}
			want := knomitapi.UnboundServerKey(name)
			if _, ok := parseMcpServers(t, mustRead(t, filepath.Join(dir, ".mcp.json")))[want]; !ok {
				t.Errorf(".mcp.json has no %q entry", want)
			}
		})
	}
}

func TestRunInit_NoFlags_SecondRun_IsByteIdenticalNoOp(t *testing.T) {
	dir := namedDir(t, "ingestion")
	chdir(t, dir)
	if err := runInit(nil); err != nil {
		t.Fatalf("first runInit: %v", err)
	}
	assertReInitChangesNothing(t, dir)
}

// A flagless re-init keeps an existing scope. The unbound key for `ingestion`
// EQUALS the old repo key, so without this rule the same-key merge would
// silently rewrite `--repo ingestion` to [].
func TestRunInit_NoFlags_ExistingBoundEntry_KeepsScope(t *testing.T) {
	for _, tc := range []struct{ name, mcp, scope string }{
		{"same key", `{"mcpServers":{"knomit-repo-ingestion":{"command":"kb","args":["--repo","ingestion"]}}}`, "repo ingestion"},
		{"other repo", `{"mcpServers":{"knomit-repo-other":{"command":"kb","args":["--repo","other"]}}}`, "repo other"},
		{"lens", `{"mcpServers":{"knomit-lens-eng":{"command":"kb","args":["--lens","eng"]}}}`, "lens eng"},
		// The legacy constant key is never migrated (init-merge decision).
		{"legacy key", `{"mcpServers":{"knomit":{"command":"kb","args":["--repo","legacy"]}}}`, "repo legacy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := namedDir(t, "ingestion")
			chdir(t, dir)
			writeFixture(t, filepath.Join(dir, ".mcp.json"), tc.mcp)

			out := captureStdout(t, func() {
				if err := runInit(nil); err != nil {
					t.Fatalf("runInit: %v", err)
				}
			})

			if got := mustRead(t, filepath.Join(dir, ".mcp.json")); !bytes.Equal(got, []byte(tc.mcp)) {
				t.Errorf(".mcp.json changed on a flagless re-init:\n%s", got)
			}
			assertNoCompanion(t, dir, ".mcp.json")
			if !strings.Contains(out, "Kept: .mcp.json") || !strings.Contains(out, tc.scope) {
				t.Errorf("summary does not say the %s scope was kept:\n%s", tc.scope, out)
			}
		})
	}
}

// An explicit flag still refreshes the same-key entry, as before.
func TestRunInit_ExplicitRepo_OverUnboundEntry_Binds(t *testing.T) {
	dir := namedDir(t, "ingestion")
	chdir(t, dir)
	if err := runInit(nil); err != nil {
		t.Fatalf("first runInit: %v", err)
	}
	if err := runInit([]string{"--repo", "ingestion"}); err != nil {
		t.Fatalf("second runInit: %v", err)
	}
	args := parseMcpServers(t, mustRead(t, filepath.Join(dir, ".mcp.json")))["knomit-repo-ingestion"].Args
	if len(args) != 2 || args[0] != "--repo" || args[1] != "ingestion" {
		t.Errorf("args = %v, want [--repo ingestion]", args)
	}
}

// ---- hooks: the binding comes from ARGS only ----

// The key names a repo, the directory has that repo's name, and the args carry
// neither flag: the entry is UNBOUND. Neither the key nor the directory may
// supply a repo.
func TestMcpBinding_KeyLooksBound_ArgsUnbound(t *testing.T) {
	for _, args := range []string{`[]`, `null`, `["http://127.0.0.1:8080"]`} {
		t.Run(args, func(t *testing.T) {
			dir := namedDir(t, "ingestion")
			cfg := `{"mcpServers":{"knomit-repo-ingestion":{"command":"kb","args":` + args + `}}}`
			if args == `null` {
				cfg = `{"mcpServers":{"knomit-repo-ingestion":{"command":"kb"}}}`
			}
			writeFixture(t, filepath.Join(dir, ".mcp.json"), cfg)

			repo, lens, unbound, ambiguous := mcpBinding(dir)
			if !unbound || ambiguous || repo != "" || lens != "" {
				t.Errorf("mcpBinding = (%q, %q, unbound=%v, ambiguous=%v), want unbound", repo, lens, unbound, ambiguous)
			}
			if got, skip := resolveWriteRepo(dir); got != "" || skip != skipUnbound {
				t.Errorf("resolveWriteRepo = (%q, %q), want (\"\", %q)", got, skip, skipUnbound)
			}
		})
	}
}

// An unbound entry beside a bound one names two different scopes.
func TestMcpBinding_UnboundBesideBound_IsAmbiguous(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, ".mcp.json"), `{"mcpServers":{
		"knomit-repo-a":{"command":"kb","args":[]},
		"knomit-repo-b":{"command":"kb","args":["--repo","b"]}}}`)
	if _, _, _, ambiguous := mcpBinding(dir); !ambiguous {
		t.Error("an unbound entry beside a --repo entry was not reported as ambiguous")
	}
}

// A degenerate --lens is broken, not unbound: it must stay distinct from an
// unbound entry, so the two side by side are ambiguous.
func TestMcpBinding_DegenerateLensBesideUnbound_IsAmbiguous(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, ".mcp.json"), `{"mcpServers":{
		"knomit-a":{"command":"kb","args":[]},
		"knomit-b":{"command":"kb","args":["--lens"]}}}`)
	if _, _, _, ambiguous := mcpBinding(dir); !ambiguous {
		t.Error("a degenerate --lens entry was treated as the same target as an unbound one")
	}
}

func TestSessionStart_Unbound_InjectsBindNote_ReadsNoRepo(t *testing.T) {
	dir := namedDir(t, "ingestion")
	writeFixture(t, filepath.Join(dir, ".mcp.json"),
		`{"mcpServers":{"knomit-repo-ingestion":{"command":"kb","args":[]}}}`)
	// Any request to the server is a failure: an unbound entry has no repo.
	t.Setenv("KNOMIT_BASE_URL", "http://127.0.0.1:1")

	var out bytes.Buffer
	if err := hookSessionStart(strings.NewReader(`{"cwd":`+strconv.Quote(dir)+`}`), &out); err != nil {
		t.Fatalf("hookSessionStart: %v", err)
	}
	if got, want := out.String(), knomitapi.UnboundNote+"\n"; got != want {
		t.Errorf("session-start output = %q, want %q", got, want)
	}
}
