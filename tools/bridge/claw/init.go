package claw

import (
	"bytes"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"knomit/tools/bridge/endpoint"
)

//go:embed all:templates
var templatesFS embed.FS

// Plugin runtime is the single source of truth in plugin-src/ (R2): the
// tested files are the shipped files. Embed the five runtime modules
// explicitly so node_modules/ and test/ are never bundled.
//
//go:embed plugin-src/index.mjs plugin-src/tools.mjs plugin-src/register.mjs plugin-src/mcp-client.mjs plugin-src/forward.mjs
var pluginFS embed.FS

// pluginRuntimeFiles are the plugin-src modules copied verbatim into the
// scaffolded plugin directory on every init.
var pluginRuntimeFiles = []string{"index.mjs", "tools.mjs", "register.mjs", "mcp-client.mjs", "forward.mjs"}

type initOptions struct {
	repo, source, profile, scope string
	// snapshot returns (knomit-tools.json bytes, profile Instructions, error).
	// Injectable for tests; nil means use the live endpoint path.
	snapshot func() ([]byte, string, error)
}

// runInit parses flags and calls runInitWith with the live snapshot.
func runInit(args []string) error {
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	repo := flags.String("repo", "", "knomit repo name (defaults to directory basename)")
	source := flags.String("source", "", "source-code slug to bake into openclaw.json (required)")
	profile := flags.String("profile", "code", "MCP profile (code, chat, generic)")
	scope := flags.String("scope", "project", "scaffold scope (project, user)")
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

	switch *scope {
	case "project", "user":
		// ok
	default:
		return fmt.Errorf("invalid scope %q (must be project or user)", *scope)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	repoName := *repo
	if repoName == "" {
		repoName = filepath.Base(cwd)
	}

	opts := initOptions{
		repo:    repoName,
		source:  *source,
		profile: *profile,
		scope:   *scope,
	}
	return runInitWith(opts)
}

// liveSnapshot discovers the server and snapshots tools + instructions.
func liveSnapshot(repo, profile string) func() ([]byte, string, error) {
	return func() ([]byte, string, error) {
		base := "http://localhost:19278"
		if u, err := endpoint.ReadLockfileBaseURL(); err == nil && u != "" {
			base = u
		}
		branch, err := endpoint.DiscoverAgentBranch(base, repo)
		if err != nil {
			return nil, "", fmt.Errorf("discover agent branch: %w", err)
		}
		url := endpoint.ServerURL(base, repo, branch, profile)
		manifest, err := SnapshotTools(url, &http.Client{})
		if err != nil {
			return nil, "", err
		}
		instr, err := SnapshotInstructions(url, &http.Client{})
		if err != nil {
			return nil, "", err
		}
		return manifest, instr, nil
	}
}

// runInitWith performs the scaffolding given resolved options.
func runInitWith(opts initOptions) error {
	snap := opts.snapshot
	if snap == nil {
		snap = liveSnapshot(opts.repo, opts.profile)
	}
	manifest, instructions, err := snap()
	if err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	root := destRoot(cwd, opts.scope)
	tmplData := map[string]string{
		"RepoName":     opts.repo,
		"Source":       opts.source,
		"Instructions": instructions,
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
		dstRel := mapDestination(srcPath, opts.scope)
		if dstRel == "" {
			return nil // template excluded
		}
		dst := filepath.Join(root, dstRel)

		raw, err := templatesFS.ReadFile(srcPath)
		if err != nil {
			return err
		}
		rendered, err := renderTemplate(string(raw), tmplData)
		if err != nil {
			return fmt.Errorf("render %s: %w", srcPath, err)
		}

		mode := fs.FileMode(0o644)

		_, statErr := os.Stat(dst)
		exists := statErr == nil

		if isOwnedByIntegration(dstRel) {
			// Always write owned files (skills, plugin files).
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

	// 2. Write the embedded plugin runtime (R2) into the plugin dir.
	pluginDir := pluginDestDir(cwd, opts.scope)
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		return err
	}
	for _, name := range pluginRuntimeFiles {
		b, err := pluginFS.ReadFile("plugin-src/" + name)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(pluginDir, name), b, 0o644); err != nil {
			return err
		}
	}

	// 3. Write the manifest next to the plugin.
	if err := os.WriteFile(filepath.Join(pluginDir, "knomit-tools.json"), manifest, 0o644); err != nil {
		return err
	}

	// 4. Write bridge-config.json so the runtime spawns knomit-bridge with the
	//    SAME repo/source/profile this init used (else the plugin connects to
	//    the default repo=core/profile=code, not the scaffolded one).
	cfg, err := json.Marshal(map[string]string{
		"repo": opts.repo, "source": opts.source, "profile": opts.profile,
	})
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "bridge-config.json"), cfg, 0o644); err != nil {
		return err
	}

	printSummary(created, overwritten, conflicts, pluginDir)
	return nil
}

// isOwnedByIntegration reports whether dstRel is a file that the integration
// owns outright (skills, plugin package files). These are always overwritten
// on re-run, so deleting them and re-running init restores them.
func isOwnedByIntegration(dstRel string) bool {
	return strings.Contains(dstRel, "skills"+string(filepath.Separator)) ||
		strings.Contains(dstRel, "openclaw-plugins"+string(filepath.Separator)) ||
		strings.Contains(dstRel, filepath.Join(".openclaw", "extensions"))
}

// destRoot resolves the root directory scope-relative destinations are
// joined against. project: cwd. user: the user's home directory (so
// "openclaw.json" etc. land under ~ rather than the invocation directory).
func destRoot(cwd, scope string) string {
	if scope == "user" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
	}
	return cwd
}

// mapDestination translates a template path under templates/ to its
// destination path, relative to destRoot(cwd, scope). Returns "" if the
// file should not be copied by the walk.
func mapDestination(srcPath, scope string) string {
	rel := strings.TrimPrefix(srcPath, "templates/")

	if rel == "openclaw.json.tmpl" {
		if scope == "user" {
			// OpenClaw reads ~/.openclaw/openclaw.json for user-scope config,
			// not a bare ~/openclaw.json.
			return filepath.Join(".openclaw", "openclaw.json")
		}
		return "openclaw.json"
	}
	if strings.HasPrefix(rel, "skills/") {
		return filepath.Join(skillsRelDir(scope), strings.TrimPrefix(rel, "skills/"))
	}
	if strings.HasPrefix(rel, "plugin/") {
		name := strings.TrimSuffix(strings.TrimPrefix(rel, "plugin/"), ".tmpl")
		return filepath.Join(pluginRelDir(scope), name)
	}
	return ""
}

// skillsRelDir is the skills directory relative to destRoot(cwd, scope).
// project: .agents/skills ; user: ~/.openclaw/skills.
func skillsRelDir(scope string) string {
	if scope == "user" {
		return filepath.Join(".openclaw", "skills")
	}
	return filepath.Join(".agents", "skills")
}

// pluginRelDir is the plugin directory relative to destRoot(cwd, scope).
// project: openclaw-plugins/knomit ; user: .openclaw/extensions/knomit.
func pluginRelDir(scope string) string {
	if scope == "user" {
		return filepath.Join(".openclaw", "extensions", "knomit")
	}
	return filepath.Join("openclaw-plugins", "knomit")
}

// pluginDestDir resolves the absolute directory the plugin package (and its
// runtime .mjs files, manifest, and bridge-config) are written to.
// project: <cwd>/openclaw-plugins/knomit ; user: ~/.openclaw/extensions/knomit.
func pluginDestDir(cwd, scope string) string {
	return filepath.Join(destRoot(cwd, scope), pluginRelDir(scope))
}

func companionPath(dst string) string {
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

func printSummary(created, overwritten, conflicts []string, pluginDir string) {
	if len(created) > 0 {
		fmt.Printf("Created: %s\n", strings.Join(created, ", "))
	}
	if len(overwritten) > 0 {
		fmt.Printf("Restored: %s\n", strings.Join(overwritten, ", "))
	}
	for _, c := range conflicts {
		fmt.Printf("WARNING: %s exists — merge from %s manually\n", c, c+".knomit")
	}
	fmt.Printf("Next: run `npm install` in %s before starting OpenClaw.\n", pluginDir)
}
