package config

import "os"

type Config struct {
	RepoPath        string
	Port            string
	CacheDir        string
	APIKey          string
	GitRemote       bool
	GitPort         string
	LLMProvider     string
	LLMModel        string
	RemoteToken     string
	RemoteUser      string
	RemotePassword  string
	RemoteSSHKey    string
	ONNXLibPath     string
}

func FromEnv() Config {
	repoPath := os.Getenv("KNOMIT_REPO")
	if repoPath == "" {
		home, _ := os.UserHomeDir()
		repoPath = home + "/.knomit"
	}
	cacheDir := os.Getenv("KNOMIT_CACHE_DIR")
	if cacheDir == "" {
		home, _ := os.UserHomeDir()
		cacheDir = home + "/.cache/knomit"
	}
	port := os.Getenv("KNOMIT_PORT")
	if port == "" {
		port = "3000"
	}
	model := os.Getenv("KNOMIT_LLM_MODEL")
	if model == "" {
		model = "claude-sonnet-4-6"
	}
	gitRemote := os.Getenv("KNOMIT_GIT_REMOTE") == "true"
	return Config{
		RepoPath:       repoPath,
		Port:           port,
		CacheDir:       cacheDir,
		APIKey:         os.Getenv("KNOMIT_API_KEY"),
		GitRemote:      gitRemote,
		GitPort:        os.Getenv("KNOMIT_GIT_PORT"),
		LLMProvider:    os.Getenv("KNOMIT_LLM_PROVIDER"),
		LLMModel:       model,
		RemoteToken:    os.Getenv("KNOMIT_REMOTE_TOKEN"),
		RemoteUser:     os.Getenv("KNOMIT_REMOTE_USER"),
		RemotePassword: os.Getenv("KNOMIT_REMOTE_PASSWORD"),
		RemoteSSHKey:   os.Getenv("KNOMIT_REMOTE_SSH_KEY"),
		ONNXLibPath:    os.Getenv("ONNXRUNTIME_SHARED_LIBRARY"),
	}
}
