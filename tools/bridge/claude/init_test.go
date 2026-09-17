package claude

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"knomit/tools/bridge/knomitapi"
)

func TestRunInit_EmptyDirectory_DropsAllFiles(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	if err := runInit([]string{"--repo", "testproj"}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	wantFiles := []string{
		".mcp.json",
		".claude/settings.json",
		".claude/skills/knomit-recall/SKILL.md",
		".claude/skills/knomit-remember/SKILL.md",
		".claude/skills/knomit-why/SKILL.md",
		".claude/skills/knomit-decided/SKILL.md",
		".claude/skills/knomit-review/SKILL.md",
		".claude/skills/knomit-update/SKILL.md",
		".claude/skills/knomit-retract/SKILL.md",
		".claude/skills/knomit-hypothesize/SKILL.md",
		"CLAUDE.md",
	}
	for _, f := range wantFiles {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}

	// .mcp.json must have the repo name interpolated
	mcp, _ := os.ReadFile(filepath.Join(dir, ".mcp.json"))
	if !strings.Contains(string(mcp), `"testproj"`) {
		t.Errorf(".mcp.json missing repo name; got:\n%s", mcp)
	}

	// No .claude/hooks/ directory should be created
	if _, err := os.Stat(filepath.Join(dir, ".claude/hooks")); err == nil {
		t.Error(".claude/hooks/ should NOT be created; hooks are now in knomit-bridge binary")
	}
}

func TestRunInit_NoHooksDirectory(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	if err := runInit([]string{"--repo", "x"}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	hooksDir := filepath.Join(dir, ".claude", "hooks")
	if _, err := os.Stat(hooksDir); err == nil {
		t.Errorf(".claude/hooks/ directory should not exist; got one at %s", hooksDir)
	}
}

func TestRunInit_SettingsJsonReferencesGoHooks(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	if err := runInit([]string{"--repo", "x"}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	s, err := os.ReadFile(filepath.Join(dir, ".claude/settings.json"))
	if err != nil {
		t.Fatalf("cannot read settings.json: %v", err)
	}
	content := string(s)

	// Must reference the three Go-based hook commands we ship.
	wantHooks := []string{
		"knomit-bridge claude hook session-start",
		"knomit-bridge claude hook post-edit",
		"knomit-bridge claude hook pre-compact",
	}
	for _, h := range wantHooks {
		if !strings.Contains(content, h) {
			t.Errorf("settings.json missing %q; got:\n%s", h, content)
		}
	}

	// Must NOT reference removed hooks.
	removedHooks := []string{
		"hook post-commit",
		"hook user-prompt-submit",
		"hook stop",
	}
	for _, h := range removedHooks {
		if strings.Contains(content, h) {
			t.Errorf("settings.json must not reference removed %q; got:\n%s", h, content)
		}
	}

	// Must NOT reference old .sh paths
	if strings.Contains(content, ".sh") {
		t.Errorf("settings.json should not reference .sh files; got:\n%s", content)
	}
	if strings.Contains(content, "$CLAUDE_PROJECT_DIR") {
		t.Errorf("settings.json should not reference $CLAUDE_PROJECT_DIR; got:\n%s", content)
	}
	if strings.Contains(content, "SessionEnd") {
		t.Errorf("settings.json must not register SessionEnd; got:\n%s", content)
	}
}

// TestRunInit_ExistingMcpJson_DifferentKnomitKey_DropsCompanion is the one
// .mcp.json case init still refuses to merge. A knomit-bridge entry under some
// OTHER key is a scope init cannot reconcile: adding the derived key beside it
// would give the project two knomit scopes, which disables the hooks outright
// (see the one-scope-per-project rule in mcpBinding). Only a human can say
// which scope was meant, so the companion and its two-scopes warning stay.
func TestRunInit_ExistingMcpJson_DifferentKnomitKey_DropsCompanion(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	// The legacy constant key, from before the key was derived per scope.
	existing := []byte(`{"mcpServers":{"knomit":{"command":"knomit-bridge","args":["--repo","legacy"]}}}`)
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), existing, 0o644); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		if err := runInit([]string{"--repo", "x"}); err != nil {
			t.Fatalf("runInit: %v", err)
		}
	})

	companion := filepath.Join(dir, ".mcp.json.knomit")
	if _, err := os.Stat(companion); err != nil {
		t.Errorf("expected companion file at %s: %v", companion, err)
	}

	// The original must be untouched, byte for byte.
	if got := mustRead(t, filepath.Join(dir, ".mcp.json")); !bytes.Equal(got, existing) {
		t.Errorf("original .mcp.json was modified; got:\n%s", got)
	}
	// The two-scopes warning is the whole point of keeping the companion here.
	if !strings.Contains(out, "ONE knomit SCOPE") {
		t.Errorf("summary omitted the two-scopes warning:\n%s", out)
	}
}

// TestRunInit_ExistingClaudeMd_HeadingWithoutMarker_DropsBlockCompanion is the
// one CLAUDE.md case init still refuses to merge. A block identified only by
// its heading has no closing delimiter, so init cannot tell where it ends —
// the template shipped without the HTML markers until c36015e7, and users strip
// comments. Guessing the extent would either eat the user's own prose or leave
// half a stale block behind, so this one keeps the companion and the
// "replace the whole block" warning.
func TestRunInit_ExistingClaudeMd_HeadingWithoutMarker_DropsBlockCompanion(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	existing := []byte("# Existing\n\n" + blockHeading + "\n\nsome older prose\n")
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), existing, 0o644); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		if err := runInit([]string{"--repo", "x"}); err != nil {
			t.Fatalf("runInit: %v", err)
		}
	})

	companion := filepath.Join(dir, "CLAUDE.md.knomit-block")
	if _, err := os.Stat(companion); err != nil {
		t.Errorf("expected companion at %s: %v", companion, err)
	}
	if got := mustRead(t, filepath.Join(dir, "CLAUDE.md")); !bytes.Equal(got, existing) {
		t.Errorf("original CLAUDE.md was modified; got:\n%s", got)
	}
	if !strings.Contains(out, "replace the whole block") {
		t.Errorf("summary omitted the whole-block warning:\n%s", out)
	}
}

func TestRunInit_SkillsDeleted_GetRestored(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	if err := runInit([]string{"--repo", "x"}); err != nil {
		t.Fatalf("runInit #1: %v", err)
	}
	skillPath := filepath.Join(dir, ".claude/skills/knomit-recall/SKILL.md")

	// User deletes the skill
	if err := os.Remove(skillPath); err != nil {
		t.Fatal(err)
	}

	// Re-running init must restore it
	if err := runInit([]string{"--repo", "x"}); err != nil {
		t.Fatalf("runInit #2: %v", err)
	}

	if _, err := os.Stat(skillPath); err != nil {
		t.Errorf("skill was not restored after re-run: %v", err)
	}
}

func TestRunInit_SkillFrontmatterMatchesDir(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	if err := runInit([]string{"--repo", "x"}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	skills := map[string]string{
		".claude/skills/knomit-recall/SKILL.md":      "name: knomit-recall",
		".claude/skills/knomit-remember/SKILL.md":    "name: knomit-remember",
		".claude/skills/knomit-why/SKILL.md":         "name: knomit-why",
		".claude/skills/knomit-decided/SKILL.md":     "name: knomit-decided",
		".claude/skills/knomit-review/SKILL.md":      "name: knomit-review",
		".claude/skills/knomit-update/SKILL.md":      "name: knomit-update",
		".claude/skills/knomit-retract/SKILL.md":     "name: knomit-retract",
		".claude/skills/knomit-hypothesize/SKILL.md": "name: knomit-hypothesize",
	}
	for path, wantFrontmatter := range skills {
		data, err := os.ReadFile(filepath.Join(dir, path))
		if err != nil {
			t.Errorf("cannot read %s: %v", path, err)
			continue
		}
		if !strings.Contains(string(data), wantFrontmatter) {
			t.Errorf("%s: frontmatter missing %q; got:\n%s", path, wantFrontmatter, data)
		}
	}
}

// TestRunInit_RepoMode_McpJsonArgsAreExactlyRepo asserts a repo-mode .mcp.json
// carries exactly ["--repo", <name>] — no vestigial --source/--profile flags.
func TestRunInit_RepoMode_McpJsonArgsAreExactlyRepo(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	if err := runInit([]string{"--repo", "team-kb"}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	mcp, err := os.ReadFile(filepath.Join(dir, ".mcp.json"))
	if err != nil {
		t.Fatalf("read .mcp.json: %v", err)
	}
	var cfg struct {
		McpServers map[string]struct {
			Args []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(mcp, &cfg); err != nil {
		t.Fatalf(".mcp.json does not parse: %v\n%s", err, mcp)
	}
	// The key is DERIVED from the scope, not the constant "knomit" — that
	// constant made a second `claude init` collide and is why two knomit
	// servers could never coexist in one project.
	if _, stale := cfg.McpServers["knomit"]; stale {
		t.Errorf(`.mcp.json still uses the constant key "knomit"; want %q`, "knomit-repo-team-kb")
	}
	got := cfg.McpServers["knomit-repo-team-kb"].Args
	want := []string{"--repo", "team-kb"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("knomit-repo-team-kb args = %v, want %v", got, want)
	}
}

// TestRunInit_ScaffoldedConfigBindsHooks is the test whose absence let a real
// bug through: every mcpBinding test hand-builds .mcp.json with a literal key,
// so none of them noticed when runInit stopped emitting that key. This pipes
// runInit's actual output into mcpBinding.
//
// The failure it guards is silent: mcpBinding used to select the server by the
// constant key "knomit", so a derived key made it fall through to the basename
// fallback — repo mode against a directory-named repo, or (worse) a LENS-scoped
// project demoted to a basename repo, which is the wrong-repo hazard
// mcpBinding's contract says must never happen.
func TestRunInit_ScaffoldedConfigBindsHooks(t *testing.T) {
	t.Run("repo scope binds to the configured repo", func(t *testing.T) {
		dir := t.TempDir()
		chdir(t, dir)
		if err := runInit([]string{"--repo", "team-kb"}); err != nil {
			t.Fatalf("runInit: %v", err)
		}
		repo, lens, ambiguous := mcpBinding(dir)
		if ambiguous {
			t.Fatal("single server reported as ambiguous")
		}
		if lens != "" {
			t.Errorf("lens = %q, want empty (repo scope)", lens)
		}
		if repo != "team-kb" {
			t.Errorf("repo = %q, want %q — hooks would bind to the wrong repo", repo, "team-kb")
		}
		if repo == filepath.Base(dir) {
			t.Error("repo fell back to the directory basename; the --repo flag was ignored")
		}
	})

	t.Run("lens scope never falls back to the basename", func(t *testing.T) {
		dir := t.TempDir()
		chdir(t, dir)
		if err := runInit([]string{"--lens", "eng"}); err != nil {
			t.Fatalf("runInit: %v", err)
		}
		repo, lens, ambiguous := mcpBinding(dir)
		if ambiguous {
			t.Fatal("single server reported as ambiguous")
		}
		if lens != "eng" {
			t.Errorf("lens = %q, want %q", lens, "eng")
		}
		if repo != "" {
			t.Errorf("repo = %q, want empty — a lens-scoped project must never "+
				"resolve to a repo, least of all the directory basename", repo)
		}
	})
}

// TestMcpBinding_MultipleKnomitServers pins the fail-safe for the configuration
// this PR makes possible for the first time. With two knomit servers there is no
// principled answer to "which repo do the hooks bind to?", so binding must skip
// with a stated reason rather than pick one and risk running post-edit against
// the wrong repo.
func TestMcpBinding_MultipleKnomitServers(t *testing.T) {
	dir := t.TempDir()
	cfg := `{"mcpServers":{
		"knomit-codebase":{"command":"knomit-bridge","args":["--repo","codebase"]},
		"knomit-agentic":{"command":"knomit-bridge","args":["--lens","agentic"]}
	}}`
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	repo, lens, ambiguous := mcpBinding(dir)
	if !ambiguous {
		t.Fatalf("two knomit servers not reported as ambiguous (repo=%q lens=%q)", repo, lens)
	}
	if repo != "" || lens != "" {
		t.Errorf("ambiguous binding leaked a target: repo=%q lens=%q", repo, lens)
	}
	if got, want := mustSkipReason(t, dir), "multiple_knomit_servers"; got != want {
		t.Errorf("skip reason = %q, want %q", got, want)
	}
}

func mustSkipReason(t *testing.T, dir string) string {
	t.Helper()
	_, skip := resolveWriteRepo(dir)
	return skip
}

// TestRunInit_RejectsOverlongDerivedKey guards the tool-name ceiling. The key
// used to be a 6-char constant so this was unreachable; it now derives from a
// repo name that defaults to the directory basename.
func TestRunInit_RejectsOverlongDerivedKey(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	long := strings.Repeat("a", knomitapi.MaxServerKeyLen)
	err := runInit([]string{"--repo", long})
	if err == nil {
		t.Fatalf("runInit accepted a repo name yielding a %d-char key", len(knomitapi.ServerKey(long, "")))
	}
	if !strings.Contains(err.Error(), "server key") {
		t.Errorf("error %q does not explain the key-length limit", err)
	}
	// The remedy has to be actionable: repo mode can trip this with no flag at
	// all (the name defaults to the directory basename), so the message must
	// quote the name budget and say the repo need not match the directory.
	for _, want := range []string{
		fmt.Sprintf("max %d", knomitapi.MaxScopeNameLen),
		"--repo",
		"need not match the directory",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestRunInit_AcceptsMaxLengthScopeName pins the other side of the boundary: a
// name at exactly the budget must still scaffold. Without it, tightening the
// prefix would silently shrink the accepted range with no test failing.
func TestRunInit_AcceptsMaxLengthScopeName(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	name := strings.Repeat("a", knomitapi.MaxScopeNameLen)
	if err := runInit([]string{"--repo", name}); err != nil {
		t.Fatalf("runInit rejected a %d-char repo name at the documented max: %v", len(name), err)
	}
	if got := len(knomitapi.ServerKey(name, "")); got != knomitapi.MaxServerKeyLen {
		t.Errorf("key for a max-length name is %d chars, want exactly %d — "+
			"maxScopeNameLen and the axis prefix have drifted apart", got, knomitapi.MaxServerKeyLen)
	}
}

// TestBlockMarkerCurrent_ComesFromTemplate ties "current" to the shipped
// template. The constant it replaced was a hand-copy of the template's first
// line, and nothing failed when the two drifted: bump the template alone and a
// freshly merged block reports as stale forever, bump the copy alone and every
// install reports as current. Both directions are silent, which is the same
// failure the version marker exists to prevent.
func TestBlockMarkerCurrent_ComesFromTemplate(t *testing.T) {
	if blockMarkerCurrent == "" {
		t.Fatalf("%s has no %q marker on its first line, so claudeMdBlockNote "+
			"can no longer recognise a current block", claudeMdBlockTemplate, blockMarkerPrefix)
	}
	data, err := templatesFS.ReadFile(claudeMdBlockTemplate)
	if err != nil {
		t.Fatalf("read %s: %v", claudeMdBlockTemplate, err)
	}
	first, _, _ := strings.Cut(string(data), "\n")
	if got := strings.TrimSpace(first); got != blockMarkerCurrent {
		t.Errorf("blockMarkerCurrent = %q, template first line = %q", blockMarkerCurrent, got)
	}
	if !strings.HasPrefix(blockMarkerCurrent, blockMarkerPrefix) {
		t.Errorf("marker %q does not start with %q, so the older-version arm "+
			"of claudeMdBlockNote can never fire", blockMarkerCurrent, blockMarkerPrefix)
	}
}

// TestClaudeMdBlockNote covers the merge advice, which had no test at all. The
// three outcomes give materially different instructions — replace the block,
// append it, or say nothing — so getting one wrong tells the user to duplicate
// or to skip a needed merge.
func TestClaudeMdBlockNote(t *testing.T) {
	block, err := templatesFS.ReadFile(claudeMdBlockTemplate)
	if err != nil {
		t.Fatalf("read %s: %v", claudeMdBlockTemplate, err)
	}
	for _, tc := range []struct {
		name, content, want string
	}{
		{"current block says nothing", "# Project\n\n" + string(block), ""},
		{
			"older marker asks for a whole-block replace",
			"# Project\n\n<!-- knomit:integration v1 -->\n" + blockHeading + "\n\nold text\n",
			"older version",
		},
		{
			// v3 is the marker that was CURRENT until the memory-guard block
			// bumped it to v4. A project scaffolded before that bump must be
			// told its block is stale, or it never learns the rule the bump
			// exists to ship — this is the case a bump silently breaks if the
			// template and the constant ever drift apart.
			"the immediately previous version is still older",
			"# Project\n\n<!-- knomit:integration v3 -->\n" + blockHeading + "\n\nold text\n",
			"older version",
		},
		{
			// The template shipped without the HTML marker until c36015e7, and
			// users strip comments. Calling such a block absent would advise
			// appending a SECOND full copy.
			"marker-less block is recognised by its heading",
			"# Project\n\n" + blockHeading + "\n\nold text\n",
			"older version",
		},
		{"no block at all asks for an append", "# Project\n\nnothing knomit here\n", "no knomit block"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "CLAUDE.md")
			if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			got := claudeMdBlockNote(path)
			if tc.want == "" {
				if got != "" {
					t.Errorf("note = %q, want empty", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("note = %q, want it to mention %q", got, tc.want)
			}
		})
	}

	t.Run("unreadable file says nothing", func(t *testing.T) {
		if got := claudeMdBlockNote(filepath.Join(t.TempDir(), "absent.md")); got != "" {
			t.Errorf("note = %q, want empty for a missing file", got)
		}
	})
}

func TestRunInit_Lens_WritesLensScopedMcpJson(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	if err := runInit([]string{"--lens", "eng"}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	mcp, err := os.ReadFile(filepath.Join(dir, ".mcp.json"))
	if err != nil {
		t.Fatalf("cannot read .mcp.json: %v", err)
	}
	content := string(mcp)
	if !strings.Contains(content, `"--lens"`) {
		t.Errorf(".mcp.json missing --lens flag; got:\n%s", content)
	}
	if !strings.Contains(content, `"eng"`) {
		t.Errorf(".mcp.json missing lens name; got:\n%s", content)
	}
	// Lens mode must not emit repo-scoped flags.
	for _, unwanted := range []string{"--repo", "--source", "--profile"} {
		if strings.Contains(content, unwanted) {
			t.Errorf(".mcp.json should not contain %q in lens mode; got:\n%s", unwanted, content)
		}
	}
}

func TestRunInit_LensAndRepo_Errors(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	err := runInit([]string{"--lens", "eng", "--repo", "core"})
	if err == nil {
		t.Fatal("runInit with both --lens and --repo = nil, want error")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error %q does not mention mutual exclusion", err)
	}
}

func TestRunInit_InvalidNames_ErrorBeforeWriting(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantSub string // substring the error must name (the offending flag)
	}{
		{"repo with quote+comma", []string{"--repo", `a","x`}, "--repo"},
		{"repo with backslash", []string{"--repo", `a\b`}, "--repo"},
		{"repo with space", []string{"--repo", "a b"}, "--repo"},
		{"lens with quote+comma", []string{"--lens", `a","x`}, "--lens"},
		{"lens with backslash", []string{"--lens", `a\b`}, "--lens"},
		{"lens with space", []string{"--lens", "a b"}, "--lens"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			chdir(t, dir)

			err := runInit(tc.args)
			if err == nil {
				t.Fatalf("runInit(%v) = nil, want error", tc.args)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not name offending flag %q", err, tc.wantSub)
			}

			// Nothing must have been written before the validation failed.
			for _, f := range []string{".mcp.json", ".claude/settings.json", "CLAUDE.md"} {
				if _, statErr := os.Stat(filepath.Join(dir, f)); statErr == nil {
					t.Errorf("%s was written despite invalid input", f)
				}
			}
		})
	}
}

func TestRunInit_ValidNames_McpJsonParsesAsJSON(t *testing.T) {
	t.Run("repo mode", func(t *testing.T) {
		dir := t.TempDir()
		chdir(t, dir)
		if err := runInit([]string{"--repo", "team-kb"}); err != nil {
			t.Fatalf("runInit: %v", err)
		}
		mcp, err := os.ReadFile(filepath.Join(dir, ".mcp.json"))
		if err != nil {
			t.Fatalf("read .mcp.json: %v", err)
		}
		var v any
		if err := json.Unmarshal(mcp, &v); err != nil {
			t.Errorf(".mcp.json does not parse as JSON: %v\n%s", err, mcp)
		}
	})
	t.Run("lens mode", func(t *testing.T) {
		dir := t.TempDir()
		chdir(t, dir)
		if err := runInit([]string{"--lens", "eng"}); err != nil {
			t.Fatalf("runInit: %v", err)
		}
		mcp, err := os.ReadFile(filepath.Join(dir, ".mcp.json"))
		if err != nil {
			t.Fatalf("read .mcp.json: %v", err)
		}
		var v any
		if err := json.Unmarshal(mcp, &v); err != nil {
			t.Errorf(".mcp.json does not parse as JSON: %v\n%s", err, mcp)
		}
	})
}

func TestJsonStr_EscapesQuotesAndBackslashes(t *testing.T) {
	cases := map[string]string{
		`plain`:    `"plain"`,
		`a"b`:      `"a\"b"`,
		`a\b`:      `"a\\b"`,
		`a","x`:    `"a\",\"x"`,
		"tab\ttab": `"tab\ttab"`,
	}
	for in, want := range cases {
		got := jsonStr(in)
		if got != want {
			t.Errorf("jsonStr(%q) = %q, want %q", in, got, want)
		}
		// The escaped output, embedded in JSON, must round-trip to the input.
		var s string
		if err := json.Unmarshal([]byte(got), &s); err != nil {
			t.Errorf("jsonStr(%q) = %q does not parse as a JSON string: %v", in, got, err)
		} else if s != in {
			t.Errorf("jsonStr(%q) round-tripped to %q", in, s)
		}
	}
}

func chdir(t *testing.T, dir string) {
	t.Helper()
	old, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(old) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// Merging the three merge-required files.
//
// The companion protocol — write <file>.knomit beside it, print "merge
// manually" — is silent-failure-prone. Re-running init on this very checkout
// after the memory-guard hook shipped left .claude/settings.json WITHOUT the
// PreToolUse entry: the operator merged CLAUDE.md and .mcp.json by hand and
// never noticed the third companion, so the block marker said v4 while the
// guard was not registered. init now merges structurally wherever the file's
// shape makes the merge unambiguous, and keeps a companion only where it does
// not.
// ---------------------------------------------------------------------------

// handFormattedSettings is a settings.json as a PROJECT actually carries one:
// pretty-printed, with the user's own permissions and keys beside the hooks.
// Merge tests start from this rather than from init's own output, because
// re-emitting the file is only invisible when the input already looks like what
// the encoder would have produced.
const handFormattedSettings = `{
  "hooks": {
    "SessionStart": [
      {
        "matcher": "",
        "hooks": [
          {
            "type": "command",
            "command": "knomit-bridge claude hook session-start"
          }
        ]
      }
    ],
    "PostToolUse": [
      {
        "matcher": "Edit|Write|MultiEdit",
        "hooks": [
          {
            "type": "command",
            "command": "knomit-bridge claude hook post-edit"
          }
        ]
      },
      {
        "matcher": "AskUserQuestion",
        "hooks": [
          {
            "type": "command",
            "command": "knomit-bridge claude hook post-ask"
          }
        ]
      }
    ],
    "PreCompact": [
      {
        "matcher": "",
        "hooks": [
          {
            "type": "command",
            "command": "knomit-bridge claude hook pre-compact"
          }
        ]
      }
    ]
  },
  "permissions": {
    "allow": [],
    "deny": [
      "Read(./.claude/plans/archive/**)"
    ],
    "additionalDirectories": []
  },
  "plansDirectory": ".claude/plans"
}
`

// captureStdout runs fn with os.Stdout redirected and returns what it printed.
// The summary lines ARE the user-facing contract: a merge nobody is told about
// is indistinguishable from the silent no-merge this change removes.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	defer func() { os.Stdout = old }()
	fn()
	_ = w.Close()
	return <-done
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func writeFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// assertNoCompanion is the negative half of every merge test. A file init can
// merge must never ALSO leave a companion — the companion is the artefact the
// operator learns to ignore, which is how the memory-guard hook went missing.
func assertNoCompanion(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, name := range names {
		for _, suffix := range []string{".knomit", ".knomit-block"} {
			if _, err := os.Stat(filepath.Join(dir, name+suffix)); err == nil {
				t.Errorf("companion %s%s was written; this file is mergeable", name, suffix)
			}
		}
	}
}

// assertSingleInsertion pins the merge to an INSERTION rather than a rewrite:
// after stripping the longest common prefix and the longest common suffix, all
// of the original must be accounted for. Re-emitting the file from a decoded
// structure fails this even when the JSON is semantically identical, which is
// the point — the user's formatting, key order and blank lines are theirs.
func assertSingleInsertion(t *testing.T, before, after []byte, what string) {
	t.Helper()
	p := 0
	for p < len(before) && p < len(after) && before[p] == after[p] {
		p++
	}
	s := 0
	for s < len(before)-p && s < len(after)-p && before[len(before)-1-s] == after[len(after)-1-s] {
		s++
	}
	if p+s != len(before) {
		t.Errorf("%s was rewritten, not inserted into: %d of %d original bytes survive "+
			"as a common prefix/suffix\n--- before ---\n%s\n--- after ---\n%s",
			what, p+s, len(before), before, after)
	}
}

// TestRunInit_SecondRun_IsByteIdenticalNoOp is what makes re-init safe to run
// at any time. Under the companion protocol the three files were byte-identical
// only because init declined to touch them — and left three companions saying
// so.
func TestRunInit_SecondRun_IsByteIdenticalNoOp(t *testing.T) {
	t.Run("from init's own output", func(t *testing.T) {
		dir := t.TempDir()
		chdir(t, dir)
		if err := runInit([]string{"--repo", "x"}); err != nil {
			t.Fatalf("runInit #1: %v", err)
		}
		assertReInitChangesNothing(t, dir, "--repo", "x")
	})

	// The case that actually occurs: a project whose files were hand-formatted
	// and hand-merged, already carrying every hook this build ships.
	t.Run("from a hand-formatted project already up to date", func(t *testing.T) {
		dir := t.TempDir()
		chdir(t, dir)
		block, err := templatesFS.ReadFile(claudeMdBlockTemplate)
		if err != nil {
			t.Fatal(err)
		}
		writeFixture(t, filepath.Join(dir, ".claude", "settings.json"),
			strings.Replace(handFormattedSettings,
				`    "PreCompact": [`,
				`    "PreToolUse": [
      {
        "matcher": "Write|Edit|MultiEdit|Bash",
        "hooks": [
          {
            "type": "command",
            "command": "knomit-bridge claude hook memory-guard"
          }
        ]
      }
    ],
    "PreCompact": [`, 1))
		writeFixture(t, filepath.Join(dir, "CLAUDE.md"), "# Project\n\nHouse rules.\n\n"+string(block))
		writeFixture(t, filepath.Join(dir, ".mcp.json"), fmt.Sprintf(`{
  "mcpServers": {
    %q: {
      "command": "knomit-bridge",
      "args": ["--repo", "x"]
    }
  }
}
`, knomitapi.ServerKey("x", "")))
		assertReInitChangesNothing(t, dir, "--repo", "x")
	})
}

func assertReInitChangesNothing(t *testing.T, dir string, args ...string) {
	t.Helper()
	files := []string{".mcp.json", ".claude/settings.json", "CLAUDE.md"}
	before := map[string][]byte{}
	for _, f := range files {
		before[f] = mustRead(t, filepath.Join(dir, f))
	}

	out := captureStdout(t, func() {
		if err := runInit(args); err != nil {
			t.Fatalf("re-init: %v", err)
		}
	})

	for _, f := range files {
		if got := mustRead(t, filepath.Join(dir, f)); !bytes.Equal(got, before[f]) {
			t.Errorf("%s changed on a re-init\n--- before ---\n%s\n--- after ---\n%s", f, before[f], got)
		}
	}
	assertNoCompanion(t, dir, ".mcp.json", "CLAUDE.md")
	assertNoCompanion(t, filepath.Join(dir, ".claude"), "settings.json")
	if strings.Contains(out, "Updated:") {
		t.Errorf("a no-op re-init reported an update:\n%s", out)
	}
	if strings.Contains(out, "merge") {
		t.Errorf("a no-op re-init asked for a manual merge:\n%s", out)
	}
}

// TestRunInit_ExistingSettings_MergesTemplateHooks is the regression this change
// exists for: a settings.json scaffolded before the memory-guard hook must GAIN
// the PreToolUse entry, keeping the user's hooks, permissions and formatting.
func TestRunInit_ExistingSettings_MergesTemplateHooks(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	settingsPath := filepath.Join(dir, ".claude", "settings.json")
	existing := handFormattedSettings + ""
	writeFixture(t, settingsPath, existing)

	out := captureStdout(t, func() {
		if err := runInit([]string{"--repo", "x"}); err != nil {
			t.Fatalf("runInit: %v", err)
		}
	})

	got := mustRead(t, settingsPath)
	if !strings.Contains(string(got), "knomit-bridge claude hook memory-guard") {
		t.Errorf("memory-guard hook was not merged in; got:\n%s", got)
	}
	for _, want := range []string{
		`"Read(./.claude/plans/archive/**)"`,
		`"additionalDirectories": []`,
		`"plansDirectory": ".claude/plans"`,
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("merge dropped %s; got:\n%s", want, got)
		}
	}
	// Exactly one hook event is missing, so exactly one insertion is correct.
	assertSingleInsertion(t, []byte(existing), got, ".claude/settings.json")

	var parsed any
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Errorf("merged settings.json does not parse: %v\n%s", err, got)
	}
	assertNoCompanion(t, filepath.Join(dir, ".claude"), "settings.json")
	if !strings.Contains(out, "Updated: .claude/settings.json") {
		t.Errorf("summary did not report the settings merge:\n%s", out)
	}
	if !strings.Contains(out, "memory-guard") {
		t.Errorf("summary did not name the hook it added:\n%s", out)
	}

	// And it settles: a second run has nothing left to do.
	assertReInitChangesNothing(t, dir, "--repo", "x")
}

// TestRunInit_ExistingSettings_HookUnderUserMatcher_NotDuplicated pins
// idempotence to the hook COMMAND, not the matcher. A user who registered
// post-edit under their own matcher already HAS the hook; keying off the
// template's matcher hands them a second copy that fires on every edit twice,
// and a duplicate is invisible in the diff of a long settings.json.
func TestRunInit_ExistingSettings_HookUnderUserMatcher_NotDuplicated(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	settingsPath := filepath.Join(dir, ".claude", "settings.json")
	writeFixture(t, settingsPath, `{
  "hooks": {
    "PostToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {
            "type": "command",
            "command": "knomit-bridge claude hook post-edit"
          }
        ]
      }
    ]
  }
}
`)

	if err := runInit([]string{"--repo", "x"}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	got := string(mustRead(t, settingsPath))
	if n := strings.Count(got, "claude hook post-edit"); n != 1 {
		t.Errorf("post-edit appears %d times, want 1 — idempotence is keyed by the "+
			"matcher rather than by the hook command:\n%s", n, got)
	}
	if !strings.Contains(got, `"Bash"`) {
		t.Errorf("the merge rewrote the user's own matcher; got:\n%s", got)
	}
	// A hook genuinely absent is still added, under the template's matcher.
	if !strings.Contains(got, "claude hook post-ask") {
		t.Errorf("post-ask was not added; got:\n%s", got)
	}
	assertReInitChangesNothing(t, dir, "--repo", "x")
}

// TestRunInit_ExistingSettings_PathPrefixedHookCommand_NotDuplicated: hooks are
// commonly registered by absolute path, because knomit-bridge is not on $PATH
// outside the macOS .app. A plain string compare treats
// "<dir>/knomit-bridge claude hook post-edit" as a different hook and registers
// a second copy — which then runs the SAME hook twice per edit.
func TestRunInit_ExistingSettings_PathPrefixedHookCommand_NotDuplicated(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	settingsPath := filepath.Join(dir, ".claude", "settings.json")
	writeFixture(t, settingsPath, `{
  "hooks": {
    "PostToolUse": [
      {
        "matcher": "Edit|Write|MultiEdit",
        "hooks": [
          {
            "type": "command",
            "command": "${CLAUDE_PROJECT_DIR:-.}/dist/knomit-bridge claude hook post-edit"
          }
        ]
      }
    ],
    "SessionStart": [
      {
        "matcher": "",
        "hooks": [
          {
            "type": "command",
            "command": "/usr/local/bin/knomit-bridge claude hook session-start"
          }
        ]
      }
    ]
  }
}
`)

	if err := runInit([]string{"--repo", "x"}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	got := string(mustRead(t, settingsPath))
	for _, cmd := range []string{"claude hook post-edit", "claude hook session-start"} {
		if n := strings.Count(got, cmd); n != 1 {
			t.Errorf("%q appears %d times, want 1 — a path-prefixed hook command was "+
				"not recognised as the same hook:\n%s", cmd, n, got)
		}
	}
	// The user's paths are theirs; the merge must not rewrite them to bare names.
	for _, want := range []string{"${CLAUDE_PROJECT_DIR:-.}/dist/knomit-bridge", "/usr/local/bin/knomit-bridge"} {
		if !strings.Contains(got, want) {
			t.Errorf("merge rewrote the user's hook path %q away; got:\n%s", want, got)
		}
	}
	assertReInitChangesNothing(t, dir, "--repo", "x")
}

// TestRunInit_MalformedSettings_LeftByteIdentical: a settings.json init cannot
// parse is one it must not touch at all. Rewriting it would destroy the hand
// edit the user is halfway through, and a companion would be exactly the silent
// failure this change removes — so the only safe outcome is a loud error naming
// the file.
func TestRunInit_MalformedSettings_LeftByteIdentical(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	broken := "{\n  \"hooks\": {\n    \"SessionStart\": [ , ]\n  }\n"
	settingsPath := filepath.Join(dir, ".claude", "settings.json")
	writeFixture(t, settingsPath, broken)

	// The other two merge targets, to pin that nothing lands before the check.
	md := "# Project\n"
	mcp := `{"mcpServers":{}}`
	writeFixture(t, filepath.Join(dir, "CLAUDE.md"), md)
	writeFixture(t, filepath.Join(dir, ".mcp.json"), mcp)

	err := runInit([]string{"--repo", "x"})
	if err == nil {
		t.Fatal("runInit accepted an unparseable settings.json")
	}
	if !strings.Contains(err.Error(), ".claude/settings.json") {
		t.Errorf("error %q does not name the offending file", err)
	}
	if got := mustRead(t, settingsPath); !bytes.Equal(got, []byte(broken)) {
		t.Errorf("unparseable settings.json was modified:\n%s", got)
	}
	assertNoCompanion(t, filepath.Join(dir, ".claude"), "settings.json")

	// init validates before it writes — the same contract the name checks hold
	// (TestRunInit_InvalidNames_ErrorBeforeWriting). A scaffold that half-lands
	// leaves the operator reconciling two files against an error about a third.
	if got := mustRead(t, filepath.Join(dir, "CLAUDE.md")); !bytes.Equal(got, []byte(md)) {
		t.Errorf("CLAUDE.md was merged before the settings check failed:\n%s", got)
	}
	if got := mustRead(t, filepath.Join(dir, ".mcp.json")); !bytes.Equal(got, []byte(mcp)) {
		t.Errorf(".mcp.json was merged before the settings check failed:\n%s", got)
	}
	if _, statErr := os.Stat(filepath.Join(dir, ".claude", "skills")); statErr == nil {
		t.Error("skills were written before the settings check failed")
	}
}

// TestRunInit_ExistingClaudeMd_OlderMarker_ReplacedInPlace: the version marker
// exists so a stale block can be REPLACED. Telling the operator to do it by hand
// is what let installed blocks drift to "Nine /knomit-… slash commands" against
// a template saying eleven.
func TestRunInit_ExistingClaudeMd_OlderMarker_ReplacedInPlace(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	block, err := templatesFS.ReadFile(claudeMdBlockTemplate)
	if err != nil {
		t.Fatal(err)
	}
	preamble := "# Project\n\nOur own house rules.\n\n"
	stale := "<!-- knomit:integration v3 -->\n" + blockHeading + "\n\nold text\n<!-- /knomit:integration -->\n"
	tail := "\n## Local section\n\nkeep me\n"
	mdPath := filepath.Join(dir, "CLAUDE.md")
	writeFixture(t, mdPath, preamble+stale+tail)

	out := captureStdout(t, func() {
		if err := runInit([]string{"--repo", "x"}); err != nil {
			t.Fatalf("runInit: %v", err)
		}
	})

	if got, want := string(mustRead(t, mdPath)), preamble+string(block)+tail; got != want {
		t.Errorf("block not replaced in place\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	assertNoCompanion(t, dir, "CLAUDE.md")
	if !strings.Contains(out, "Updated: CLAUDE.md") {
		t.Errorf("summary did not report the block replacement:\n%s", out)
	}
	// Naming both versions is what tells the operator the bump actually landed.
	for _, want := range []string{"v3", "v4"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary %q does not name version %q", out, want)
		}
	}
	assertReInitChangesNothing(t, dir, "--repo", "x")
}

// TestRunInit_ExistingClaudeMd_NoBlock_AppendsOnce: a CLAUDE.md with no knomit
// block gains one — and gains exactly one, however often init runs.
func TestRunInit_ExistingClaudeMd_NoBlock_AppendsOnce(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	block, err := templatesFS.ReadFile(claudeMdBlockTemplate)
	if err != nil {
		t.Fatal(err)
	}
	mdPath := filepath.Join(dir, "CLAUDE.md")
	writeFixture(t, mdPath, "# Existing\n")

	out := captureStdout(t, func() {
		if err := runInit([]string{"--repo", "x"}); err != nil {
			t.Fatalf("runInit #1: %v", err)
		}
	})
	if !strings.Contains(out, "Updated: CLAUDE.md") {
		t.Errorf("summary did not report the append:\n%s", out)
	}

	afterFirst := string(mustRead(t, mdPath))
	if want := "# Existing\n\n" + string(block); afterFirst != want {
		t.Errorf("block not appended blank-line separated\n--- got ---\n%s\n--- want ---\n%s", afterFirst, want)
	}
	if n := strings.Count(afterFirst, blockHeading); n != 1 {
		t.Errorf("knomit block heading appears %d times, want 1", n)
	}
	assertNoCompanion(t, dir, "CLAUDE.md")
	assertReInitChangesNothing(t, dir, "--repo", "x")
}

// TestRunInit_ExistingMcpJson_SameKey_UpdatedInPlace: the entry under the
// derived key is ours, so init refreshes it and leaves every other server alone.
func TestRunInit_ExistingMcpJson_SameKey_UpdatedInPlace(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	key := knomitapi.ServerKey("x", "")
	mcpPath := filepath.Join(dir, ".mcp.json")
	writeFixture(t, mcpPath, fmt.Sprintf(`{
  "mcpServers": {
    "other": { "command": "other-server", "args": ["--flag"] },
    %q: { "command": "knomit-bridge", "args": ["--repo", "stale"] }
  }
}
`, key))

	out := captureStdout(t, func() {
		if err := runInit([]string{"--repo", "x"}); err != nil {
			t.Fatalf("runInit: %v", err)
		}
	})

	raw := mustRead(t, mcpPath)
	cfg := parseMcpServers(t, raw)
	if got, want := cfg[key].Args, []string{"--repo", "x"}; !reflect.DeepEqual(got, want) {
		t.Errorf("%s args = %v, want %v — the stale entry was not refreshed", key, got, want)
	}
	if got, want := cfg["other"].Args, []string{"--flag"}; !reflect.DeepEqual(got, want) {
		t.Errorf("an unrelated server was modified: args = %v, want %v", got, want)
	}
	assertNoCompanion(t, dir, ".mcp.json")
	if !strings.Contains(out, "Updated: .mcp.json") {
		t.Errorf("summary did not report the .mcp.json update:\n%s", out)
	}
	assertReInitChangesNothing(t, dir, "--repo", "x")
}

// TestRunInit_ExistingMcpJson_SameKey_PreservesCommandAndExtraKeys splits the
// entry under our own key into the half init owns and the half the user owns.
// `args` are derived from the scope, so init refreshes them. `command` is
// DEPLOYMENT-specific: knomit-bridge is never on $PATH outside the macOS .app,
// so a checkout that points at its own build does so deliberately, and a refresh
// that resets it to the bare name silently stops the MCP server loading. Any
// other key the user added is theirs for the same reason.
func TestRunInit_ExistingMcpJson_SameKey_PreservesCommandAndExtraKeys(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	key := knomitapi.ServerKey("x", "")
	mcpPath := filepath.Join(dir, ".mcp.json")
	existing := fmt.Sprintf(`{
  "mcpServers": {
    %q: {
      "command": "${CLAUDE_PROJECT_DIR:-.}/dist/knomit-bridge",
      "args": ["--repo", "stale"],
      "env": { "KNOMIT_MCP_DEBUG": "1" }
    }
  }
}
`, key)
	writeFixture(t, mcpPath, existing)

	if err := runInit([]string{"--repo", "x"}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	got := mustRead(t, mcpPath)
	cfg := parseMcpServers(t, got)
	if want := []string{"--repo", "x"}; !reflect.DeepEqual(cfg[key].Args, want) {
		t.Errorf("args = %v, want %v — the scope was not refreshed", cfg[key].Args, want)
	}
	// The user's half must survive verbatim, not merely equivalently.
	for _, want := range []string{
		`"command": "${CLAUDE_PROJECT_DIR:-.}/dist/knomit-bridge"`,
		`"env": { "KNOMIT_MCP_DEBUG": "1" }`,
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("merge did not preserve %s byte-for-byte; got:\n%s", want, got)
		}
	}
	// Only args moved, so only args' bytes may differ.
	assertSingleInsertion(t, []byte(strings.Replace(existing, `["--repo", "stale"]`, "", 1)),
		[]byte(strings.Replace(string(got), string(argsBytes(t, got, key)), "", 1)), ".mcp.json")
	assertNoCompanion(t, dir, ".mcp.json")
	assertReInitChangesNothing(t, dir, "--repo", "x")
}

// argsBytes returns the literal `args` value text for a server key, so a test
// can subtract the one region init is allowed to have rewritten.
func argsBytes(t *testing.T, data []byte, key string) []byte {
	t.Helper()
	root, err := indexJSON(data)
	if err != nil {
		t.Fatalf("index .mcp.json: %v", err)
	}
	args := root.child("mcpServers").child(key).child("args")
	if args == nil || !args.span.container() {
		t.Fatalf("no args array under %q", key)
	}
	return data[args.span.open : args.span.close+1]
}

// TestRunInit_ExistingMcpJson_SameKey_KeepsArgsOnOneLine: replacing a value must
// not reflow it. A project that writes `"args": ["--lens", "eng"]` on one line
// gets it back on one line — expanding it to three is the same formatting churn
// this merge exists to avoid, just confined to the one region init may rewrite.
func TestRunInit_ExistingMcpJson_SameKey_KeepsArgsOnOneLine(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	key := knomitapi.ServerKey("", "eng")
	mcpPath := filepath.Join(dir, ".mcp.json")
	writeFixture(t, mcpPath, fmt.Sprintf(`{
  "mcpServers": {
    %q: {
      "command": "knomit-bridge",
      "args": ["--lens", "stale-scope"]
    }
  }
}
`, key))

	if err := runInit([]string{"--lens", "eng"}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	got := string(mustRead(t, mcpPath))
	if !strings.Contains(got, `"args": ["--lens", "eng"]`) {
		t.Errorf("a one-line args array was reflowed; got:\n%s", got)
	}
	if n := strings.Count(got, "\n"); n != 8 {
		t.Errorf("file has %d newlines, want 8 — the merge changed the line structure:\n%s", n, got)
	}
	assertReInitChangesNothing(t, dir, "--lens", "eng")
}

// TestRunInit_ExistingMcpJson_SameKey_CorrectArgs_IsNoOp: an entry already
// naming the right scope is left completely alone — no rewrite, and no Updated
// line, because a summary that reports work it did not do is how an operator
// stops reading the summary.
func TestRunInit_ExistingMcpJson_SameKey_CorrectArgs_IsNoOp(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	key := knomitapi.ServerKey("x", "")
	mcpPath := filepath.Join(dir, ".mcp.json")
	existing := fmt.Sprintf(`{
  "mcpServers": {
    %q: {
      "command": "/opt/knomit/knomit-bridge",
      "args": ["--repo", "x"]
    }
  }
}
`, key)
	writeFixture(t, mcpPath, existing)

	out := captureStdout(t, func() {
		if err := runInit([]string{"--repo", "x"}); err != nil {
			t.Fatalf("runInit: %v", err)
		}
	})

	if got := mustRead(t, mcpPath); !bytes.Equal(got, []byte(existing)) {
		t.Errorf(".mcp.json was rewritten although its args were already correct:\n%s", got)
	}
	if strings.Contains(out, ".mcp.json") {
		t.Errorf("summary mentions .mcp.json although nothing changed:\n%s", out)
	}
	assertNoCompanion(t, dir, ".mcp.json")
}

// TestRunInit_ExistingMcpJson_NoKnomitEntry_AddsEntry: a project with other MCP
// servers and no knomit entry gets one added beside them.
func TestRunInit_ExistingMcpJson_NoKnomitEntry_AddsEntry(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	mcpPath := filepath.Join(dir, ".mcp.json")
	existing := `{
  "mcpServers": {
    "other": { "command": "other-server", "args": ["--flag"] }
  }
}
`
	writeFixture(t, mcpPath, existing)

	out := captureStdout(t, func() {
		if err := runInit([]string{"--repo", "x"}); err != nil {
			t.Fatalf("runInit: %v", err)
		}
	})

	raw := mustRead(t, mcpPath)
	cfg := parseMcpServers(t, raw)
	key := knomitapi.ServerKey("x", "")
	if got, want := cfg[key].Args, []string{"--repo", "x"}; !reflect.DeepEqual(got, want) {
		t.Errorf("%s args = %v, want %v", key, got, want)
	}
	if _, ok := cfg["other"]; !ok {
		t.Errorf("the pre-existing server was dropped:\n%s", raw)
	}
	assertSingleInsertion(t, []byte(existing), raw, ".mcp.json")
	assertNoCompanion(t, dir, ".mcp.json")
	if !strings.Contains(out, "Updated: .mcp.json") {
		t.Errorf("summary did not report the .mcp.json merge:\n%s", out)
	}
	assertReInitChangesNothing(t, dir, "--repo", "x")
}

func parseMcpServers(t *testing.T, raw []byte) map[string]struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
} {
	t.Helper()
	var cfg struct {
		McpServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf(".mcp.json does not parse: %v\n%s", err, raw)
	}
	return cfg.McpServers
}

// TestHookIdentity is the unit behind command-keyed idempotence. Everything
// that reduces to the same identity is the same hook however it was spelled;
// anything else must stay distinct, or a merge would swallow a user's own hook.
func TestHookIdentity(t *testing.T) {
	same := []string{
		"knomit-bridge claude hook post-edit",
		"/usr/local/bin/knomit-bridge claude hook post-edit",
		"${CLAUDE_PROJECT_DIR:-.}/dist/knomit-bridge claude hook post-edit",
		"knomit-bridge.exe claude hook post-edit",
		"knomit-bridge  claude   hook   post-edit",
		"knomit-bridge --log /tmp/b.log claude hook post-edit",
	}
	want := hookIdentity(same[0])
	for _, cmd := range same[1:] {
		if got := hookIdentity(cmd); got != want {
			t.Errorf("hookIdentity(%q) = %q, want %q — the same hook would be registered twice", cmd, got, want)
		}
	}
	for _, cmd := range []string{
		"knomit-bridge claude hook post-ask",
		"knomit-bridge claude hook memory-guard",
		"my-own-hook",
		// Not the bridge: a third-party tool must never be mistaken for ours.
		"some-other-tool claude hook post-edit",
	} {
		if got := hookIdentity(cmd); got == want {
			t.Errorf("hookIdentity(%q) = %q collides with post-edit", cmd, got)
		}
	}
}
