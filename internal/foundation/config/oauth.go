package config

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
	"time"
)

// OAuthConfig turns this instance into an OAuth 2.1 authorization server and
// resource server for itself (F19 phase 3a). It is OFF unless both Issuer and
// Addr are set: then `knomit serve` opens a THIRD listener on Addr, the only
// place a bearer token is ever judged. The plaintext, local and TLS listeners
// are unchanged by this section.
//
// Issuer is the canonical URL clients reach Addr by — usually a reverse proxy
// with real TLS in front of it. It is the `iss`, the base of every URL in the
// metadata documents, and the root every token's audience must sit under. The
// server never reads X-Forwarded-*: what the issuer says is what it is.
type OAuthConfig struct {
	Issuer string `toml:"issuer"` // env KNOMIT_OAUTH_ISSUER; normalised by Load
	Addr   string `toml:"addr"`   // env KNOMIT_OAUTH_ADDR, e.g. "127.0.0.1:19280"

	// Token lifetimes. Policy, not measured constants: an access token is
	// short enough that revocation-by-expiry is tolerable, a refresh family
	// lasts RefreshTTL from its FIRST issuance (absolute, not sliding).
	AccessTTL  time.Duration `toml:"access_ttl"`
	RefreshTTL time.Duration `toml:"refresh_ttl"`

	// DefaultClient keeps the built-in `kb` client (loopback redirect). A
	// public instance may set it false; a configured [[oauth.client]] with id
	// "kb" replaces the built-in instead.
	DefaultClient bool `toml:"default_client"`

	Clients []OAuthClient `toml:"client"`
}

// OAuthClient is a pre-registered client. Redirect URIs match exactly, except
// loopback IP literals, whose port is ignored (RFC 8252 §7.3).
type OAuthClient struct {
	ID           string   `toml:"id"`
	Name         string   `toml:"name"`
	RedirectURIs []string `toml:"redirect_uris"`
}

// KBClientID is the built-in client `kb login` presents.
const KBClientID = "kb"

// builtinKBClient is `kb login`'s registration: a loopback listener on an
// ephemeral port, so the port is left out (RFC 8252 §7.3 matching).
func builtinKBClient() OAuthClient {
	return OAuthClient{
		ID:           KBClientID,
		Name:         "kb (knomit bridge)",
		RedirectURIs: []string{"http://127.0.0.1/callback", "http://[::1]/callback"},
	}
}

// Enabled reports whether the OAuth listener is configured. Validate has
// already refused one of the two without the other.
func (o OAuthConfig) Enabled() bool { return o.Issuer != "" && o.Addr != "" }

// EffectiveClients is the pre-registered client list the server resolves
// against: the configured clients, plus the built-in `kb` unless
// DefaultClient is false or a configured client already uses its id.
func (o OAuthConfig) EffectiveClients() []OAuthClient {
	out := append([]OAuthClient(nil), o.Clients...)
	if !o.DefaultClient {
		return out
	}
	for _, c := range out {
		if c.ID == KBClientID {
			return out
		}
	}
	return append(out, builtinKBClient())
}

// NormalizeIssuer validates an issuer and returns its one spelling: scheme
// and host lower-cased, no trailing slash, nothing but scheme, host, port and
// path. https is required except on a loopback host, where http is allowed
// for local use (localhost, 127.0.0.1, [::1]) — never a LAN address.
func NormalizeIssuer(s string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil {
		return "", fmt.Errorf("[oauth].issuer %q: %w", s, err)
	}
	if !u.IsAbs() || u.Host == "" || u.Hostname() == "" {
		return "", fmt.Errorf("[oauth].issuer %q: must be an absolute https:// URL", s)
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" {
		return "", fmt.Errorf("[oauth].issuer %q: must not carry userinfo, a query or a fragment", s)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	switch u.Scheme {
	case "https":
	case "http":
		switch u.Hostname() {
		case "localhost", "127.0.0.1", "::1":
		default:
			return "", fmt.Errorf("[oauth].issuer %q: http is allowed only on localhost, 127.0.0.1 or [::1]; use https", s)
		}
	default:
		return "", fmt.Errorf("[oauth].issuer %q: scheme must be https", s)
	}
	p := strings.TrimSuffix(u.EscapedPath(), "/")
	if p != "" && path.Clean(p) != p {
		return "", fmt.Errorf("[oauth].issuer %q: path is not canonical", s)
	}
	return u.Scheme + "://" + u.Host + p, nil
}

// validate is Config.Validate's [oauth] part.
func (o OAuthConfig) validate() error {
	if (o.Issuer == "") != (o.Addr == "") {
		return errors.New("config: [oauth].issuer and [oauth].addr must be set together (both empty turns OAuth off)")
	}
	if o.Issuer == "" {
		return nil
	}
	if norm, err := NormalizeIssuer(o.Issuer); err != nil {
		return fmt.Errorf("config: %w", err)
	} else if norm != o.Issuer {
		return fmt.Errorf("config: [oauth].issuer %q is not normalised (want %q)", o.Issuer, norm)
	}
	if o.AccessTTL <= 0 || o.RefreshTTL <= 0 {
		return fmt.Errorf("config: [oauth].access_ttl and refresh_ttl must be positive, got %v / %v", o.AccessTTL, o.RefreshTTL)
	}
	seen := map[string]bool{}
	for _, c := range o.Clients {
		if c.ID == "" {
			return errors.New("config: [[oauth.client]] needs an id")
		}
		if seen[c.ID] {
			return fmt.Errorf("config: [[oauth.client]] id %q appears twice", c.ID)
		}
		seen[c.ID] = true
		if len(c.RedirectURIs) == 0 {
			return fmt.Errorf("config: [[oauth.client]] %q needs at least one redirect_uri", c.ID)
		}
		for _, r := range c.RedirectURIs {
			u, err := url.Parse(r)
			if err != nil || !u.IsAbs() || u.Host == "" || u.Fragment != "" {
				return fmt.Errorf("config: [[oauth.client]] %q redirect_uri %q must be an absolute URL without a fragment", c.ID, r)
			}
		}
	}
	return nil
}
