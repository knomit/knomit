package claude

import (
	"bytes"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/template"
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
	source := flags.String("source", "", "source-code slug to bake into .mcp.json (required)")
	profile := flags.String("profile", "code", "MCP profile (code, chat, generic)")
	if err := flags.Parse(args); err != nil {
		return err
	}

	if *source == "" {
		return fmt.Errorf("--source is required (the source-code slug used in src:// refs)")
	}

	switch *profile {
	case "code", "chat", "generic":
		// ok
	default:
		return fmt.Errorf("invalid profile %q (must be code, chat, or generic)", *profile)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	repoName := *repo
	if repoName == "" {
		repoName = filepath.Base(cwd)
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
		dstRel := mapDestination(srcPath)
		if dstRel == "" {
			return nil // template excluded
		}
		dst := filepath.Join(cwd, dstRel)

		data, err := templatesFS.ReadFile(srcPath)
		if err != nil {
			return err
		}
		rendered, err := renderTemplate(string(data), map[string]string{"RepoName": repoName, "Profile": *profile, "Source": *source})
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
	case "mcp.json.tmpl":
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
	t, err := template.New("").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("parse template: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("execute template: %w", err)
	}
	return buf.String(), nil
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
	}
}

func companionRel(c string) string {
	if c == "CLAUDE.md" {
		return c + ".knomit-block"
	}
	return c + ".knomit"
}
