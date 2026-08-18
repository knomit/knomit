package claude

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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

func TestRunInit_ExistingMcpJson_DropsCompanion(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(`{"mcpServers":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := runInit([]string{"--repo", "x"}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	companion := filepath.Join(dir, ".mcp.json.knomit")
	if _, err := os.Stat(companion); err != nil {
		t.Errorf("expected companion file at %s: %v", companion, err)
	}

	// Original .mcp.json should be untouched
	orig, _ := os.ReadFile(filepath.Join(dir, ".mcp.json"))
	if !strings.Contains(string(orig), `"mcpServers":{}`) {
		t.Errorf("original .mcp.json was modified; got:\n%s", orig)
	}
}

func TestRunInit_ExistingClaudeMd_DropsBlockCompanion(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("# Existing\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := runInit([]string{"--repo", "x"}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	companion := filepath.Join(dir, "CLAUDE.md.knomit-block")
	if _, err := os.Stat(companion); err != nil {
		t.Errorf("expected companion at %s: %v", companion, err)
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

// TestServerKey pins the derivation. The prefix names the axis and is
// unconditional: see TestServerKey_IsInjective for why both properties matter.
func TestServerKey(t *testing.T) {
	for _, tc := range []struct {
		name, repo, lens, want string
	}{
		{"repo scoping prefixes", "team-kb", "", "knomit-repo-team-kb"},
		{"lens scoping prefixes", "team-kb", "eng", "knomit-lens-eng"},
		{"lens wins over repo", "team-kb", "eng", "knomit-lens-eng"},
		{"repo named knomit still prefixes", "knomit", "", "knomit-repo-knomit"},
		{"already-prefixed name prefixes again", "knomit-web", "", "knomit-repo-knomit-web"},
		{"knomit substring is not special", "knomitten", "", "knomit-repo-knomitten"},
		{"repo named after the other axis", "lens-eng", "", "knomit-repo-lens-eng"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := serverKey(tc.repo, tc.lens); got != tc.want {
				t.Errorf("serverKey(%q, %q) = %q, want %q", tc.repo, tc.lens, got, tc.want)
			}
		})
	}
}

// TestServerKey_IsInjective pins the property the whole derivation exists for:
// distinct scopes must never produce the same .mcp.json key. Duplicate keys in
// one object silently drop an entry, which is exactly the clobbering the derived
// key replaced — so a collision here is not cosmetic.
//
// Two tempting simplifications both break it, and both are covered:
//
//   - Sharing one `knomit-` prefix across the axes. Repos and lenses are
//     separate namespaces validated by the same repos.IsValidName, so a repo and
//     a lens may share a name; `--repo eng` and `--lens eng` would then collide.
//   - Skipping the prefix when the name already carries it, to avoid
//     `knomit-repo-knomit`. That is many-to-one by construction.
//
// The name set below is deliberately adversarial: names that already carry the
// product prefix, and names that spell the OTHER axis marker.
func TestServerKey_IsInjective(t *testing.T) {
	names := []string{
		"eng", "web", "knomit", "knomit-web", "knomitten",
		"lens-eng", "repo-eng", "lens", "repo", "knomit-lens-eng",
	}
	type scope struct{ repo, lens string }
	seen := make(map[string]scope, 2*len(names))
	for _, n := range names {
		for _, s := range []scope{{repo: n}, {repo: "unrelated", lens: n}} {
			key := serverKey(s.repo, s.lens)
			if prev, dup := seen[key]; dup {
				t.Errorf("key %q derived from two distinct scopes: {repo:%q lens:%q} and {repo:%q lens:%q}",
					key, prev.repo, prev.lens, s.repo, s.lens)
				continue
			}
			seen[key] = s
		}
	}
	// The specific collision the axis prefix exists to prevent, asserted
	// directly so a regression names itself rather than surfacing as a map hit.
	if repoKey, lensKey := serverKey("eng", ""), serverKey("eng", "eng"); repoKey == lensKey {
		t.Errorf("repo %q and lens %q both derive %q", "eng", "eng", repoKey)
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
	long := strings.Repeat("a", maxServerKeyLen)
	err := runInit([]string{"--repo", long})
	if err == nil {
		t.Fatalf("runInit accepted a repo name yielding a %d-char key", len(serverKey(long, "")))
	}
	if !strings.Contains(err.Error(), "server key") {
		t.Errorf("error %q does not explain the key-length limit", err)
	}
	// The remedy has to be actionable: repo mode can trip this with no flag at
	// all (the name defaults to the directory basename), so the message must
	// quote the name budget and say the repo need not match the directory.
	for _, want := range []string{
		fmt.Sprintf("max %d", maxScopeNameLen),
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
	name := strings.Repeat("a", maxScopeNameLen)
	if err := runInit([]string{"--repo", name}); err != nil {
		t.Fatalf("runInit rejected a %d-char repo name at the documented max: %v", len(name), err)
	}
	if got := len(serverKey(name, "")); got != maxServerKeyLen {
		t.Errorf("key for a max-length name is %d chars, want exactly %d — "+
			"maxScopeNameLen and the axis prefix have drifted apart", got, maxServerKeyLen)
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
