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
	"knomit/tools/bridge/knomitapi"
	"knomit/tools/bridge/skills"
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
	if key := knomitapi.ServerKey(repoName, *lens); len(key) > knomitapi.MaxServerKeyLen {
		scope, flag := repoName, "--repo"
		if *lens != "" {
			scope, flag = *lens, "--lens"
		}
		return fmt.Errorf("derived MCP server key %q is %d characters (max %d), "+
			"because %s name %q is %d (max %d); pass a shorter %s "+
			"(it need not match the directory name)",
			key, len(key), knomitapi.MaxServerKeyLen,
			strings.TrimPrefix(flag, "--"), scope, len(scope), knomitapi.MaxScopeNameLen, flag)
	}

	var created []string
	var overwritten []string
	var conflicts []string

	// writeOne handles one template file: srcFS/srcPath -> its destination,
	// applying the owned-vs-merge-required rules. Shared by both walks.
	writeOne := func(srcFS fs.FS, srcPath, dstRel string) error {
		data, err := fs.ReadFile(srcFS, srcPath)
		if err != nil {
			return err
		}
		// Only *.tmpl is templated; everything else is copied verbatim. A
		// literal `{{` in a SKILL.md is ordinary prose and must not abort a
		// scaffold half-written. See the same rule in the antigravity host.
		rendered := string(data)
		if strings.HasSuffix(srcPath, ".tmpl") {
			rendered, err = renderTemplate(string(data), map[string]string{
				"RepoName":  repoName,
				"Lens":      *lens,
				"ServerKey": knomitapi.ServerKey(repoName, *lens),
			})
			if err != nil {
				return fmt.Errorf("render %s: %w", srcPath, err)
			}
		}
		dst := filepath.Join(cwd, dstRel)
		_, statErr := os.Stat(dst)
		exists := statErr == nil

		if isOwnedByIntegration(dstRel) {
			if err := writeFile(dst, []byte(rendered), 0o644); err != nil {
				return err
			}
			if exists {
				overwritten = append(overwritten, dstRel)
			} else {
				created = append(created, dstRel)
			}
			return nil
		}
		if exists {
			if err := writeFile(companionPath(dst), []byte(rendered), 0o644); err != nil {
				return err
			}
			conflicts = append(conflicts, dstRel)
			return nil
		}
		if err := writeFile(dst, []byte(rendered), 0o644); err != nil {
			return err
		}
		created = append(created, dstRel)
		return nil
	}

	// Walk 1: this package's own templates (.mcp.json, settings.json, CLAUDE.md).
	err = fs.WalkDir(templatesFS, "templates", func(srcPath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(srcPath, "templates/")
		// mcp.json.tmpl and mcp.json.lens.tmpl both target .mcp.json — select
		// exactly one based on whether a lens was requested.
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
		return writeOne(templatesFS, srcPath, dstRel)
	})
	if err != nil {
		return err
	}

	// Walk 2: the shared skill templates, which live in their own package
	// because //go:embed cannot reach a sibling directory.
	err = fs.WalkDir(skills.FS, skills.Root, func(srcPath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(srcPath, skills.Root+"/")
		return writeOne(skills.FS, srcPath, ".claude/skills/"+rel)
	})
	if err != nil {
		return err
	}

	printSummary(created, overwritten, conflicts)
	return nil
}

// isOwnedByIntegration reports whether dstRel is a file that the integration
// owns outright (skills). These are always overwritten on re-run, so deleting
// them and re-running init restores them.
func isOwnedByIntegration(dstRel string) bool {
	return strings.HasPrefix(dstRel, ".claude/skills/")
}

// mapDestination translates a template path under templates/ to its
// destination inside the project. Returns "" if the file should not be copied.
// Skills are NOT handled here; they come from the shared skills package and are
// routed by the second walk in runInit.
func mapDestination(srcPath string) string {
	switch strings.TrimPrefix(srcPath, "templates/") {
	case "mcp.json.tmpl", "mcp.json.lens.tmpl":
		return ".mcp.json"
	case "CLAUDE-md-block.txt":
		return "CLAUDE.md"
	case "settings.json.tmpl":
		return ".claude/settings.json"
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
