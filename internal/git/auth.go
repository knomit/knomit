package git

import (
	"fmt"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	gossh "golang.org/x/crypto/ssh"
)

// ResolveAuth returns a go-git transport.AuthMethod based on the remote config.
// Returns nil if no credentials are configured (anonymous access).
// defaultKeyPath is used as a fallback SSH key when cfg.SSHKey is empty.
func ResolveAuth(cfg RemoteAuthConfig, defaultKeyPath string) (transport.AuthMethod, error) {
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

	case "":
		return nil, nil

	default:
		return nil, fmt.Errorf("unknown auth method: %q", method)
	}
}

// ResolveAuthWithOrigin resolves auth, auto-detecting SSH for git@ or ssh:// URLs.
func ResolveAuthWithOrigin(cfg RemoteAuthConfig, defaultKeyPath, originURL string) (transport.AuthMethod, error) {
	if cfg.AuthMethod == "" && originURL != "" {
		if strings.HasPrefix(originURL, "git@") || strings.HasPrefix(originURL, "ssh://") {
			cfg.AuthMethod = "ssh"
		}
	}
	return ResolveAuth(cfg, defaultKeyPath)
}
