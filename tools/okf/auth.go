package main

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
)

// redactURL removes secrets from a URL so it can be printed, logged, or
// committed.
//
// For http(s) the WHOLE userinfo goes: an access token may appear as either the
// username (`https://TOKEN@host`) or the password (`https://user:TOKEN@host`),
// and we cannot tell which. For ssh the username is a login name rather than a
// secret — dropping it would publish a source URL that no longer resolves — so
// only a password is removed.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	switch {
	case u.Scheme == "http" || u.Scheme == "https":
		u.User = nil
	default:
		if _, hasPassword := u.User.Password(); !hasPassword {
			return raw
		}
		u.User = url.User(u.User.Username())
	}
	return u.String()
}

// defaultTokenUser is the basic-auth username an access token rides in when
// none is given. The token is always the PASSWORD; the username is ignored by
// GitHub and GitLab, but Bitbucket requires "x-token-auth" — which is the whole
// reason --username exists.
const defaultTokenUser = "git"

// authOpts is the credential material for one run, from flags and the
// environment. Nothing here is ever written to disk.
type authOpts struct {
	token     string
	tokenFile string
	username  string
	sshKey    string
	sshPass   string
}

// registerAuthFlags attaches the credential flags. All three fetching commands
// share them, so they are registered in one place rather than three.
func registerAuthFlags(fs *flag.FlagSet, o *authOpts) {
	fs.StringVar(&o.token, "token", "", "access token for an HTTPS source (or $KNOMIT_OKF_TOKEN)")
	fs.StringVar(&o.tokenFile, "token-file", "", "read the access token from a file")
	fs.StringVar(&o.username, "username", "", "basic-auth user for the token (default \"git\"; Bitbucket needs \"x-token-auth\")")
	fs.StringVar(&o.sshKey, "ssh-key", "", "path to an SSH private key (or $KNOMIT_OKF_SSH_KEY)")
}

// resolve folds the environment into the flags. Flags win; the environment is
// the CI path. The SSH passphrase is environment-ONLY: a passphrase on argv
// would be visible in `ps` to every user on the machine.
func (o *authOpts) resolve() error {
	if o.token != "" && o.tokenFile != "" {
		return errors.New("--token and --token-file are mutually exclusive")
	}
	if o.tokenFile != "" {
		raw, err := os.ReadFile(o.tokenFile)
		if err != nil {
			return fmt.Errorf("read token file: %w", err)
		}
		o.token = strings.TrimSpace(string(raw))
	}
	if o.token == "" {
		o.token = os.Getenv("KNOMIT_OKF_TOKEN")
	}
	if o.sshKey == "" {
		o.sshKey = os.Getenv("KNOMIT_OKF_SSH_KEY")
	}
	o.sshPass = os.Getenv("KNOMIT_OKF_SSH_PASSPHRASE")
	return nil
}

// authFor returns the AuthMethod for rawURL, or nil when the transport takes no
// credentials (a local path) or none were supplied (an anonymous knomit
// instance, or credentials already embedded in the URL — go-git handles those
// itself and must not be overridden).
func authFor(rawURL string, o authOpts) (transport.AuthMethod, error) {
	ep, err := transport.NewEndpoint(rawURL)
	if err != nil {
		// Not our error to report: the transport gives a better message when it
		// tries to use the URL.
		return nil, nil
	}
	switch ep.Protocol {
	case "http", "https":
		if o.token == "" {
			return nil, nil
		}
		user := o.username
		if user == "" {
			user = defaultTokenUser
		}
		return &githttp.BasicAuth{Username: user, Password: o.token}, nil
	case "ssh":
		return sshAuth(ep, o)
	default:
		return nil, nil
	}
}

// defaultIdentities are the key files git itself tries, newest algorithm first.
var defaultIdentities = []string{"id_ed25519", "id_rsa", "id_ecdsa"}

// sshAuth resolves an SSH credential: an explicit key, then the agent, then the
// default identity files.
//
// The agent probe is load-bearing. NewSSHAgentAuth succeeds whenever an agent
// SOCKET exists, even with no identities loaded — a very common state — so
// without asking it for signers we would return an auth method that cannot
// authenticate, and the default-identity fallback below would be dead code.
func sshAuth(ep *transport.Endpoint, o authOpts) (transport.AuthMethod, error) {
	user := ep.User
	if user == "" {
		user = defaultTokenUser
	}
	var tried []string

	if o.sshKey != "" {
		m, err := gitssh.NewPublicKeysFromFile(user, o.sshKey, o.sshPass)
		if err != nil {
			// An explicit key that cannot be loaded is an error, never a reason
			// to silently try something else — the user asked for THIS key.
			return nil, fmt.Errorf("load ssh key %s: %w", o.sshKey, err)
		}
		return m, nil
	}
	tried = append(tried, "--ssh-key (not given)")

	if m, err := gitssh.NewSSHAgentAuth(user); err != nil {
		tried = append(tried, "ssh-agent ("+err.Error()+")")
	} else if signers, serr := m.Callback(); serr != nil {
		tried = append(tried, "ssh-agent ("+serr.Error()+")")
	} else if len(signers) == 0 {
		tried = append(tried, "ssh-agent (running, 0 identities)")
	} else {
		return m, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		tried = append(tried, "home directory ("+err.Error()+")")
	} else {
		for _, name := range defaultIdentities {
			p := filepath.Join(home, ".ssh", name)
			if _, statErr := os.Stat(p); statErr != nil {
				tried = append(tried, "~/.ssh/"+name+" (not found)")
				continue
			}
			m, kerr := gitssh.NewPublicKeysFromFile(user, p, o.sshPass)
			if kerr != nil {
				tried = append(tried, "~/.ssh/"+name+" ("+kerr.Error()+")")
				continue
			}
			return m, nil
		}
	}

	return nil, fmt.Errorf(
		"no usable SSH credentials\n  tried: %s\n  hint: ssh-add <key>, or pass --ssh-key <path>",
		strings.Join(tried, ", "))
}

// explainFetchError adds the hint a user can act on, for the two failures that
// are otherwise opaque. Every other error passes through untouched — a
// misleading auth hint on a network error costs more than no hint at all.
func explainFetchError(err error, rawURL string, o authOpts) error {
	if err == nil {
		return nil
	}
	safe := redactURL(rawURL)

	// go-git verifies known_hosts with no interactive prompt, so the only way
	// out is for the message to carry the command that fixes it.
	if strings.Contains(err.Error(), "knownhosts") {
		host := rawURL
		if ep, epErr := transport.NewEndpoint(rawURL); epErr == nil {
			host = ep.Host
		}
		return fmt.Errorf("%s is not in your known_hosts\n  hint: ssh-keyscan %s >> ~/.ssh/known_hosts\n  (original: %w)",
			host, host, err)
	}

	if errors.Is(err, transport.ErrAuthenticationRequired) ||
		errors.Is(err, transport.ErrAuthorizationFailed) {
		if o.token == "" {
			return fmt.Errorf("%s requires credentials: pass --token <access-token> (or $KNOMIT_OKF_TOKEN): %w", safe, err)
		}
		return fmt.Errorf("%s rejected the token: check it has read access, and that --username is right for this host (Bitbucket needs \"x-token-auth\"): %w", safe, err)
	}
	return err
}
