package repos

import (
	"errors"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing/transport"

	"knomit/internal/corpus/repos/control"
	"knomit/internal/foundation/config"
)

// One resolver for a stored secret, used by every git operation: the
// reconcile loop and fleet publishes (makeRemoteAuthFn, reading the injected
// origin) and every spec-carried operation (resolveSpecAuth: clone,
// initialize, subscribe, probe, origin-session test, fleet move). A linked
// origin is resolved from the secret ALONE: no [remote] fallback (except
// known_hosts), no pasted token, never anonymous. Any failure (missing,
// undecryptable, wrong type, host not on the list) is an error naming the
// secret by name.

// A host off the secret's list is control.ErrSecretHostNotAllowed.
var (
	// ErrSecretTypeMismatch: the secret's type does not fit the remote URL.
	ErrSecretTypeMismatch = errors.New("the stored secret's type does not fit the remote URL: a token or basic secret needs an http(s) URL, an ssh_key secret an ssh URL")
	// ErrSecretOnPeer: a stored secret on a fleet peer origin.
	ErrSecretOnPeer = errors.New("a stored secret cannot be used for a knomit instance of your fleet: it authenticates with this instance's certificate: unlink the secret")
)

// methodForSecretType is the origin auth method a secret type implies, or ""
// for an unknown type.
func methodForSecretType(t string) string {
	switch t {
	case control.SecretTypeToken:
		return "token"
	case control.SecretTypeBasic:
		return "basic"
	case control.SecretTypeSSHKey:
		return "ssh"
	}
	return ""
}

// MethodForSecretType is methodForSecretType for the REST edge.
func MethodForSecretType(t string) string { return methodForSecretType(t) }

// checkSecretFits refuses a secret for url when the url's host is not on its
// hosts list (control.ErrSecretHostNotAllowed) or its type does not fit the url's
// scheme (ErrSecretTypeMismatch). Errors name the secret, never its value.
func checkSecretFits(meta control.SecretMeta, url string) error {
	host, err := control.RemoteHost(url)
	if err != nil {
		return fmt.Errorf("stored secret %q: %w (%v)", meta.Name, control.ErrSecretHostNotAllowed, err)
	}
	if !control.HostAllowed(meta.Hosts, host) {
		return fmt.Errorf("stored secret %q is not allowed for host %s: %w", meta.Name, host, control.ErrSecretHostNotAllowed)
	}
	method := methodForSecretType(meta.Type)
	fits := false
	switch method {
	case "token", "basic":
		fits = isHTTPURL(url)
	case "ssh":
		fits = isSSHURL(url)
	}
	if !fits || MethodMismatch(url, method) != nil {
		return fmt.Errorf("stored secret %q (type %s): %w", meta.Name, meta.Type, ErrSecretTypeMismatch)
	}
	return nil
}

// resolveLinkedAuth reveals secret secretID, re-checks that it fits url
// (host and type), and builds the auth config from the secret alone.
// knownHosts is the only setting taken from outside the secret.
func resolveLinkedAuth(secrets *control.Secrets, secretID, url, knownHosts string) (config.RemoteAuthConfig, error) {
	if secrets == nil {
		return config.RemoteAuthConfig{}, fmt.Errorf("stored secret %s: %w", secretID, errSecretsUnavailable)
	}
	meta, v, err := secrets.Reveal(secretID)
	if err != nil {
		if errors.Is(err, control.ErrSecretNotFound) {
			return config.RemoteAuthConfig{}, fmt.Errorf("stored secret %s: %w: link another credential (PUT /repos/{repo}/origin/credential)", secretID, err)
		}
		return config.RemoteAuthConfig{}, err
	}
	if err := checkSecretFits(meta, url); err != nil {
		return config.RemoteAuthConfig{}, err
	}
	cfg := config.RemoteAuthConfig{AuthMethod: methodForSecretType(meta.Type), KnownHosts: knownHosts}
	switch meta.Type {
	case control.SecretTypeToken:
		cfg.Token = v.Token
	case control.SecretTypeBasic:
		cfg.User, cfg.Password = v.Username, v.Password
	case control.SecretTypeSSHKey:
		cfg.SSHKeyPEM, cfg.SSHKeyPassphrase = v.PrivateKey, v.Passphrase
	}
	return cfg, nil
}

// resolveSpecAuth resolves the auth for a spec-carried git operation and the
// URL go-git is handed. A spec with a SecretID is resolved from the secret
// (resolveLinkedAuth); otherwise from its own pasted credential. It is the
// only caller of authConfigFromSpec (TestAuthConfigFromSpecHasOneCaller).
func (m *Manager) resolveSpecAuth(o *OriginSpec) (string, transport.AuthMethod, error) {
	if o.SecretID == "" {
		return m.resolveAuthConfig(authConfigFromSpec(o), o.URL)
	}
	if o.AuthMethod == "cert" {
		return "", nil, ErrSecretOnPeer
	}
	cfg, err := resolveLinkedAuth(m.Secrets(), o.SecretID, o.URL, m.deps.Cfg.Remote.KnownHosts)
	if err != nil {
		return "", nil, err
	}
	return m.resolveAuthConfig(cfg, o.URL)
}

// ResolveSpecAuth is resolveSpecAuth for the REST edge (origin sessions).
func (m *Manager) ResolveSpecAuth(o OriginSpec) (string, transport.AuthMethod, error) {
	return m.resolveSpecAuth(&o)
}

// CheckSecretForURL reports whether secret secretID may be linked to an
// origin at url: the secret exists (control.ErrSecretNotFound) and fits the
// url's host and scheme (control.ErrSecretHostNotAllowed, ErrSecretTypeMismatch). It
// returns the auth method the link stores.
func (m *Manager) CheckSecretForURL(secretID, url string) (string, error) {
	s := m.Secrets()
	if s == nil {
		return "", errSecretsUnavailable
	}
	meta, err := s.Get(secretID)
	if err != nil {
		return "", err
	}
	if err := checkSecretFits(meta, url); err != nil {
		return "", err
	}
	return methodForSecretType(meta.Type), nil
}
