package repos

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"

	"knomit/internal/foundation/config"
	"knomit/internal/foundation/pki"
	"knomit/internal/store"
	"knomit/internal/store/remotesync"
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
		// Host keys are verified against a known_hosts file (TOFU on first
		// contact, hard reject on a changed key). Falling back to
		// InsecureIgnoreHostKey when the path is unresolvable would silently
		// restore the MITM hole this replaced, so an unusable known_hosts is a
		// hard error instead.
		knownHosts := cfg.KnownHosts
		if knownHosts == "" {
			// Zero-value config (tests, embedded callers): keep the pins next to
			// the key they authenticate with.
			knownHosts = filepath.Join(filepath.Dir(keyPath), "known_hosts")
		}
		cb, err := hostKeyCallback(knownHosts)
		if err != nil {
			return nil, fmt.Errorf("resolve ssh auth: %w", err)
		}
		publicKeys.HostKeyCallback = cb
		return publicKeys, nil

	case "cert":
		// The credential is the instance certificate, presented by the fleet
		// transport itself (pki.InstallGitTransport), so there is no go-git
		// AuthMethod. pki.GitURL hands go-git the internal form of the URL,
		// and refuses anything that is not https://.
		return nil, nil

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
//
// An origin stored with method cert (a fleet peer, decided once by
// ResolveOrigin when it was set) inherits NOTHING from the fallback: the
// global [remote] credential is a forge credential, and a fleet peer must
// never receive it. It is keyed on the stored method, never on the URL.
func remoteAuthFromRecord(remote *remotesync.Remote, fallback config.RemoteAuthConfig) config.RemoteAuthConfig {
	if remote.AuthMethod == "cert" {
		return config.RemoteAuthConfig{AuthMethod: "cert"}
	}
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

// resolveAuthWithOrigin resolves auth, auto-detecting SSH for git@ or ssh://
// URLs, and returns the URL go-git is handed (pki.GitURL). It never
// classifies: method cert (decided when the origin was set) is the fleet
// transport, every other method the URL as written. A URL carrying the
// fleet transport's internal scheme is refused here, so an origin stored
// with it fails at every open and sync before anything is dialled.
func resolveAuthWithOrigin(cfg config.RemoteAuthConfig, defaultKeyPath, originURL string) (string, transport.AuthMethod, error) {
	gitURL, err := pki.GitURL(originURL, cfg.AuthMethod == "cert")
	if err != nil {
		return "", nil, err
	}
	if cfg.AuthMethod == "" && originURL != "" {
		if strings.HasPrefix(originURL, "git@") || strings.HasPrefix(originURL, "ssh://") {
			cfg.AuthMethod = "ssh"
		}
	}
	auth, err := resolveAuth(cfg, defaultKeyPath)
	if err != nil {
		return "", nil, err
	}
	return gitURL, auth, nil
}

// ErrPeerCredential refuses a credential given for an origin that is an
// instance of this fleet: it authenticates with this instance's certificate,
// and a typed credential is never silently dropped.
var ErrPeerCredential = errors.New("this origin is a knomit instance of your fleet; it authenticates with this instance's certificate: remove the credential")

// ErrCertNotAnInput refuses auth method cert from a caller: knomit sets it
// when an origin classifies as a fleet peer.
var ErrCertNotAnInput = errors.New("auth method cert is chosen by knomit, not given: leave the auth method empty for another knomit instance of your fleet")

// ResolveOrigin decides, ONCE, how o is reached, when an origin enters from
// a caller (create in every mode, join, attach, a connect session, a probe):
// pki.ClassifyOrigin dials an https URL with the fleet client when this
// instance holds fleet trust. A fleet peer gets auth method cert, which the
// store keeps as the "known peer" bit; anything else keeps the caller's
// auth. Every refusal names its cause: a credential for a peer
// (ErrPeerCredential), a host that cannot be reached (ErrOriginUnreachable:
// no guess is stored), a peer whose certificate is refused (the verifier's
// error), the internal scheme (pki.ErrUnsupportedOriginScheme) and cert as
// input (ErrCertNotAnInput). The returned spec is marked decided, so passing
// it on to the next step of the same operation costs no second dial.
func (m *Manager) ResolveOrigin(ctx context.Context, o OriginSpec) (OriginSpec, error) {
	if o.decided {
		return o, nil
	}
	if o.AuthMethod == "cert" {
		return o, ErrCertNotAnInput
	}
	if d := m.deps.Cfg.Git.NetworkTimeout; d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	peer, err := pki.ClassifyOrigin(ctx, m.deps.Cfg.TLS.Dir, m.deps.KeyPath, o.URL)
	switch {
	case errors.Is(err, pki.ErrUnsupportedOriginScheme):
		return o, err
	case err != nil:
		// Unreachable, or a peer whose certificate is refused (revoked,
		// refusing us, unusable trust files): the origin is not attached,
		// and the cause stays reachable by errors.Is.
		return o, fmt.Errorf("%w: %w", ErrOriginUnreachable, err)
	}
	if peer {
		if (o.AuthMethod != "" && o.AuthMethod != "none") || o.AuthToken != "" {
			return o, ErrPeerCredential
		}
		o.AuthMethod = "cert"
	}
	o.decided = true
	return o, nil
}

// ConfigureGitRemote points svc's git remote "origin" at an origin as go-git
// is to read it: pki.GitURL derives the URL from the stored one and its
// method (the internal fleet form for cert). The derived .git/config is the
// only place that form is written; control.db, the API and the UI hold the
// https URL. Every ConfigureRemote of a stored or attached origin goes
// through here, so nothing outside the derivation spells the scheme.
func ConfigureGitRemote(svc *store.Service, url, authMethod, branch, agentBranch string) error {
	gitURL, err := pki.GitURL(url, authMethod == "cert")
	if err != nil {
		return err
	}
	return svc.ConfigureRemote(gitURL, branch, agentBranch)
}
