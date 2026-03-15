package git

import (
	"fmt"

	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
)

// ResolveAuth returns a go-git transport.AuthMethod based on the remote config.
// Returns nil if no credentials are configured (anonymous access).
func ResolveAuth(cfg RemoteAuthConfig) (transport.AuthMethod, error) {
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
		publicKeys, err := gitssh.NewPublicKeysFromFile("git", cfg.SSHKey, "")
		if err != nil {
			return nil, fmt.Errorf("resolve ssh auth: %w", err)
		}
		return publicKeys, nil

	case "":
		return nil, nil

	default:
		return nil, fmt.Errorf("unknown auth method: %q", method)
	}
}
