package claude

import (
	"bytes"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"knomit/internal/repos"
)

//go:embed all:templates
var templatesFS embed.FS

// runInit scaffolds CC-side integration files into the current directory.
//
// Semantics:
//   - Owned files (.claude/skills/**): always written (overwritten if they
//     already exist).
//   - Merge-required files (.mcp.json, .claude/settings.json, CLAUDE.md):
//     if the destination exists, a companion file is dropped instead.
func runInit(args []string) error {
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	repo := flags.String("repo", "", "knomit repo name (defaults to directory basename)")
	lens := flags.String("lens", "", "lens name; writes a lens-scoped .mcp.json (mutually exclusive with --repo)")
	if err := flags.Parse(args); err != nil {
		return err
	}

	if *lens != "" && *repo != "" {
		return fmt.Errorf("--lens and --repo are mutually exclusive")
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	repoName := *repo
	if repoName == "" {
		repoName = filepath.Base(cwd)
	}

	// Validate names against the server's grammar BEFORE writing any file, so a
	// value containing quotes, backslashes, or other JSON-hostile characters is
	// rejected up front rather than silently baked into a broken .mcp.json. The
	// grammar lives in internal/repos (single source of truth); the bridge does
	// not duplicate it.
	const nameRule = "must be lowercase letters, digits, hyphens, or underscores"
	if *lens != "" {
		if !repos.IsValidName(*lens) {
			return fmt.Errorf("invalid --lens %q (%s)", *lens, nameRule)
		}
	} else {
		if !repos.IsValidName(repoName) {
			return fmt.Errorf("invalid --repo %q (%s)", repoName, nameRule)
		}
	}
	// Reject up front rather than emit a .mcp.json whose derived key produces a
	// tool name over the API's 64-char limit. Repo mode can trip this without
	// the user naming anything: repoName defaults to the directory basename, so
	// a long directory name fails an otherwise flagless `claude init`. Say so —
	// the remedy (name the repo explicitly) is not obvious from the error alone,
	// because nothing else in the tool requires the repo to match the directory.
	if key := serverKey(repoName, *lens); len(key) > maxServerKeyLen {
		scope, flag := repoName, "--repo"
		if *lens != "" {
			scope, flag = *lens, "--lens"
		}
		return fmt.Errorf("derived MCP server key %q is %d characters (max %d), "+
			"because %s name %q is %d (max %d); pass a shorter %s "+
			"(it need not match the directory name)",
			key, len(key), maxServerKeyLen,
			strings.TrimPrefix(flag, "--"), scope, len(scope), maxScopeNameLen, flag)
	}

	var created []string
	var overwritten []string
	var conflicts []string

	err = fs.WalkDir(templatesFS, "templates", func(srcPath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		// mcp.json.tmpl and mcp.json.lens.tmpl both target .mcp.json — select
		// exactly one based on whether a lens was requested.
		rel := strings.TrimPrefix(srcPath, "templates/")
		if rel == "mcp.json.tmpl" && *lens != "" {
			return nil
		}
		if rel == "mcp.json.lens.tmpl" && *lens == "" {
			return nil
		}
		dstRel := mapDestination(srcPath)
		if dstRel == "" {
			return nil // template excluded
		}
		dst := filepath.Join(cwd, dstRel)

		data, err := templatesFS.ReadFile(srcPath)
		if err != nil {
			return err
		}
		rendered, err := renderTemplate(string(data), map[string]string{
			"RepoName":  repoName,
			"Lens":      *lens,
			"ServerKey": serverKey(repoName, *lens),
		})
		if err != nil {
			return fmt.Errorf("render %s: %w", srcPath, err)
		}

		mode := fs.FileMode(0o644)

		_, statErr := os.Stat(dst)
		exists := statErr == nil

		if isOwnedByIntegration(dstRel) {
			// Always write owned files (skills).
			if err := writeFile(dst, []byte(rendered), mode); err != nil {
				return err
			}
			if exists {
				overwritten = append(overwritten, dstRel)
			} else {
				created = append(created, dstRel)
			}
			return nil
		}

		// Merge-required file: write companion if destination exists.
		if exists {
			companion := companionPath(dst)
			if err := writeFile(companion, []byte(rendered), mode); err != nil {
				return err
			}
			conflicts = append(conflicts, dstRel)
			return nil
		}

		if err := writeFile(dst, []byte(rendered), mode); err != nil {
			return err
		}
		created = append(created, dstRel)
		return nil
	})
	if err != nil {
		return err
	}

	printSummary(created, overwritten, conflicts)
	return nil
}

// serverKey derives the `.mcp.json` mcpServers key — which Claude Code turns
// into the tool-name prefix `mcp__<key>__knomit_learn`.
//
// It is DERIVED rather than the constant "knomit" because the constant made the
// scaffolding structurally single-server: a second `claude init` in the same
// project collided on the key, so two knomit servers could never coexist. A lens
// scoping wins over a repo scoping because the two are mutually exclusive at the
// flag layer and the lens is the thing actually being served.
//
// The prefix names the AXIS as well as the product, and is applied
// UNCONDITIONALLY, so the mapping from scope to key is injective: distinct
// scopes can never collide. Both halves are load-bearing.
//
// Naming the axis is what makes the two namespaces disjoint. Repos and lenses
// are separate namespaces validated by the same repos.IsValidName, so nothing
// stops a lens and a repo sharing a name — a lens named after the repo it
// writes to is the natural naming — and a shared `knomit-` prefix would map
// `--repo eng` and `--lens eng` to the same key. Since `knomit-repo-` and
// `knomit-lens-` are fixed-length and differ, no repo key can ever equal a lens
// key, whatever the names (a repo literally named `lens-eng` yields
// `knomit-repo-lens-eng`, not `knomit-lens-eng`). See TestServerKey_IsInjective.
//
// Applying it unconditionally is the other half. An earlier draft skipped the
// prefix when the name already carried it, to avoid the ugly `knomit-knomit`
// for a repo named `knomit` — but that rule is inherently many-to-one
// (`web` and `knomit-web` both map to `knomit-web`), which re-creates in one
// step exactly the clobbering this function exists to remove. A cosmetic
// objection does not outrank the correctness property.
//
// Skipping the prefix bought no backward compatibility either: `.mcp.json` is
// merge-required, so an existing config is never rewritten in place — init
// drops a companion file and lets the user merge.
//
// Callers must validate name/lens with repos.IsValidName first: the result is
// interpolated into JSON, and this function does no escaping of its own.
func serverKey(repoName, lens string) string {
	if lens != "" {
		return "knomit-lens-" + lens
	}
	return "knomit-repo-" + repoName
}

// maxServerKeyLen bounds the derived key so the fully-qualified tool name
// Claude Code builds from it stays under the API's 64-character tool-name
// limit. The longest tool is knomit_hypothesize, giving
// len("mcp__") + len(key) + len("__") + len("knomit_hypothesize") = 25 + key.
//
// Bytes, not runes: repos.IsValidName restricts names to ASCII, so the two
// counts coincide and the byte length is what the API actually measures.
//
// This could not be hit before: the key was a 6-character constant. It can now,
// because the key derives from a repo name that defaults to the directory
// basename, and repos.IsValidName constrains the character set but not length.
const maxServerKeyLen = 64 - len("mcp____knomit_hypothesize")

// maxScopeNameLen is the resulting budget for the repo or lens NAME itself —
// what the error message must quote, since that is the knob the user turns.
// Both axis prefixes are the same length, so one constant covers both.
const maxScopeNameLen = maxServerKeyLen - len("knomit-repo-")

// isOwnedByIntegration reports whether dstRel is a file that the integration
// owns outright (skills). These are always overwritten on re-run, so deleting
// them and re-running init restores them.
func isOwnedByIntegration(dstRel string) bool {
	return strings.HasPrefix(dstRel, ".claude/skills/")
}

// mapDestination translates a template path under templates/ to its
// destination path inside the project. Returns "" if the file should not
// be copied.
func mapDestination(srcPath string) string {
	rel := strings.TrimPrefix(srcPath, "templates/")
	switch rel {
	case "mcp.json.tmpl", "mcp.json.lens.tmpl":
		return ".mcp.json"
	case "CLAUDE-md-block.txt":
		return "CLAUDE.md"
	case "settings.json.tmpl":
		return ".claude/settings.json"
	}
	if strings.HasPrefix(rel, "skills/") {
		return ".claude/" + rel
	}
	return ""
}

func companionPath(dst string) string {
	if strings.HasSuffix(dst, "CLAUDE.md") {
		return dst + ".knomit-block"
	}
	return dst + ".knomit"
}

// renderTemplate is strict: templates are //go:embed-bundled and
// author-controlled, so a parse failure is a build-time bug, not a runtime
// condition to paper over. Return the error so init aborts loudly instead of
// shipping a file with unsubstituted {{.Var}} placeholders.
func renderTemplate(tmpl string, data map[string]string) (string, error) {
	t, err := template.New("").Option("missingkey=error").Funcs(template.FuncMap{"jsonStr": jsonStr}).Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("parse template: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("execute template: %w", err)
	}
	return buf.String(), nil
}

// jsonStr renders s as a quoted JSON string literal (including the surrounding
// double quotes). Used by .mcp.json templates so any value substituted into the
// JSON is properly escaped — defense in depth behind name validation. Since the
// input is always a plain string, json.Marshal cannot fail here.
func jsonStr(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		// Unreachable for string input; fall back to an obviously-invalid token
		// so a hypothetical failure surfaces loudly rather than silently.
		return `""`
	}
	return string(b)
}

func writeFile(path string, data []byte, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, mode)
}

func printSummary(created, overwritten, conflicts []string) {
	if len(created) > 0 {
		fmt.Printf("Created: %s\n", strings.Join(created, ", "))
	}
	if len(overwritten) > 0 {
		fmt.Printf("Restored: %s\n", strings.Join(overwritten, ", "))
	}
	for _, c := range conflicts {
		fmt.Printf("WARNING: %s exists — merge from %s manually\n", c, companionRel(c))
		switch c {
		case "CLAUDE.md":
			if note := claudeMdBlockNote(c); note != "" {
				fmt.Printf("         %s\n", note)
			}
		case ".mcp.json":
			// Deriving the key makes two knomit servers MERGEABLE, and the
			// obvious merge — both entries side by side — can disable the
			// hooks, since two DIFFERENT scopes leave no single repo to bind
			// to. (Two entries naming the same scope are fine; mcpBinding
			// treats them as one.) Say so here; the user finds out otherwise
			// only by noticing that knomit went quiet.
			fmt.Println("         note: keep ONE knomit SCOPE — a project whose knomit entries")
			fmt.Println("         name two different repos or lenses has no single repo to bind")
			fmt.Println("         to, so the hooks disable themselves (the MCP tools still work")
			fmt.Println("         for both). Duplicate entries naming the same scope are fine.")
		}
	}
}

// blockMarkerPrefix makes the integration block's version legible to init.
// Without a consumer the version in the marker would be inert decoration: a
// stale installed block would be indistinguishable from a current one, which is
// the whole reason the block drifted (installed copies saying "Nine /knomit-…
// slash commands" against a template saying "Eleven").
const (
	blockMarkerPrefix = "<!-- knomit:integration"
	// blockHeading identifies a block whose marker is missing entirely —
	// pre-marker installs, or a user who stripped the HTML comments.
	blockHeading = "## Working with knomit memory"
	// claudeMdBlockTemplate is the embedded block whose marker defines "current".
	claudeMdBlockTemplate = "templates/CLAUDE-md-block.txt"
)

// blockMarkerCurrent is the marker of the block THIS build ships, read off the
// embedded template rather than restated as a constant here. A hand-written copy
// drifts silently in BOTH directions: bump the template to v3 and forget the
// copy, and claudeMdBlockNote calls a freshly merged current block "from an
// older version"; bump the copy and forget the template, and every install is
// reported current forever. That is the same "the marker drifted because nothing
// read it" failure the marker exists to prevent, moved one level up.
//
// Empty if the template has no marker line. That disables only the
// already-current shortcut — claudeMdBlockNote guards against the empty string,
// since strings.Contains(anything, "") is true and would report every CLAUDE.md
// as current. TestBlockMarkerCurrent_ComesFromTemplate turns the condition into
// a test failure rather than a silent degradation.
var blockMarkerCurrent = templateBlockMarker()

func templateBlockMarker() string {
	data, err := templatesFS.ReadFile(claudeMdBlockTemplate)
	if err != nil {
		return ""
	}
	first, _, _ := strings.Cut(string(data), "\n")
	first = strings.TrimSpace(first)
	if !strings.HasPrefix(first, blockMarkerPrefix) {
		return ""
	}
	return first
}

// claudeMdBlockNote reports how the CLAUDE.md already on disk compares to the
// block this build ships, so the merge warning says WHAT to merge. Returns ""
// when the file is unreadable or already current — nothing useful to add.
func claudeMdBlockNote(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	content := string(data)
	switch {
	case blockMarkerCurrent != "" && strings.Contains(content, blockMarkerCurrent):
		return ""
	case strings.Contains(content, blockMarkerPrefix), strings.Contains(content, blockHeading):
		// The heading is checked too: the template shipped without the HTML
		// marker until c36015e7, and users strip comments. Calling such a block
		// absent would advise appending a SECOND full copy.
		return "its knomit block is from an older version — replace the whole block, not just parts"
	default:
		return "it has no knomit block yet — append the companion's contents"
	}
}

func companionRel(c string) string {
	if c == "CLAUDE.md" {
		return c + ".knomit-block"
	}
	return c + ".knomit"
}
