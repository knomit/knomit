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
	if cfg.OntologyRoot != "kb" {
		t.Errorf("OntologyRoot = %q, want %q", cfg.OntologyRoot, "kb")
	}
	if cfg.LLM.Model != "gemini-2.5-flash" {
		t.Errorf("LLM.Model = %q, want %q", cfg.LLM.Model, "gemini-2.5-flash")
	}
	home, _ := os.UserHomeDir()
	if cfg.Home != home+"/.knomit" {
		t.Errorf("Home = %q, want %q", cfg.Home, home+"/.knomit")
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
serve = true
`), 0o644)
	t.Setenv("KNOMIT_REPO", dir)
	t.Setenv("KNOMIT_GIT_SERVE", "false")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Git.Serve {
		t.Error("Git.Serve should be false (env override of TOML true)")
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
	if cfg.OntologyRoot != "kb" {
		t.Errorf("OntologyRoot = %q, want default %q", cfg.OntologyRoot, "kb")
	}
	if cfg.LLM.Model != "gemini-2.5-flash" {
		t.Errorf("LLM.Model = %q, want default %q", cfg.LLM.Model, "gemini-2.5-flash")
	}
}

func TestAllEnvVars(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KNOMIT_REPO", dir)
	t.Setenv("KNOMIT_PORT", "9090")
	t.Setenv("KNOMIT_LLM_MODEL", "test-model")
	t.Setenv("KNOMIT_LLM_PROVIDER", "test-provider")
	t.Setenv("KNOMIT_API_KEY", "sk-test")
	t.Setenv("KNOMIT_GIT_ORIGIN", "https://github.com/example/repo.git")
	t.Setenv("KNOMIT_GIT_SERVE", "true")
	t.Setenv("KNOMIT_GIT_PORT", "9418")
	t.Setenv("KNOMIT_REMOTE_TOKEN", "tok")
	t.Setenv("KNOMIT_REMOTE_USER", "usr")
	t.Setenv("KNOMIT_REMOTE_PASSWORD", "pw")
	t.Setenv("KNOMIT_REMOTE_SSH_KEY", "/tmp/key")
	t.Setenv("KNOMIT_REMOTE_AUTH", "token")
	t.Setenv("ONNXRUNTIME_SHARED_LIBRARY", "/tmp/ort.so")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Home != dir {
		t.Errorf("Home = %q, want %q", cfg.Home, dir)
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
	if cfg.Git.Origin != "https://github.com/example/repo.git" {
		t.Errorf("Git.Origin = %q", cfg.Git.Origin)
	}
	if !cfg.Git.Serve {
		t.Error("Git.Serve should be true")
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
	if cfg.Remote.AuthMethod != "token" {
		t.Errorf("Remote.AuthMethod = %q", cfg.Remote.AuthMethod)
	}
	if cfg.ONNXLibPath != "/tmp/ort.so" {
		t.Errorf("ONNXLibPath = %q", cfg.ONNXLibPath)
	}
}

func TestTOMLSections(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "knomit.toml"), []byte(`
[git]
origin = "https://github.com/example/repo.git"
serve = true
port = "9418"

[remote]
token = "my-token"
user = "deployer"
password = "secret"
ssh_key = "/home/deploy/.ssh/id_ed25519"
auth_method = "ssh"
`), 0o644)
	t.Setenv("KNOMIT_REPO", dir)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Git.Origin != "https://github.com/example/repo.git" {
		t.Errorf("Git.Origin = %q", cfg.Git.Origin)
	}
	if !cfg.Git.Serve {
		t.Error("Git.Serve should be true")
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
	if cfg.Remote.AuthMethod != "ssh" {
		t.Errorf("Remote.AuthMethod = %q", cfg.Remote.AuthMethod)
	}
}

func TestNewEnvVars(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KNOMIT_REPO", dir)
	t.Setenv("KNOMIT_GIT_ORIGIN", "git@github.com:org/repo.git")
	t.Setenv("KNOMIT_REMOTE_AUTH", "basic")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Git.Origin != "git@github.com:org/repo.git" {
		t.Errorf("Git.Origin = %q, want %q", cfg.Git.Origin, "git@github.com:org/repo.git")
	}
	if cfg.Remote.AuthMethod != "basic" {
		t.Errorf("Remote.AuthMethod = %q, want %q", cfg.Remote.AuthMethod, "basic")
	}
}

func TestLoad_KnomitHomeEnvVar(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KNOMIT_HOME", dir)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Home != dir {
		t.Errorf("Home = %q, want %q", cfg.Home, dir)
	}
}

func TestLoad_KnomitRepoBackwardCompat(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KNOMIT_REPO", dir)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Home != dir {
		t.Errorf("Home = %q, want %q (KNOMIT_REPO should set Home)", cfg.Home, dir)
	}
}

func TestLoad_KnomitHomePrecedence(t *testing.T) {
	homeDir := t.TempDir()
	repoDir := t.TempDir()
	t.Setenv("KNOMIT_HOME", homeDir)
	t.Setenv("KNOMIT_REPO", repoDir)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Home != homeDir {
		t.Errorf("Home = %q, want %q (KNOMIT_HOME should take precedence over KNOMIT_REPO)", cfg.Home, homeDir)
	}
}
