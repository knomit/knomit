package main

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
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

// sshAuth is implemented in the next commit; until then an SSH source fails
// loudly rather than silently attempting an anonymous fetch.
func sshAuth(ep *transport.Endpoint, o authOpts) (transport.AuthMethod, error) {
	return nil, errors.New("ssh sources are not supported yet")
}
