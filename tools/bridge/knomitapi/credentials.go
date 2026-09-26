package knomitapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"knomit/internal/config"
	"knomit/internal/platform/privdir"
)

// Credentials is what `kb login` keeps for one knomit host: the OAuth token
// pair and where to refresh and revoke it. It lives at
// <knomit home>/credentials/<host>_<port>, mode 0600, and nowhere else —
// never in .mcp.json, which names a command and is committed. (An OS
// keychain is later.)
type Credentials struct {
	Issuer             string    `json:"issuer"`
	TokenEndpoint      string    `json:"token_endpoint"`
	RevocationEndpoint string    `json:"revocation_endpoint,omitempty"`
	ClientID           string    `json:"client_id"`
	Resource           string    `json:"resource"`
	Scope              string    `json:"scope"`
	AccessToken        string    `json:"access_token"`
	RefreshToken       string    `json:"refresh_token"`
	Expiry             time.Time `json:"expiry"`
}

// CredentialsPath is where the credentials for u's host live. The name is
// <host>_<port> with the port made explicit (443/80 from the scheme) and
// every character outside [a-z0-9.-] replaced by "-", so an IPv6 literal or
// a port separator never puts a ':' in a Windows file name. The home is the
// one knomit resolves (KNOMIT_HOME honoured), never a literal ~/.knomit.
func CredentialsPath(u *url.URL) (string, error) {
	home, err := config.ResolveHome()
	if err != nil {
		return "", err
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", fmt.Errorf("no host in %q", u.String())
	}
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		default:
			return "", fmt.Errorf("unsupported scheme %q", u.Scheme)
		}
	}
	name := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '-' {
			return r
		}
		return '-'
	}, host)
	return filepath.Join(home, "credentials", name+"_"+port), nil
}

// LoadCredentials reads the credentials for u's host; os.ErrNotExist when
// there are none.
func LoadCredentials(u *url.URL) (*Credentials, error) {
	p, err := CredentialsPath(u)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var c Credentials
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("credentials %s: %w", p, err)
	}
	if c.AccessToken == "" {
		return nil, fmt.Errorf("credentials %s: no access token", p)
	}
	return &c, nil
}

// SaveCredentials writes them at 0600 in a 0700 directory under the private
// data root (see ensureCredentialsDir), via a temp file and a rename so a
// concurrent reader never sees half a file.
func SaveCredentials(u *url.URL, c *Credentials) error {
	p, err := CredentialsPath(u)
	if err != nil {
		return err
	}
	if err := ensureCredentialsDir(p); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".cred-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// DeleteCredentials removes them; absent is not an error.
func DeleteCredentials(u *url.URL) error {
	p, err := CredentialsPath(u)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// withCredentialsLock serialises refreshes of one host's credentials ACROSS
// PROCESSES. Several kb bridges (one per agent session) share the file, and
// the server revokes a whole family when a rotated refresh token is presented
// again — so two processes refreshing at once would log everyone out.
func withCredentialsLock(ctx context.Context, u *url.URL, fn func() error) error {
	p, err := CredentialsPath(u)
	if err != nil {
		return err
	}
	if err := ensureCredentialsDir(p); err != nil {
		return err
	}
	f, err := os.OpenFile(p+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := lockFile(ctx, f); err != nil {
		return err
	}
	defer unlockFile(f)
	return fn()
}

// ensureCredentialsDir creates the directory that holds p, making the data
// root above it private first (privdir.Ensure). `kb login` can be the first
// thing ever run on a machine, before any server boot has done that, and on
// Windows the token files are private only by inheriting the root's DACL:
// their 0600 is a no-op there.
func ensureCredentialsDir(p string) error {
	dir := filepath.Dir(p)
	if err := privdir.Ensure(filepath.Dir(dir)); err != nil {
		return err
	}
	return os.MkdirAll(dir, 0o700)
}
