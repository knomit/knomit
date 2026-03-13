# TOML Configuration Support

## Problem

Configuration is scattered across env vars and hardcoded defaults. There's no file-based config, and the ontology root ("know") is hardcoded with no way to change it.

## Design

### Precedence

defaults (code) → TOML file → env vars

### TOML File Discovery

1. Next to the binary (`knomit.toml`)
2. In the repo directory (`$KNOMIT_REPO/knomit.toml` or `~/.knomit/knomit.toml`)

`RepoPath` is resolved from `KNOMIT_REPO` env var (or default `~/.knomit`) before searching for the TOML file, since the repo dir is itself a search location.

### Config Structs

Each section struct lives in its consuming package. The root `Config` composes them.

```go
// internal/config/config.go
type Config struct {
    RepoPath     string             `toml:"repo"`
    CacheDir     string             `toml:"cache_dir"`
    Port         string             `toml:"port"`
    OntologyRoot string             `toml:"ontology_root"`
    ONNXLibPath  string             `toml:"onnx_lib_path"`
    LLM          llm.Config         `toml:"llm"`
    Remote       RemoteConfig       `toml:"remote"`
    Git          git.Config         `toml:"git"`
}

// internal/llm/config.go
type Config struct {
    Model    string `toml:"model"`
    Provider string `toml:"provider"`
    APIKey   string `toml:"api_key"`
}

// internal/git/config.go
type Config struct {
    Remote bool   `toml:"remote"`
    Port   string `toml:"port"`
}

// internal/config/config.go
type RemoteConfig struct {
    Token    string `toml:"token"`
    User     string `toml:"user"`
    Password string `toml:"password"`
    SSHKey   string `toml:"ssh_key"`
}
```

### Defaults

Each section struct has a `Defaults()` constructor:

```go
// internal/llm/config.go
func DefaultConfig() Config {
    return Config{
        Model: "claude-sonnet-4-6",
    }
}
```

Root `Config` has a top-level `Defaults()` that composes them:

```go
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
```

### Loading

`FromEnv()` becomes `Load() (Config, error)`.

```
Load():
  1. cfg = Defaults()
  2. Resolve RepoPath from KNOMIT_REPO env (needed for step 3)
  3. Find knomit.toml (binary dir → repo dir)
  4. If found, toml.DecodeFile() onto cfg
  5. Overlay env vars (only when set)
  6. Return cfg
```

### Env Var Overlay

Helper functions that only overwrite when the env var is set:

```go
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
```

Env var mapping (unchanged from today):

| Env Var | Config Field |
|---------|-------------|
| `KNOMIT_REPO` | `RepoPath` |
| `KNOMIT_CACHE_DIR` | `CacheDir` |
| `KNOMIT_PORT` | `Port` |
| `KNOMIT_LLM_MODEL` | `LLM.Model` |
| `KNOMIT_LLM_PROVIDER` | `LLM.Provider` |
| `KNOMIT_API_KEY` | `LLM.APIKey` |
| `KNOMIT_GIT_REMOTE` | `Git.Remote` |
| `KNOMIT_GIT_PORT` | `Git.Port` |
| `KNOMIT_REMOTE_TOKEN` | `Remote.Token` |
| `KNOMIT_REMOTE_USER` | `Remote.User` |
| `KNOMIT_REMOTE_PASSWORD` | `Remote.Password` |
| `KNOMIT_REMOTE_SSH_KEY` | `Remote.SSHKey` |
| `ONNXRUNTIME_SHARED_LIBRARY` | `ONNXLibPath` |

### OntologyRoot Propagation

The hardcoded `"know"` in these locations reads from `Config.OntologyRoot` instead:

- `internal/mcp/learn.go` — `normalizePath()` prefix
- `internal/mcp/explore.go` — default path parameter
- `internal/web/handlers.go` — default browse path
- `internal/mcp/instructions.go` — base instructions text

### TOML File Example

```toml
repo = "~/.knomit"
port = "3000"
ontology_root = "know"

[llm]
model = "claude-sonnet-4-6"
api_key = "sk-ant-..."

[git]
remote = false

[remote]
user = "git"
ssh_key = "~/.ssh/id_ed25519"
```

### Dependency

Add `github.com/BurntSushi/toml` to `go.mod`.

### API Change

`config.FromEnv()` → `config.Load()`, returns `(Config, error)`. All callers updated.

## Non-Goals

- Migration of existing repos when ontology root changes
- Config file generation / `knomit config` CLI command
- Validation beyond TOML parse errors
