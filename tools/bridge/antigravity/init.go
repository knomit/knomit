package antigravity

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

// PluginDir is where the bundle lands, relative to the workspace root.
// Antigravity discovers plugins under <workspace>/.agents/plugins/.
const PluginDir = ".agents/plugins/knomit"

// runInit scaffolds the Antigravity plugin into the current directory.
//
// Every file is OWNED: unlike the Claude Code host there is nothing to merge,
// because no destination is a file the user also writes. So there are no
// companion files, no block markers, and no ownership predicate — init
// overwrites the whole tree on every run.
func runInit(args []string) error {
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	repo := flags.String("repo", "", "knomit repo name (defaults to directory basename)")
	lens := flags.String("lens", "", "lens name; writes a lens-scoped config (mutually exclusive with --repo)")
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

	// Validate BEFORE writing anything, so a value containing quotes or other
	// JSON-hostile characters is rejected up front rather than baked into a
	// broken mcp_config.json. The grammar lives in internal/repos.
	const nameRule = "must be lowercase letters, digits, hyphens, or underscores"
	if *lens != "" {
		if !repos.IsValidName(*lens) {
			return fmt.Errorf("invalid --lens %q (%s)", *lens, nameRule)
		}
	} else if !repos.IsValidName(repoName) {
		return fmt.Errorf("invalid --repo %q (%s)", repoName, nameRule)
	}
	// NOTE: knomitapi.MaxServerKeyLen is deliberately NOT enforced here. That
	// bound exists so the fully-qualified tool name Claude Code derives
	// (mcp__<key>__knomit_hypothesize) stays under the API's 64-character
	// limit. Antigravity exposes MCP tools under bare names, so the budget does
	// not apply — enforcing it rejected any project directory of 28+ characters
	// with an error whose stated reason was about a different host entirely.

	data := map[string]string{
		"RepoName":  repoName,
		"Lens":      *lens,
		"ServerKey": knomitapi.ServerKey(repoName, *lens),
	}
	root := filepath.Join(cwd, PluginDir)

	// Walk 1: this host's own templates.
	err = fs.WalkDir(templatesFS, "templates", func(srcPath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(srcPath, "templates/")
		// Exactly one of the two mcp_config templates applies.
		switch {
		case rel == "mcp_config.json.tmpl" && *lens != "":
			return nil
		case rel == "mcp_config.json.lens.tmpl" && *lens == "":
			return nil
		}
		dstRel := rel
		if rel == "mcp_config.json.tmpl" || rel == "mcp_config.json.lens.tmpl" {
			dstRel = "mcp_config.json"
		}
		return renderInto(templatesFS, srcPath, filepath.Join(root, dstRel), data)
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
		return renderInto(skills.FS, srcPath, filepath.Join(root, "skills", rel), data)
	})
	if err != nil {
		return err
	}

	printSummary(repoName, *lens)
	return nil
}

// renderInto copies one file to dst, creating parent directories. Only files
// named *.tmpl are run through text/template.
//
// The rest are copied byte-for-byte on purpose. Ten of the fourteen scaffolded
// files are SKILL.md documents that contain no template action, and running
// them through a strict engine made any literal `{{` in prose a hard init
// failure for BOTH hosts — after some files had already been written, with no
// rollback. A skill documenting a tool payload or another agent's templating is
// an ordinary thing to write; it must not break scaffolding.
func renderInto(srcFS fs.FS, srcPath, dst string, data map[string]string) error {
	raw, err := fs.ReadFile(srcFS, srcPath)
	if err != nil {
		return err
	}
	rendered := string(raw)
	if strings.HasSuffix(srcPath, ".tmpl") {
		rendered, err = renderTemplate(string(raw), data)
		if err != nil {
			return fmt.Errorf("render %s: %w", srcPath, err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, []byte(rendered), 0o644)
}

// renderTemplate is strict: templates are //go:embed-bundled and
// author-controlled, so a parse failure is a build-time bug. Returning the
// error makes init abort loudly rather than ship a file with unsubstituted
// {{.Var}} placeholders.
func renderTemplate(tmpl string, data map[string]string) (string, error) {
	t, err := template.New("").Option("missingkey=error").
		Funcs(template.FuncMap{"jsonStr": jsonStr}).Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("parse template: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("execute template: %w", err)
	}
	return buf.String(), nil
}

// jsonStr renders s as a quoted JSON string literal. Defense in depth behind
// name validation. Since the input is always a plain string, json.Marshal
// cannot fail here.
func jsonStr(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

func printSummary(repoName, lens string) {
	scope := "repo " + repoName
	if lens != "" {
		scope = "lens " + lens
	}
	fmt.Printf("Wrote %s/ (bound to %s)\n", PluginDir, scope)
	fmt.Println()
	fmt.Println("Antigravity loads this plugin only when it has a registered workspace.")
	fmt.Println("  Interactive:  run `agy` from this directory — nothing else needed.")
	fmt.Println("  Headless:     pass `--add-dir <this directory>` to every `agy -p` run.")
	fmt.Println("Without it, agy silently loads no knomit skills, rules, hooks, or MCP tools.")
}
