package main

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
// Conflict handling: existing files with the same name get a `.knomit`
// companion file dropped next to them for the user to merge.
func runInit(args []string) error {
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	repo := flags.String("repo", "", "knomit repo name (defaults to directory basename)")
	if err := flags.Parse(args); err != nil {
		return err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	repoName := *repo
	if repoName == "" {
		repoName = filepath.Base(cwd)
	}

	// Idempotency: if .mcp.json already declares mcpServers.knomit and
	// CLAUDE.md already contains the integration marker, skip everything.
	if alreadyIntegrated(cwd) {
		fmt.Println("knomit-bridge: already integrated; nothing to do")
		return nil
	}

	var created []string
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
		rendered, err := renderTemplate(string(data), map[string]string{"RepoName": repoName})
		if err != nil {
			return fmt.Errorf("render %s: %w", srcPath, err)
		}

		mode := fs.FileMode(0o644)
		if strings.HasSuffix(srcPath, ".sh") {
			mode = 0o755
		}

		_, statErr := os.Stat(dst)
		if statErr == nil {
			// File exists — write companion instead of overwriting.
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

	printSummary(created, conflicts)
	return nil
}

// mapDestination translates a template path under templates/ to its
// destination path inside the project. Returns "" if the file should not
// be copied.
func mapDestination(srcPath string) string {
	rel := strings.TrimPrefix(srcPath, "templates/")
	switch rel {
	case ".mcp.json.tmpl":
		return ".mcp.json"
	case "CLAUDE-md-block.txt":
		return "CLAUDE.md"
	case "claude/settings.json.tmpl":
		return ".claude/settings.json"
	}
	if strings.HasPrefix(rel, "claude/") {
		return ".claude/" + strings.TrimPrefix(rel, "claude/")
	}
	return rel
}

func companionPath(dst string) string {
	if strings.HasSuffix(dst, "CLAUDE.md") {
		return dst + ".knomit-block"
	}
	return dst + ".knomit"
}

func renderTemplate(tmpl string, data map[string]string) (string, error) {
	t, err := template.New("").Parse(tmpl)
	if err != nil {
		// Not a valid template — return as-is.
		return tmpl, nil
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func writeFile(path string, data []byte, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, mode)
}

func alreadyIntegrated(cwd string) bool {
	if mcp, err := os.ReadFile(filepath.Join(cwd, ".mcp.json")); err == nil {
		if bytes.Contains(mcp, []byte(`"knomit"`)) {
			return true
		}
	}
	if cmd, err := os.ReadFile(filepath.Join(cwd, "CLAUDE.md")); err == nil {
		if bytes.Contains(cmd, []byte("<!-- knomit:integration -->")) {
			return true
		}
	}
	return false
}

func printSummary(created, conflicts []string) {
	if len(created) > 0 {
		fmt.Printf("Created: %s\n", strings.Join(created, ", "))
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
