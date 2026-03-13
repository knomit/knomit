# TOML Configuration Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add layered TOML-based configuration (defaults → knomit.toml → env vars) with configurable ontology root.

**Architecture:** Section config structs live in their consuming packages (`llm.Config`, `git.Config`). Root `config.Config` composes them. `config.Load()` applies three layers: code defaults, optional TOML file, env var overrides. The ontology root (`"know"` by default) is propagated to MCP handlers and web handlers via config.

**Tech Stack:** Go, `github.com/BurntSushi/toml`

**Spec:** `docs/superpowers/specs/2026-03-12-toml-config-design.md`

---

## Chunk 1: Config structs and loading

### Task 1: Add BurntSushi/toml dependency

**Files:**
- Modify: `go.mod`

- [ ] **Step 1: Add the dependency**

Run: `go get github.com/BurntSushi/toml`

- [ ] **Step 2: Verify it's in go.mod**

Run: `grep BurntSushi go.mod`
Expected: `github.com/BurntSushi/toml v...`

- [ ] **Step 3: Commit**

```bash
git add go.mod go.sum
git commit -m "deps: add BurntSushi/toml for config file support"
```

---

### Task 2: Create llm.Config and git.Config section structs

**Files:**
- Create: `internal/llm/config.go`
- Create: `internal/git/config.go`

- [ ] **Step 1: Create `internal/llm/config.go`**

```go
package llm

// Config holds LLM-related configuration.
type Config struct {
	Model    string `toml:"model"`
	Provider string `toml:"provider"`
	APIKey   string `toml:"api_key"`
}

// DefaultConfig returns LLM config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		Model: "claude-sonnet-4-6",
	}
}
```

- [ ] **Step 2: Create `internal/git/config.go`**

```go
package git

// Config holds git-related configuration.
type Config struct {
	Remote bool   `toml:"remote"`
	Port   string `toml:"port"`
}

// DefaultConfig returns git config with sensible defaults.
func DefaultConfig() Config {
	return Config{}
}
```

- [ ] **Step 3: Verify compilation**

Run: `go build ./internal/llm/ ./internal/git/`
Expected: success

- [ ] **Step 4: Commit**

```bash
git add internal/llm/config.go internal/git/config.go
git commit -m "feat(config): add LLM and git section config structs"
```

---

### Task 3: Write tests for config.Load()

**Files:**
- Create: `internal/config/config_test.go`

- [ ] **Step 1: Write tests**

```go
package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaults(t *testing.T) {
	cfg := Defaults()
	if cfg.Port != "3000" {
		t.Errorf("Port = %q, want %q", cfg.Port, "3000")
	}
	if cfg.OntologyRoot != "know" {
		t.Errorf("OntologyRoot = %q, want %q", cfg.OntologyRoot, "know")
	}
	if cfg.LLM.Model != "claude-sonnet-4-6" {
		t.Errorf("LLM.Model = %q, want %q", cfg.LLM.Model, "claude-sonnet-4-6")
	}
	home, _ := os.UserHomeDir()
	if cfg.RepoPath != home+"/.knomit" {
		t.Errorf("RepoPath = %q, want %q", cfg.RepoPath, home+"/.knomit")
	}
	if cfg.CacheDir != home+"/.cache/knomit" {
		t.Errorf("CacheDir = %q, want %q", cfg.CacheDir, home+"/.cache/knomit")
	}
}

func TestLoadFromTOML(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "knomit.toml")
	os.WriteFile(tomlPath, []byte(`
port = "4000"
ontology_root = "facts"

[llm]
model = "gemini-2.0-flash"
provider = "gemini"
`), 0o644)

	// Point KNOMIT_REPO at this dir so Load() finds the TOML file there.
	t.Setenv("KNOMIT_REPO", dir)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Port != "4000" {
		t.Errorf("Port = %q, want %q", cfg.Port, "4000")
	}
	if cfg.OntologyRoot != "facts" {
		t.Errorf("OntologyRoot = %q, want %q", cfg.OntologyRoot, "facts")
	}
	if cfg.LLM.Model != "gemini-2.0-flash" {
		t.Errorf("LLM.Model = %q, want %q", cfg.LLM.Model, "gemini-2.0-flash")
	}
	if cfg.LLM.Provider != "gemini" {
		t.Errorf("LLM.Provider = %q, want %q", cfg.LLM.Provider, "gemini")
	}
}

func TestEnvOverridesToml(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "knomit.toml")
	os.WriteFile(tomlPath, []byte(`
port = "4000"

[llm]
model = "gemini-2.0-flash"
`), 0o644)

	t.Setenv("KNOMIT_REPO", dir)
	t.Setenv("KNOMIT_PORT", "5000")
	t.Setenv("KNOMIT_LLM_MODEL", "claude-opus-4-6")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Port != "5000" {
		t.Errorf("Port = %q, want %q (env should override TOML)", cfg.Port, "5000")
	}
	if cfg.LLM.Model != "claude-opus-4-6" {
		t.Errorf("LLM.Model = %q, want %q (env should override TOML)", cfg.LLM.Model, "claude-opus-4-6")
	}
}

func TestLoadNoTOMLFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KNOMIT_REPO", dir)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	// Should get defaults
	if cfg.Port != "3000" {
		t.Errorf("Port = %q, want %q", cfg.Port, "3000")
	}
}

func TestLoadMalformedTOML(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "knomit.toml"), []byte(`[invalid`), 0o644)
	t.Setenv("KNOMIT_REPO", dir)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() should return error for malformed TOML")
	}
}

func TestTildeExpansion(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "knomit.toml"), []byte(`
cache_dir = "~/cache/knomit"
onnx_lib_path = "~/lib/ort.dylib"

[remote]
ssh_key = "~/.ssh/id_ed25519"
`), 0o644)
	t.Setenv("KNOMIT_REPO", dir)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	home, _ := os.UserHomeDir()
	if cfg.CacheDir != home+"/cache/knomit" {
		t.Errorf("CacheDir = %q, want tilde expanded to %q", cfg.CacheDir, home+"/cache/knomit")
	}
	if cfg.ONNXLibPath != home+"/lib/ort.dylib" {
		t.Errorf("ONNXLibPath = %q, want tilde expanded to %q", cfg.ONNXLibPath, home+"/lib/ort.dylib")
	}
	if cfg.Remote.SSHKey != home+"/.ssh/id_ed25519" {
		t.Errorf("Remote.SSHKey = %q, want tilde expanded to %q", cfg.Remote.SSHKey, home+"/.ssh/id_ed25519")
	}
}

func TestEnvBoolOverridesToml(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "knomit.toml"), []byte(`
[git]
remote = true
`), 0o644)
	t.Setenv("KNOMIT_REPO", dir)
	t.Setenv("KNOMIT_GIT_REMOTE", "false")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Git.Remote {
		t.Error("Git.Remote should be false (env override of TOML true)")
	}
}

func TestDefaultsPreservedWithPartialTOML(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "knomit.toml"), []byte(`
port = "4000"
`), 0o644)
	t.Setenv("KNOMIT_REPO", dir)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Port != "4000" {
		t.Errorf("Port = %q, want %q", cfg.Port, "4000")
	}
	// Defaults should be preserved for fields not in TOML
	if cfg.OntologyRoot != "know" {
		t.Errorf("OntologyRoot = %q, want default %q", cfg.OntologyRoot, "know")
	}
	if cfg.LLM.Model != "claude-sonnet-4-6" {
		t.Errorf("LLM.Model = %q, want default %q", cfg.LLM.Model, "claude-sonnet-4-6")
	}
}

func TestAllEnvVars(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KNOMIT_REPO", dir)
	t.Setenv("KNOMIT_CACHE_DIR", "/tmp/cache")
	t.Setenv("KNOMIT_PORT", "9090")
	t.Setenv("KNOMIT_LLM_MODEL", "test-model")
	t.Setenv("KNOMIT_LLM_PROVIDER", "test-provider")
	t.Setenv("KNOMIT_API_KEY", "sk-test")
	t.Setenv("KNOMIT_GIT_REMOTE", "true")
	t.Setenv("KNOMIT_GIT_PORT", "9418")
	t.Setenv("KNOMIT_REMOTE_TOKEN", "tok")
	t.Setenv("KNOMIT_REMOTE_USER", "usr")
	t.Setenv("KNOMIT_REMOTE_PASSWORD", "pw")
	t.Setenv("KNOMIT_REMOTE_SSH_KEY", "/tmp/key")
	t.Setenv("ONNXRUNTIME_SHARED_LIBRARY", "/tmp/ort.so")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.RepoPath != dir {
		t.Errorf("RepoPath = %q, want %q", cfg.RepoPath, dir)
	}
	if cfg.CacheDir != "/tmp/cache" {
		t.Errorf("CacheDir = %q", cfg.CacheDir)
	}
	if cfg.Port != "9090" {
		t.Errorf("Port = %q", cfg.Port)
	}
	if cfg.LLM.Model != "test-model" {
		t.Errorf("LLM.Model = %q", cfg.LLM.Model)
	}
	if cfg.LLM.Provider != "test-provider" {
		t.Errorf("LLM.Provider = %q", cfg.LLM.Provider)
	}
	if cfg.LLM.APIKey != "sk-test" {
		t.Errorf("LLM.APIKey = %q", cfg.LLM.APIKey)
	}
	if !cfg.Git.Remote {
		t.Error("Git.Remote should be true")
	}
	if cfg.Git.Port != "9418" {
		t.Errorf("Git.Port = %q", cfg.Git.Port)
	}
	if cfg.Remote.Token != "tok" {
		t.Errorf("Remote.Token = %q", cfg.Remote.Token)
	}
	if cfg.Remote.User != "usr" {
		t.Errorf("Remote.User = %q", cfg.Remote.User)
	}
	if cfg.Remote.Password != "pw" {
		t.Errorf("Remote.Password = %q", cfg.Remote.Password)
	}
	if cfg.Remote.SSHKey != "/tmp/key" {
		t.Errorf("Remote.SSHKey = %q", cfg.Remote.SSHKey)
	}
	if cfg.ONNXLibPath != "/tmp/ort.so" {
		t.Errorf("ONNXLibPath = %q", cfg.ONNXLibPath)
	}
}

func TestTOMLSections(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "knomit.toml"), []byte(`
[git]
remote = true
port = "9418"

[remote]
token = "my-token"
user = "deployer"
password = "secret"
ssh_key = "/home/deploy/.ssh/id_ed25519"
`), 0o644)
	t.Setenv("KNOMIT_REPO", dir)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if !cfg.Git.Remote {
		t.Error("Git.Remote should be true")
	}
	if cfg.Git.Port != "9418" {
		t.Errorf("Git.Port = %q, want %q", cfg.Git.Port, "9418")
	}
	if cfg.Remote.Token != "my-token" {
		t.Errorf("Remote.Token = %q", cfg.Remote.Token)
	}
	if cfg.Remote.User != "deployer" {
		t.Errorf("Remote.User = %q", cfg.Remote.User)
	}
	if cfg.Remote.Password != "secret" {
		t.Errorf("Remote.Password = %q", cfg.Remote.Password)
	}
	if cfg.Remote.SSHKey != "/home/deploy/.ssh/id_ed25519" {
		t.Errorf("Remote.SSHKey = %q", cfg.Remote.SSHKey)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/config/ -v`
Expected: compilation errors (Load, Defaults, OntologyRoot don't exist yet)

- [ ] **Step 3: Commit**

```bash
git add internal/config/config_test.go
git commit -m "test(config): add tests for TOML config loading"
```

---

### Task 4: Implement config.Load()

**Files:**
- Modify: `internal/config/config.go`

- [ ] **Step 1: Rewrite config.go with new structs and Load()**

Replace the entire contents of `internal/config/config.go` with:

```go
package config

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"knomit/internal/git"
	"knomit/internal/llm"
)

// RemoteConfig holds git remote authentication settings.
type RemoteConfig struct {
	Token    string `toml:"token"`
	User     string `toml:"user"`
	Password string `toml:"password"`
	SSHKey   string `toml:"ssh_key"`
}

// Config is the root configuration, composed of section structs.
type Config struct {
	RepoPath     string       `toml:"repo"`
	CacheDir     string       `toml:"cache_dir"`
	Port         string       `toml:"port"`
	OntologyRoot string       `toml:"ontology_root"`
	ONNXLibPath  string       `toml:"onnx_lib_path"`
	LLM          llm.Config   `toml:"llm"`
	Remote       RemoteConfig `toml:"remote"`
	Git          git.Config   `toml:"git"`
}

// Defaults returns a Config populated with default values.
func Defaults() Config {
	home, _ := os.UserHomeDir()
	return Config{
		RepoPath:     home + "/.knomit",
		CacheDir:     home + "/.cache/knomit",
		Port:         "3000",
		OntologyRoot: "know",
		LLM:          llm.DefaultConfig(),
		Git:          git.DefaultConfig(),
	}
}

// Load builds a Config by layering: defaults → TOML file → env vars.
func Load() (Config, error) {
	cfg := Defaults()

	// Resolve RepoPath from env first (needed for TOML file discovery).
	if v := os.Getenv("KNOMIT_REPO"); v != "" {
		cfg.RepoPath = v
	}

	// Find and decode TOML file.
	if path := findConfigFile(cfg.RepoPath); path != "" {
		if _, err := toml.DecodeFile(path, &cfg); err != nil {
			return Config{}, err
		}
	}

	// Overlay env vars.
	envOr("KNOMIT_REPO", &cfg.RepoPath)
	envOr("KNOMIT_CACHE_DIR", &cfg.CacheDir)
	envOr("KNOMIT_PORT", &cfg.Port)
	envOr("KNOMIT_LLM_MODEL", &cfg.LLM.Model)
	envOr("KNOMIT_LLM_PROVIDER", &cfg.LLM.Provider)
	envOr("KNOMIT_API_KEY", &cfg.LLM.APIKey)
	envBoolOr("KNOMIT_GIT_REMOTE", &cfg.Git.Remote)
	envOr("KNOMIT_GIT_PORT", &cfg.Git.Port)
	envOr("KNOMIT_REMOTE_TOKEN", &cfg.Remote.Token)
	envOr("KNOMIT_REMOTE_USER", &cfg.Remote.User)
	envOr("KNOMIT_REMOTE_PASSWORD", &cfg.Remote.Password)
	envOr("KNOMIT_REMOTE_SSH_KEY", &cfg.Remote.SSHKey)
	envOr("ONNXRUNTIME_SHARED_LIBRARY", &cfg.ONNXLibPath)

	// Expand tildes in path fields.
	expandTilde(&cfg.RepoPath)
	expandTilde(&cfg.CacheDir)
	expandTilde(&cfg.ONNXLibPath)
	expandTilde(&cfg.Remote.SSHKey)

	return cfg, nil
}

// findConfigFile looks for knomit.toml next to the binary, then in repoPath.
func findConfigFile(repoPath string) string {
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "knomit.toml")
		if fileExists(p) {
			return p
		}
	}
	p := filepath.Join(repoPath, "knomit.toml")
	if fileExists(p) {
		return p
	}
	return ""
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func envOr(key string, target *string) {
	if v := os.Getenv(key); v != "" {
		*target = v
	}
}

func envBoolOr(key string, target *bool) {
	if v := os.Getenv(key); v != "" {
		*target = v == "true"
	}
}

func expandTilde(s *string) {
	if strings.HasPrefix(*s, "~/") {
		home, _ := os.UserHomeDir()
		*s = home + (*s)[1:]
	}
}
```

- [ ] **Step 2: Run tests**

Run: `go test ./internal/config/ -v`
Expected: all tests pass

- [ ] **Step 3: Commit**

```bash
git add internal/config/config.go
git commit -m "feat(config): implement layered TOML config loading"
```

---

## Chunk 2: Update callers and propagate ontology root

### Task 5: Update main.go callers (FromEnv → Load)

**Files:**
- Modify: `cmd/knomit/main.go`

- [ ] **Step 1: Replace all `config.FromEnv()` calls with `config.Load()`**

There are 4 call sites: `serveCmd` (line 48), `initCmd` (line 172), `resetCmd` (line 193), `rebuildCmd` (line 217).

Each becomes:
```go
cfg, err := config.Load()
if err != nil {
    return fmt.Errorf("load config: %w", err)
}
```

- [ ] **Step 2: Update field references in serveCmd**

The following field paths change:
- `cfg.LLMModel` → `cfg.LLM.Model`
- `cfg.LLMProvider` → `cfg.LLM.Provider`
- `cfg.APIKey` → `cfg.LLM.APIKey`
- `cfg.GitRemote` → `cfg.Git.Remote`

Specifically in `serveCmd`:
- Line 94: `cfg.LLMModel, cfg.LLMProvider` → `cfg.LLM.Model, cfg.LLM.Provider`
- Line 98: `cfg.LLMModel` → `cfg.LLM.Model`
- Line 114: `cfg.GitRemote` → `cfg.Git.Remote`
- Line 115: `cfg.APIKey` → `cfg.LLM.APIKey`

- [ ] **Step 3: Verify compilation**

Run: `go build ./cmd/knomit/`
Expected: success

- [ ] **Step 4: Commit**

```bash
git add cmd/knomit/main.go
git commit -m "refactor: update main.go to use config.Load()"
```

---

### Task 6: Propagate OntologyRoot to MCP server

**Files:**
- Modify: `internal/mcp/server.go`
- Modify: `internal/mcp/instructions.go`
- Modify: `internal/mcp/learn.go`
- Modify: `internal/mcp/explore.go`
- Modify: `internal/mcp/update.go`
- Modify: `internal/mcp/why.go`
- Modify: `internal/mcp/forget.go`
- Modify: `cmd/knomit/main.go` (NewServer call)

- [ ] **Step 1: Update `normalizePath` to accept ontologyRoot**

In `internal/mcp/learn.go`, change:

```go
func normalizePath(path string) string {
	if !strings.HasPrefix(path, "know/") {
		path = "know/" + path
	}
```

To:

```go
func normalizePath(ontologyRoot, path string) string {
	prefix := ontologyRoot + "/"
	if !strings.HasPrefix(path, prefix) {
		path = prefix + path
	}
```

- [ ] **Step 2: Update all normalizePath callers**

In `learn.go` (line 98):
```go
path := normalizePath(ontologyRoot, fi.Path)
```

The `LearnHandler` signature changes to accept `ontologyRoot`:
```go
func LearnHandler(gs GitStore, idx SearchIndex, ontologyRoot string) ...
```
And captures it in the closure.

Same pattern for:
- `UpdateHandler` in `update.go` — add `ontologyRoot string` param, pass to `normalizePath(ontologyRoot, file)`
- `WhyHandler` in `why.go` — add `ontologyRoot string` param, pass to `normalizePath(ontologyRoot, file)`
- `ForgetHandler` in `forget.go` — add `ontologyRoot string` param, pass to `normalizePath(ontologyRoot, file)`

- [ ] **Step 3: Update `ExploreHandler` default path**

In `explore.go`, change ExploreHandler to accept `ontologyRoot string`:

```go
func ExploreHandler(gs GitStore, ontologyRoot string) ...
```

Line 31: `path := req.GetString("path", "know")` → `path := req.GetString("path", ontologyRoot)`

Also update `exploreTool()` to accept `ontologyRoot` so the description is accurate:

```go
func exploreTool(ontologyRoot string) mcpgo.Tool {
	return mcpgo.NewTool("knomit_explore",
		mcpgo.WithDescription("List the contents of a knowledge base path."),
		mcpgo.WithString("path",
			mcpgo.Description(fmt.Sprintf("Path to explore (default: %q).", ontologyRoot)),
		),
	)
}
```

- [ ] **Step 4: Update `ProfileInstructions` to accept ontologyRoot**

In `instructions.go`, change:

```go
func ProfileInstructions(profile, ontologyRoot string) string {
```

Change `baseInstructions` from a const to a function:

```go
func baseInstructionsText(ontologyRoot string) string {
	return fmt.Sprintf(`You are connected to a knomit knowledge base. Use the available tools to learn, query, and manage knowledge.

Key concepts:
- Facts are stored as markdown files under %s/ with YAML frontmatter (domain, entities, confidence, sources, refs)
- Each fact has a path like %s/topic/subtopic/fact-name.md
- Use knomit_learn to store new knowledge, knomit_query to search, knomit_why for provenance
- Use knomit_update to modify existing facts, knomit_forget to remove outdated knowledge
- Use knomit_explore to browse the knowledge tree`, ontologyRoot, ontologyRoot)
}
```

And update `ProfileInstructions`:
```go
func ProfileInstructions(profile, ontologyRoot string) string {
	addendum, ok := profileAddenda[profile]
	if !ok {
		addendum = profileAddenda["code"]
	}
	return baseInstructionsText(ontologyRoot) + "\n\n" + addendum
}
```

- [ ] **Step 5: Update `NewServer` to accept and propagate ontologyRoot**

In `server.go`:

```go
func NewServer(gs GitStore, idx SearchIndex, llmAdapter llm.LLMAdapter, profile, ontologyRoot string) *server.MCPServer {
	s := server.NewMCPServer("knomit", "1.0.0",
		server.WithInstructions(ProfileInstructions(profile, ontologyRoot)),
	)

	s.AddTool(learnTool(), LearnHandler(gs, idx, ontologyRoot))
	s.AddTool(queryTool(), QueryHandler(gs, idx))
	s.AddTool(whyTool(), WhyHandler(gs, ontologyRoot))
	s.AddTool(updateTool(), UpdateHandler(gs, idx, ontologyRoot))
	s.AddTool(exploreTool(ontologyRoot), ExploreHandler(gs, ontologyRoot))
	s.AddTool(forgetTool(), ForgetHandler(gs, idx, ontologyRoot))

	return s
}
```

- [ ] **Step 6: Update the NewServer call in main.go**

In `cmd/knomit/main.go` line 108:

```go
mcpSrv := mcp.NewServer(gs, idx, llmAdapter, p, cfg.OntologyRoot)
```

- [ ] **Step 7: Verify compilation**

Run: `go build ./cmd/knomit/`
Expected: success

- [ ] **Step 8: Commit**

```bash
git add internal/mcp/server.go internal/mcp/instructions.go internal/mcp/learn.go internal/mcp/explore.go internal/mcp/update.go internal/mcp/why.go internal/mcp/forget.go cmd/knomit/main.go
git commit -m "feat(config): propagate ontology root through MCP handlers"
```

---

### Task 7: Propagate OntologyRoot to web handlers

**Files:**
- Modify: `internal/web/handlers.go`
- Modify: `internal/web/server.go` (if `handleBrowse` is wired there)

- [ ] **Step 1: Check how handleBrowse is wired**

Read `internal/web/server.go` to find how `handleBrowse` is called and what parameters `NewRouter` takes.

- [ ] **Step 2: Update handleBrowse to accept ontologyRoot**

In `handlers.go`, change:

```go
func handleBrowse(gs GitStore, ontologyRoot string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Query().Get("path")
		if path == "" {
			path = ontologyRoot
		}
```

- [ ] **Step 3: Update the wiring in server.go/router**

Pass `ontologyRoot` (from config) through `NewRouter` or wherever `handleBrowse` is called. Add an `ontologyRoot string` parameter to `NewRouter` if needed, and pass `cfg.OntologyRoot` from main.go.

- [ ] **Step 4: Verify compilation**

Run: `go build ./cmd/knomit/`
Expected: success

- [ ] **Step 5: Commit**

```bash
git add internal/web/handlers.go internal/web/server.go cmd/knomit/main.go
git commit -m "feat(config): propagate ontology root to web browse handler"
```

---

### Task 8: Fix tests that reference hardcoded "know"

**Files:**
- Modify: `internal/mcp/explore_test.go`
- Modify: any other test files that call updated functions

- [ ] **Step 1: Find all test files that need updating**

Run: `go build ./...` and `go test ./... -count=1 -short` to find compilation errors from changed signatures.

- [ ] **Step 2: Fix each test file**

For MCP handler tests, the handler constructors now take an extra `ontologyRoot` parameter. Pass `"know"` to keep existing test behavior:

- `LearnHandler(gs, idx)` → `LearnHandler(gs, idx, "know")`
- `ExploreHandler(gs)` → `ExploreHandler(gs, "know")`
- `UpdateHandler(gs, idx)` → `UpdateHandler(gs, idx, "know")`
- `WhyHandler(gs)` → `WhyHandler(gs, "know")`
- `ForgetHandler(gs, idx)` → `ForgetHandler(gs, idx, "know")`
- `NewServer(gs, idx, adapter, profile)` → `NewServer(gs, idx, adapter, profile, "know")`
- `exploreTool()` → `exploreTool("know")`

- [ ] **Step 3: Add a test for ProfileInstructions with custom ontologyRoot**

In the appropriate MCP test file, add:

```go
func TestProfileInstructionsOntologyRoot(t *testing.T) {
	text := ProfileInstructions("code", "facts")
	if !strings.Contains(text, "facts/") {
		t.Errorf("ProfileInstructions should interpolate ontologyRoot, got: %s", text)
	}
	if strings.Contains(text, "know/") {
		t.Errorf("ProfileInstructions should not contain hardcoded 'know/', got: %s", text)
	}
}
```

- [ ] **Step 4: Run full test suite**

Run: `go test ./... -count=1`
Expected: all tests pass

- [ ] **Step 5: Commit**

Stage only the modified test files (do NOT use `git add -A`):

```bash
git add internal/mcp/explore_test.go internal/mcp/learn_test.go internal/mcp/update_test.go internal/mcp/why_test.go internal/mcp/forget_test.go internal/mcp/server_test.go internal/web/handlers_test.go
git commit -m "test: update tests for ontology root parameter"
```

(Only add files that actually changed — check with `git diff --name-only` first.)

---

### Task 9: Final verification

- [ ] **Step 1: Run full build**

Run: `go build ./...`
Expected: success

- [ ] **Step 2: Run full test suite**

Run: `go test ./... -count=1 -v`
Expected: all tests pass

- [ ] **Step 3: Manual smoke test with TOML file**

Create a temporary `knomit.toml` next to the binary and verify it loads:

```bash
mkdir -p /tmp/knomit-smoke
echo 'port = "4444"' > /tmp/knomit-smoke/knomit.toml
KNOMIT_REPO=/tmp/knomit-smoke go run ./cmd/knomit/ serve &
# Check it starts on port 4444, then kill the background process
```
