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
	"golang.org/x/crypto/ssh/knownhosts"
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

// safeURL renders a URL for a message — an error, a log line, a committed
// config — and, unlike redactURL, it is safe for EVERY input.
//
// redactURL returns its argument verbatim when url.Parse fails, which is
// exactly the case where a credential would otherwise survive:
// `https://u:TOKEN@host:port/kb.git` (a non-numeric port) does not parse, so
// redactURL alone hands the token straight through. Here an unparseable URL
// has its whole userinfo replaced instead.
//
// An OPAQUE parse counts as a failure here for the same reason. `user:TOKEN@host:path`
// parses cleanly as scheme "user" with everything after the colon opaque, so
// url.URL.User is nil and redactURL again finds nothing to strip.
//
// The ssh exemption that keeps a login name visible is not extended to a
// string we could not parse — with no trustworthy scheme we cannot tell a
// login name from a token — with one exception: scp-like `user@host:path` has
// no password field at all, so its userinfo IS a login name.
func safeURL(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Opaque == "" {
		return redactURL(raw)
	}
	// The LAST "@" is the cut: everything before it goes, so no candidate
	// userinfo can survive even in a string too malformed to reason about.
	at := strings.LastIndex(raw, "@")
	if at < 0 {
		return raw // userinfo cannot exist without an "@"
	}
	if isSCPLike(raw, at) {
		return raw
	}
	return "***@" + raw[at+1:]
}

// isSCPLike reports whether raw is git's scp shorthand `user@host:path`, whose
// single userinfo field is a login name and never a password.
func isSCPLike(raw string, at int) bool {
	return !strings.Contains(raw, "://") &&
		at == strings.Index(raw, "@") &&
		!strings.Contains(raw[:at], ":")
}

// redactedError is an error whose MESSAGE has had a URL's credentials removed,
// while errors.Is and errors.As still reach the original underneath.
//
// Redacting our own "%s" of the URL is not enough, because the error we wrap
// quotes the URL too and neither layer below us is safe:
//
//   - net/http's *url.Error strips only the PASSWORD, so a token riding as the
//     username (`https://TOKEN@host/…`) is printed in full;
//   - net/url.Parse's own error quotes the ENTIRE input string, credentials
//     included — and an unparseable URL is exactly the case that reaches it.
//
// Both travel to the terminal through %w unless the text is rewritten here.
type redactedError struct {
	msg   string
	inner error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.inner }

// wrapURLError builds "<op> <safe url>: <scrubbed cause>" — the only way a
// transport error naming a source URL may be reported.
func wrapURLError(op, rawURL string, err error) error {
	return &redactedError{
		msg:   fmt.Sprintf("%s %s: %s", op, safeURL(rawURL), scrubURLSecrets(err.Error(), rawURL)),
		inner: err,
	}
}

// scrubURLSecrets removes rawURL's credentials from msg. It works on the TEXT
// rather than trusting the layer below, because the forms in which a transport
// error quotes a URL are not ours to control.
func scrubURLSecrets(msg, rawURL string) string {
	userinfo, password := urlCredentials(rawURL)
	if userinfo != "" {
		msg = strings.ReplaceAll(msg, userinfo+"@", "***@")
	}
	// The password on its own, wherever it appears: a layer below may have
	// quoted just that field, and unlike a username a token cannot plausibly
	// collide with ordinary words in a message.
	if password != "" {
		msg = strings.ReplaceAll(msg, password, "***")
	}
	return msg
}

// urlCredentials returns rawURL's userinfo exactly as the URL spells it, plus
// its password field alone. It follows redactURL's rules: the whole userinfo is
// secret over http(s) (a token may be either field), only the password is
// secret elsewhere, and scp-like `user@host:path` has no secret at all.
func urlCredentials(rawURL string) (userinfo, password string) {
	if u, err := url.Parse(rawURL); err == nil && u.Opaque == "" {
		if u.User == nil {
			return "", ""
		}
		pw, _ := u.User.Password()
		if u.Scheme != "http" && u.Scheme != "https" {
			return "", pw
		}
		return u.User.String(), pw
	}
	at := strings.LastIndex(rawURL, "@")
	if at < 0 || isSCPLike(rawURL, at) {
		return "", ""
	}
	userinfo = rawURL[:at]
	if i := strings.Index(userinfo, "://"); i >= 0 {
		userinfo = userinfo[i+3:]
	}
	if i := strings.Index(userinfo, ":"); i >= 0 {
		password = userinfo[i+1:]
	}
	return userinfo, password
}

// urlCarriesCredentials reports whether raw embeds http(s) userinfo. A 401
// against such a URL must not tell the user to "pass --token": they already
// passed one, in the URL. An ssh username is a login name, not a credential,
// so it does not count.
func urlCarriesCredentials(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

// defaultTokenUser is the basic-auth username an access token rides in when
// none is given. The token is always the PASSWORD; the username is ignored by
// GitHub and GitLab, but Bitbucket requires "x-token-auth" — which is the whole
// reason --username exists.
const defaultTokenUser = "git"

// defaultSSHUser is the login name used when an ssh URL names none. It is what
// GitHub, GitLab and Bitbucket all expect. It shares a value with
// defaultTokenUser by coincidence only: one is an ssh login, the other a
// basic-auth field, and --username retargets only the latter.
const defaultSSHUser = "git"

// authOpts is the credential material for one run, from flags and the
// environment. Nothing here is ever written to disk.
type authOpts struct {
	token     string
	tokenFile string
	username  string
	sshKey    string
	sshPass   string

	// Whether the credential came from the COMMAND LINE rather than the
	// environment, recorded by resolve before it folds the two together.
	// checkAuthApplies rejects only the former: a flag is something the user
	// asked THIS run to do, while $KNOMIT_OKF_* is ambient CI configuration
	// that must not break an unrelated fetch over another transport.
	tokenFromFlag  bool
	sshKeyFromFlag bool
}

// registerAuthFlags attaches the credential flags. All three fetching commands
// share them, so they are registered in one place rather than three.
func registerAuthFlags(fs *flag.FlagSet, o *authOpts) {
	fs.StringVar(&o.token, "token", "", "access token for an HTTPS source (or $KNOMIT_OKF_TOKEN)")
	fs.StringVar(&o.tokenFile, "token-file", "", "read the access token from a file")
	fs.StringVar(&o.username, "username", "", "basic-auth user for the token (default \"git\"; Bitbucket needs \"x-token-auth\")")
	fs.StringVar(&o.sshKey, "ssh-key", "", "path to an SSH private key (or $KNOMIT_OKF_SSH_KEY)")
}

// validate checks the flag combination alone. It is separate from resolve so a
// command that never fetches — `branches --no-fetch` — still rejects a
// contradictory command line instead of ignoring it, without reading a file or
// touching the environment.
func (o *authOpts) validate() error {
	if o.token != "" && o.tokenFile != "" {
		return errors.New("--token and --token-file are mutually exclusive")
	}
	return nil
}

// specified reports whether any credential FLAG was given, so a command that
// will not use them can say so rather than ignore them silently. Environment
// variables are deliberately excluded: $KNOMIT_OKF_TOKEN is ambient CI
// configuration, not something the user asked this run to do.
func (o authOpts) specified() bool {
	return o.token != "" || o.tokenFile != "" || o.username != "" || o.sshKey != ""
}

// resolve folds the environment into the flags. Flags win; the environment is
// the CI path. The SSH passphrase is environment-ONLY: a passphrase on argv
// would be visible in `ps` to every user on the machine.
func (o *authOpts) resolve() error {
	if err := o.validate(); err != nil {
		return err
	}
	o.tokenFromFlag = o.token != "" || o.tokenFile != ""
	o.sshKeyFromFlag = o.sshKey != ""
	if o.tokenFile != "" {
		raw, err := os.ReadFile(o.tokenFile)
		if err != nil {
			return fmt.Errorf("read token file: %w", err)
		}
		o.token = strings.TrimSpace(string(raw))
		if o.token == "" {
			// An explicitly named credential source that yields nothing is an
			// error, never a reason to fall through to $KNOMIT_OKF_TOKEN — the
			// same rule an unloadable --ssh-key already follows. Falling back
			// here would let the environment override the file the user named,
			// and send a DIFFERENT token than the one they chose.
			return fmt.Errorf("token file %s is empty", o.tokenFile)
		}
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

// checkAuthApplies rejects a credential the user passed EXPLICITLY that this
// URL's transport cannot use.
//
// Without it the flag is dropped on the floor: `--ssh-key` against an https
// source fetches anonymously and then fails with a 401 whose hint says "pass
// --token", and `--token` against an ssh source falls through to whatever the
// agent happens to hold. Both send the user hunting for a credential problem
// that is really a command-line problem. An unusable flag is an error for the
// same reason an unloadable --ssh-key is: the user asked for THIS credential.
//
// Only flags are rejected — see authOpts on why the environment is exempt.
func (o authOpts) checkAuthApplies(ep *transport.Endpoint) error {
	httpOnly := "--token/--token-file/--username"
	switch ep.Protocol {
	case "http", "https":
		if o.sshKeyFromFlag {
			return fmt.Errorf("--ssh-key cannot be used with an %s source: pass --token <access-token> instead", ep.Protocol)
		}
		// --username only ever names the basic-auth field a TOKEN rides in, so
		// on its own it authenticates nothing and the fetch would go out
		// anonymously — the same silently-dropped flag as the two cases above.
		if o.username != "" && o.token == "" {
			return errors.New("--username sets the basic-auth user for a token, so it does nothing on its own: pass --token <access-token> (or $KNOMIT_OKF_TOKEN) as well")
		}
	case "ssh":
		if o.tokenFromFlag || o.username != "" {
			return fmt.Errorf("%s cannot be used with an ssh source: pass --ssh-key <path> instead, or use the https URL for this repository", httpOnly)
		}
	default:
		// A local path, and anything else go-git resolves without a network
		// peer. Nothing to authenticate against at all.
		if o.tokenFromFlag || o.username != "" || o.sshKeyFromFlag {
			return fmt.Errorf("credential flags cannot be used with a local source (%s): it is read directly from disk", ep.Protocol)
		}
	}
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
	if err := o.checkAuthApplies(ep); err != nil {
		return nil, err
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
		user = defaultSSHUser
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

// hostKeyProblem is which of the two known_hosts failures happened. They read
// alike and are fixed by opposite actions, so they are classified rather than
// lumped together under the substring "knownhosts".
type hostKeyProblem int

const (
	hostKeyNone hostKeyProblem = iota
	// hostKeyUnknown: the host has no entry at all — a first contact.
	hostKeyUnknown
	// hostKeyMismatch: the host HAS an entry and the key presented differs.
	// Possibly a re-keyed server, possibly a machine-in-the-middle.
	hostKeyMismatch
)

// classifyHostKeyError prefers the typed error x/crypto/ssh/knownhosts returns,
// whose Want field distinguishes the two exactly: populated for a mismatch,
// empty for an unknown host. The substring fallback exists because the typed
// error does not survive every transport's wrapping, and losing the hint
// entirely on a dependency update would be worse than a fuzzy match.
func classifyHostKeyError(err error) hostKeyProblem {
	var ke *knownhosts.KeyError
	if errors.As(err, &ke) {
		if len(ke.Want) > 0 {
			return hostKeyMismatch
		}
		return hostKeyUnknown
	}
	// Kept narrow on purpose. A looser "host key" match would also catch
	// unrelated handshake failures — "ssh: no common algorithm for host key" —
	// and dress a benign negotiation problem up as a possible interception.
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "knownhosts") && !strings.Contains(msg, "known_hosts") {
		return hostKeyNone
	}
	// Default to the SAFE reading: an unrecognised host-key error is treated as
	// a mismatch, whose advice is "verify it" rather than "trust it".
	if strings.Contains(msg, "key is unknown") || strings.Contains(msg, "not in known") {
		return hostKeyUnknown
	}
	return hostKeyMismatch
}

// hostOf names the host in rawURL, reporting whether it could.
//
// transport.NewEndpoint's only failure path for a non-scp-like, non-file URL is
// net/url.Parse itself — the exact parse redactURL runs. So whenever it fails,
// redactURL has ALSO failed and silently returned rawURL unredacted, and safeURL
// would be no safer than the raw string. Callers print a placeholder instead of
// a URL that may carry a credential.
func hostOf(rawURL string) (host string, ok bool) {
	ep, err := transport.NewEndpoint(rawURL)
	if err != nil || ep.Host == "" {
		return "", false
	}
	return ep.Host, true
}

// explainFetchError adds the hint a user can act on, for the two failures that
// are otherwise opaque. Every other error passes through untouched — a
// misleading auth hint on a network error costs more than no hint at all.
func explainFetchError(err error, rawURL string, o authOpts) error {
	if err == nil {
		return nil
	}
	safe := safeURL(rawURL)

	// go-git verifies known_hosts with no interactive prompt, so the only way
	// out is for the message to carry the command that fixes it — but WHICH
	// command depends on which failure, and the two must never be confused.
	switch classifyHostKeyError(err) {
	case hostKeyMismatch:
		// Emphatically NOT the ssh-keyscan advice below: appending the key that
		// just failed to match would silence the warning by trusting whatever
		// answered, which is exactly what the check exists to prevent.
		host, named := hostOf(rawURL)
		if !named {
			return fmt.Errorf("the SSH host key does NOT match the one recorded in your known_hosts (the source URL could not be parsed to name the host)\n  this is a re-keyed server or a machine-in-the-middle — do NOT run ssh-keyscan to silence it\n  hint: verify the new fingerprint out of band, then drop the stale entry: ssh-keygen -R <host>\n  (original: %w)", err)
		}
		return fmt.Errorf("the SSH host key for %s does NOT match the one recorded in your known_hosts\n  this is a re-keyed server or a machine-in-the-middle — do NOT run ssh-keyscan to silence it\n  hint: verify the new fingerprint out of band, then drop the stale entry: ssh-keygen -R %s\n  (original: %w)",
			host, host, err)
	case hostKeyUnknown:
		host, named := hostOf(rawURL)
		if !named {
			return fmt.Errorf("the SSH source is not in your known_hosts (its URL could not be parsed to name the host)\n  hint: find the host from your source URL and run: ssh-keyscan <host> >> ~/.ssh/known_hosts\n  (original: %w)", err)
		}
		return fmt.Errorf("%s is not in your known_hosts\n  hint: ssh-keyscan %s >> ~/.ssh/known_hosts\n  (original: %w)",
			host, host, err)
	}

	if errors.Is(err, transport.ErrAuthenticationRequired) ||
		errors.Is(err, transport.ErrAuthorizationFailed) {
		if o.token == "" {
			// Credentials already in the URL are credentials: telling this user
			// to "pass --token" would send them looking for something they
			// already supplied, when the real answer is that what they supplied
			// was refused.
			if urlCarriesCredentials(rawURL) {
				return fmt.Errorf("%s rejected the credentials embedded in the source URL: check they are valid and have read access (a token can be passed with --token <access-token> instead): %w", safe, err)
			}
			return fmt.Errorf("%s requires credentials: pass --token <access-token> (or $KNOMIT_OKF_TOKEN): %w", safe, err)
		}
		return fmt.Errorf("%s rejected the token: check it has read access, and that --username is right for this host (Bitbucket needs \"x-token-auth\"): %w", safe, err)
	}
	return err
}
