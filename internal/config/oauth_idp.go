package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"unicode"

	"knomit/internal/auth"
)

// OAuthIDPConfig is consent path 3 (F19 phase 3c): a human approves a
// waiting OAuth request by proving an identity at ONE external provider
// (GitHub) whose stable numeric id is on AllowedSubjects. It is off unless
// [oauth.idp] is present, and needs [oauth] on. knomit still issues its own
// tokens; the provider only answers "who is at this browser".
//
// The client secret is NEVER plaintext in knomit.toml: it comes from
// ClientSecretFile (owner-only, 0600 or stricter) or from the env var
// KNOMIT_OAUTH_IDP_CLIENT_SECRET, which wins when both are set. A
// client_secret key in the TOML is refused at load.
type OAuthIDPConfig struct {
	Provider         string `toml:"provider"` // "github", the only one
	ClientID         string `toml:"client_id"`
	ClientSecretFile string `toml:"client_secret_file"`
	// ClientSecret exists only so a plaintext secret in the TOML is refused
	// by name instead of being reported as an unknown key and ignored.
	ClientSecret string `toml:"client_secret"`
	// AllowedSubjects lists "github-<numeric id>", optionally followed by
	// the permission names that cap that person's tokens:
	// "github-583231 read". Ids only: a login can be renamed and then
	// claimed by someone else (`knomit oauth idp resolve github <login>`
	// prints the id to paste). Empty is refused: it would admit nobody.
	AllowedSubjects []string `toml:"allowed_subjects"`

	// Secret is filled by Load from the env or the file; never from TOML.
	Secret IDPSecret `toml:"-"`
}

// IDPSecretEnv is the env var the provider client secret may come from.
const IDPSecretEnv = "KNOMIT_OAUTH_IDP_CLIENT_SECRET"

// IDPSecret holds the provider client secret. Every way of printing or
// encoding it yields "[redacted]"; only Reveal returns the value, and only
// the token exchange calls it.
type IDPSecret struct{ v string }

func NewIDPSecret(s string) IDPSecret { return IDPSecret{v: s} }

// Reveal returns the secret. Call it only to send it to the provider.
func (s IDPSecret) Reveal() string { return s.v }

func (s IDPSecret) IsZero() bool { return s.v == "" }

const redacted = "[redacted]"

func (IDPSecret) String() string               { return redacted }
func (IDPSecret) GoString() string             { return redacted }
func (IDPSecret) Format(f fmt.State, _ rune)   { _, _ = f.Write([]byte(redacted)) }
func (IDPSecret) MarshalJSON() ([]byte, error) { return json.Marshal(redacted) }
func (IDPSecret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// AllowedSubject is one parsed allow-list entry. Scopes nil means no cap
// beyond the default ceiling.
type AllowedSubject struct {
	Subject string
	Scopes  []string
}

// idpSubjectRE is the one spelling of a provider subject: provider name,
// '-', the provider's numeric id without leading zeros. No ':' — the token
// subject must pass the issuer's subjectRE and keep Principal.String()'s
// <kind>:<id>@<via> unambiguous (3c ruling R3).
var idpSubjectRE = regexp.MustCompile(`^github-[1-9][0-9]{0,19}$`)

// Allowed parses AllowedSubjects. Validate has already refused anything it
// cannot parse, so errors are impossible on a loaded config.
func (c OAuthIDPConfig) Allowed() []AllowedSubject {
	out := make([]AllowedSubject, 0, len(c.AllowedSubjects))
	for _, e := range c.AllowedSubjects {
		a, _ := parseAllowedSubject(e)
		out = append(out, a)
	}
	return out
}

func parseAllowedSubject(e string) (AllowedSubject, error) {
	f := strings.Fields(e)
	if len(f) == 0 {
		return AllowedSubject{}, errors.New("empty entry")
	}
	if strings.Contains(f[0], "@") {
		return AllowedSubject{}, fmt.Errorf("%q: logins are not accepted, only the numeric id (a renamed login can be claimed by someone else); get it with `knomit oauth idp resolve github <login>`", f[0])
	}
	if !idpSubjectRE.MatchString(f[0]) {
		return AllowedSubject{}, fmt.Errorf("%q: want github-<numeric id>, optionally followed by permission names", f[0])
	}
	a := AllowedSubject{Subject: f[0]}
	if len(f) > 1 {
		if _, err := auth.ParseSet(f[1:]); err != nil {
			return AllowedSubject{}, fmt.Errorf("%q: %w", e, err)
		}
		if slices.Contains(f[1:], string(auth.Admin)) {
			return AllowedSubject{}, fmt.Errorf("%q: admin is never granted to a token", e)
		}
		a.Scopes = f[1:]
	}
	return a, nil
}

// loadIDPSecret fills c.Secret from the env var or the file. It runs in
// Load, after the TOML and before Validate.
func (c *OAuthIDPConfig) loadIDPSecret() error {
	if c.ClientSecret != "" {
		return fmt.Errorf("config: [oauth.idp].client_secret is refused: the secret must not be plaintext in knomit.toml; put it in a file readable only by you (mode 0600) named by client_secret_file, or in the env var %s", IDPSecretEnv)
	}
	if v := os.Getenv(IDPSecretEnv); v != "" {
		c.Secret = NewIDPSecret(v)
		return nil
	}
	if c.ClientSecretFile == "" {
		return fmt.Errorf("config: [oauth.idp] needs the provider client secret: set client_secret_file (a file of mode 0600) or the env var %s", IDPSecretEnv)
	}
	st, err := os.Stat(c.ClientSecretFile)
	if err != nil {
		return fmt.Errorf("config: [oauth.idp].client_secret_file: %w", err)
	}
	// POSIX modes only; Windows reports synthetic bits, so no check there.
	// On Windows the guard for a secret file placed under <home> is the data
	// root's DACL (internal/platform/privdir, applied at boot), which the file
	// inherits; a file kept elsewhere is the user's responsibility.
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("config: [oauth.idp].client_secret_file %q has mode %o; it must be readable by its owner only (0600 or stricter)", c.ClientSecretFile, st.Mode().Perm())
	}
	b, err := os.ReadFile(c.ClientSecretFile)
	if err != nil {
		return fmt.Errorf("config: [oauth.idp].client_secret_file: %w", err)
	}
	c.Secret = NewIDPSecret(strings.TrimSpace(string(b)))
	return nil
}

// validate is Config.Validate's [oauth.idp] part.
func (c *OAuthIDPConfig) validate(oauthOn bool) error {
	if !oauthOn {
		return errors.New("config: [oauth.idp] needs [oauth] issuer and addr: it approves requests on the OAuth listener")
	}
	if c.Provider != "github" {
		return fmt.Errorf("config: [oauth.idp].provider %q: the only provider is \"github\"", c.Provider)
	}
	if c.ClientID == "" || strings.IndexFunc(c.ClientID, func(r rune) bool { return r > unicode.MaxASCII || !unicode.IsPrint(r) || r == ' ' }) >= 0 {
		return errors.New("config: [oauth.idp].client_id must be the provider's client id (printable ASCII, no spaces)")
	}
	if c.Secret.IsZero() {
		return fmt.Errorf("config: [oauth.idp] has no client secret: set client_secret_file or %s", IDPSecretEnv)
	}
	if len(c.AllowedSubjects) == 0 {
		return errors.New("config: [oauth.idp].allowed_subjects is empty, which would admit nobody; list github-<numeric id> entries")
	}
	seen := map[string]bool{}
	for _, e := range c.AllowedSubjects {
		a, err := parseAllowedSubject(e)
		if err != nil {
			return fmt.Errorf("config: [oauth.idp].allowed_subjects: %w", err)
		}
		if seen[a.Subject] {
			return fmt.Errorf("config: [oauth.idp].allowed_subjects lists %s twice", a.Subject)
		}
		seen[a.Subject] = true
	}
	return nil
}
