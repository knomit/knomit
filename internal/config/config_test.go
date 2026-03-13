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
	if cfg.LLM.Model != "gemini-2.5-flash" {
		t.Errorf("LLM.Model = %q, want %q", cfg.LLM.Model, "gemini-2.5-flash")
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
	if cfg.OntologyRoot != "know" {
		t.Errorf("OntologyRoot = %q, want default %q", cfg.OntologyRoot, "know")
	}
	if cfg.LLM.Model != "gemini-2.5-flash" {
		t.Errorf("LLM.Model = %q, want default %q", cfg.LLM.Model, "gemini-2.5-flash")
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
