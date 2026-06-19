package repos

import (
	"fmt"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	gossh "golang.org/x/crypto/ssh"

	"knomit/internal/config"
	"knomit/internal/store"
)

// resolveAuth returns a go-git transport.AuthMethod based on the remote config.
// Returns nil if no credentials are configured (anonymous access).
// defaultKeyPath is used as a fallback SSH key when cfg.SSHKey is empty.
func resolveAuth(cfg config.RemoteAuthConfig, defaultKeyPath string) (transport.AuthMethod, error) {
	method := cfg.AuthMethod

	// Infer method from available fields when not explicitly set.
	if method == "" {
		if cfg.Token != "" {
			method = "token"
		} else if cfg.User == "" && cfg.Password == "" && cfg.SSHKey == "" {
			return nil, nil
		}
	}

	switch method {
	case "token":
		user := cfg.User
		if user == "" {
			user = "x-token"
		}
		return &githttp.BasicAuth{
			Username: user,
			Password: cfg.Token,
		}, nil

	case "basic":
		return &githttp.BasicAuth{
			Username: cfg.User,
			Password: cfg.Password,
		}, nil

	case "ssh":
		keyPath := cfg.SSHKey
		if keyPath == "" {
			keyPath = defaultKeyPath
		}
		if keyPath == "" {
			return nil, fmt.Errorf("ssh auth requires a key path")
		}
		publicKeys, err := gitssh.NewPublicKeysFromFile("git", keyPath, "")
		if err != nil {
			return nil, fmt.Errorf("resolve ssh auth: %w", err)
		}
		publicKeys.HostKeyCallback = gossh.InsecureIgnoreHostKey()
		return publicKeys, nil

	case "", "none":
		// "" = inferred-anonymous; "none" = explicitly anonymous (the caller
		// chose no auth, so resolveAuthWithOrigin must not auto-promote to SSH).
		return nil, nil

	default:
		return nil, fmt.Errorf("unknown auth method: %q", method)
	}
}

// remoteAuthFromRecord builds a RemoteAuthConfig from a stored remote record,
// falling back to the global config for fields not set in the record.
func remoteAuthFromRecord(remote *store.Remote, fallback config.RemoteAuthConfig) config.RemoteAuthConfig {
	cfg := fallback
	if remote.AuthMethod != "" {
		cfg.AuthMethod = remote.AuthMethod
	}
	if remote.AuthToken != "" {
		if cfg.AuthMethod == "basic" {
			// token field stores user:password
			if parts := strings.SplitN(remote.AuthToken, ":", 2); len(parts) == 2 {
				cfg.User = parts[0]
				cfg.Password = parts[1]
			}
		} else {
			cfg.Token = remote.AuthToken
		}
	}
	return cfg
}

// resolveAuthWithOrigin resolves auth, auto-detecting SSH for git@ or ssh:// URLs.
func resolveAuthWithOrigin(cfg config.RemoteAuthConfig, defaultKeyPath, originURL string) (transport.AuthMethod, error) {
	if cfg.AuthMethod == "" && originURL != "" {
		if strings.HasPrefix(originURL, "git@") || strings.HasPrefix(originURL, "ssh://") {
			cfg.AuthMethod = "ssh"
		}
	}
	return resolveAuth(cfg, defaultKeyPath)
}
