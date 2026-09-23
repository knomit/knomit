package oauth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"knomit/internal/config"
)

// ErrInvalidClient is every reason a client_id does not resolve. The
// authorize endpoint shows it on a page (never a redirect: the redirect_uri
// cannot be trusted without the client); the token endpoint answers
// invalid_client.
var ErrInvalidClient = errors.New("oauth: invalid client")

// Client is a resolved client: pre-registered in knomit.toml, or described by
// a Client ID Metadata Document at its https client_id.
//
// A CIMD client_id proves who controls a DOMAIN, not which process on the
// user's machine is asking — any local program can present a legitimate
// client's id and a loopback redirect. That is why the operator sees the
// requester's address and user agent before approving, and why PKCE, not the
// client id, is what binds the code to the process that started the flow.
type Client struct {
	ID           string
	Name         string
	RedirectURIs []string
	Metadata     bool // resolved from a CIMD, not configured
}

// RedirectAllowed matches uri against the registered list: exactly, except
// that a registered http://127.0.0.1/... or http://[::1]/... accepts any port
// (RFC 8252 §7.3), because a native client listens on an ephemeral one.
// localhost is NOT treated that way: a name can be re-pointed, an IP literal
// cannot, and the RFC recommends the literal for exactly that reason.
func (c Client) RedirectAllowed(uri string) bool {
	req, err := url.Parse(uri)
	if err != nil || uri == "" || req.Fragment != "" || strings.Contains(uri, "#") {
		return false
	}
	for _, reg := range c.RedirectURIs {
		if reg == uri {
			return true
		}
		ru, err := url.Parse(reg)
		if err != nil || ru.Scheme != "http" || !isLoopbackLiteral(ru.Hostname()) {
			continue
		}
		if req.Scheme == "http" && req.Hostname() == ru.Hostname() &&
			req.EscapedPath() == ru.EscapedPath() && req.RawQuery == ru.RawQuery && !req.ForceQuery &&
			req.User == nil && ru.User == nil {
			return true
		}
	}
	return false
}

func isLoopbackLiteral(h string) bool { return h == "127.0.0.1" || h == "::1" }

// Resolver turns a client_id into a Client: a pre-registered client first,
// then a CIMD when the id is an https URL, else ErrInvalidClient. Dynamic
// Client Registration is phase 3b.
type Resolver struct {
	static map[string]Client
	fetch  *cimdFetcher
	now    func() time.Time

	mu    sync.Mutex
	cache map[string]cachedClient
}

type cachedClient struct {
	client  Client
	expires time.Time
}

// cimdMaxAge caps how long a metadata document is trusted, whatever its
// Cache-Control says: a client that changes its redirect URIs is picked up
// within the hour.
const cimdMaxAge = time.Hour

// cimdCacheEntries bounds the cache so a stream of distinct client_id URLs
// cannot grow it without limit; beyond it, expired entries are dropped and
// then the whole cache if still full (it is only a cache).
const cimdCacheEntries = 256

func NewResolver(clients []config.OAuthClient) *Resolver {
	r := &Resolver{static: map[string]Client{}, fetch: newCIMDFetcher(), now: time.Now, cache: map[string]cachedClient{}}
	for _, c := range clients {
		r.static[c.ID] = Client{ID: c.ID, Name: c.Name, RedirectURIs: append([]string(nil), c.RedirectURIs...)}
	}
	return r
}

func (r *Resolver) Resolve(ctx context.Context, id string) (Client, error) {
	if c, ok := r.static[id]; ok {
		return c, nil
	}
	if !isCIMDURL(id) {
		return Client{}, fmt.Errorf("%w: %q is neither pre-registered nor an https client metadata URL", ErrInvalidClient, id)
	}
	now := r.now()
	r.mu.Lock()
	if e, ok := r.cache[id]; ok && now.Before(e.expires) {
		r.mu.Unlock()
		return e.client, nil
	}
	r.mu.Unlock()

	c, maxAge, err := r.fetch.fetch(ctx, id)
	if err != nil {
		return Client{}, fmt.Errorf("%w: %w", ErrInvalidClient, err)
	}
	if maxAge > cimdMaxAge {
		maxAge = cimdMaxAge
	}
	if maxAge > 0 {
		r.mu.Lock()
		if len(r.cache) >= cimdCacheEntries {
			for k, e := range r.cache {
				if !now.Before(e.expires) {
					delete(r.cache, k)
				}
			}
			if len(r.cache) >= cimdCacheEntries {
				r.cache = map[string]cachedClient{}
			}
		}
		r.cache[id] = cachedClient{client: c, expires: now.Add(maxAge)}
		r.mu.Unlock()
	}
	return c, nil
}

// isCIMDURL: https, a host, a path other than "/", and no userinfo, query or
// fragment — the shape the CIMD draft requires of a client_id.
func isCIMDURL(id string) bool {
	if !printableASCII(id) {
		return false
	}
	u, err := url.Parse(id)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return false
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(id, "#") {
		return false
	}
	return u.EscapedPath() != "" && u.EscapedPath() != "/"
}

// --- fetching a Client ID Metadata Document (R7) ---------------------------

var (
	errResolvedForbidden = errors.New("client metadata host resolves to a forbidden address")
	errDialForbidden     = errors.New("refusing to connect to a forbidden address")
	// errUnsafeClientMetadata: a displayed field of a CIMD carries a control
	// or format character, or is too long. The document is refused whole.
	errUnsafeClientMetadata = errors.New("client metadata carries a control or format character, or an over-long name")
)

// maxClientNameRunes caps client_name: long enough for any product name,
// short enough that it cannot push the rest of the pending display away.
const maxClientNameRunes = 100

// cimdFetcher fetches a metadata document with the SSRF guards the MCP spec
// now says the AUTHORIZATION SERVER needs, since the client_id is a URL an
// attacker chooses:
//
//   - resolve the host ONCE and refuse if ANY address is forbidden;
//   - dial the address that was checked, never the name again (rebinding),
//     and re-check the address actually connected to in Dialer.Control;
//   - no proxy, no redirects, TLS verified (system roots), a 64 KiB cap,
//     a 5 s total timeout, and a JSON content type.
//
// lookup, allow and roots are fields only so tests can point the fetcher at
// an in-process server; production uses the zero-configuration values.
type cimdFetcher struct {
	lookup  func(ctx context.Context, host string) ([]net.IP, error)
	allow   func(net.IP) bool
	roots   *x509.CertPool // nil = system roots
	timeout time.Duration
	max     int64
}

func newCIMDFetcher() *cimdFetcher {
	return &cimdFetcher{
		lookup: func(ctx context.Context, host string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(ctx, "ip", host)
		},
		allow:   func(ip net.IP) bool { return !forbiddenIP(ip) },
		timeout: 5 * time.Second,
		max:     64 << 10,
	}
}

// dialer re-checks the address it is about to connect to. The fetch dials an
// IP literal, so this sees exactly the address that was resolved and checked;
// it is the second half of the rebinding defence, not a replacement for the
// first.
func (f *cimdFetcher) dialer() *net.Dialer {
	return &net.Dialer{
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(host); ip == nil || !f.allow(ip) {
				return fmt.Errorf("%w: %s", errDialForbidden, host)
			}
			return nil
		},
	}
}

type cimdDocument struct {
	ClientID                string   `json:"client_id"`
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
}

func (f *cimdFetcher) fetch(ctx context.Context, id string) (Client, time.Duration, error) {
	u, err := url.Parse(id)
	if err != nil {
		return Client{}, 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()

	host := u.Hostname()
	ips, err := f.lookup(ctx, host)
	if err != nil {
		return Client{}, 0, fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(ips) == 0 {
		return Client{}, 0, fmt.Errorf("resolve %s: no addresses", host)
	}
	for _, ip := range ips {
		if !f.allow(ip) {
			return Client{}, 0, fmt.Errorf("%w: %s -> %s", errResolvedForbidden, host, ip)
		}
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	pinned := net.JoinHostPort(ips[0].String(), port)
	d := f.dialer()
	// ONE deadline for the whole fetch — DNS, connect, TLS, headers, body —
	// carried by ctx; no per-phase timeouts beside it.
	client := &http.Client{
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return d.DialContext(ctx, network, pinned)
			},
			// ServerName comes from the request URL's host, so the
			// certificate is verified against the NAME the client_id carries,
			// not the pinned address.
			TLSClientConfig:   &tls.Config{RootCAs: f.roots, MinVersion: tls.VersionTLS12},
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("client metadata documents may not redirect")
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, id, nil)
	if err != nil {
		return Client{}, 0, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return Client{}, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Client{}, 0, fmt.Errorf("client metadata: HTTP %d", resp.StatusCode)
	}
	mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || (mt != "application/json" && !strings.HasSuffix(mt, "+json")) {
		return Client{}, 0, fmt.Errorf("client metadata: content type %q is not JSON", resp.Header.Get("Content-Type"))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, f.max+1))
	if err != nil {
		return Client{}, 0, err
	}
	if int64(len(body)) > f.max {
		return Client{}, 0, fmt.Errorf("client metadata: larger than %d bytes", f.max)
	}
	var doc cimdDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		return Client{}, 0, fmt.Errorf("client metadata: %w", err)
	}
	if doc.ClientID != id {
		return Client{}, 0, fmt.Errorf("client metadata: document names client_id %q, fetched from %q", doc.ClientID, id)
	}
	if len(doc.RedirectURIs) == 0 {
		return Client{}, 0, errors.New("client metadata: no redirect_uris")
	}
	// Nothing from the document reaches the operator's terminal unless it is
	// inert there: `knomit oauth pending` is the only consent screen, and an
	// ESC in a name can conceal the lines that follow it (review B2). Refused
	// here, at ingest, so nothing unsafe is ever stored.
	if !displaySafe(doc.ClientName) || utf8.RuneCountInString(doc.ClientName) > maxClientNameRunes {
		return Client{}, 0, fmt.Errorf("%w: client_name %q", errUnsafeClientMetadata, doc.ClientName)
	}
	for _, u := range doc.RedirectURIs {
		if !printableASCII(u) {
			return Client{}, 0, fmt.Errorf("%w: redirect_uri %q", errUnsafeClientMetadata, u)
		}
	}
	if m := doc.TokenEndpointAuthMethod; m != "" && m != "none" {
		return Client{}, 0, fmt.Errorf("client metadata: token_endpoint_auth_method %q; only public clients (none) are supported", m)
	}
	return Client{ID: id, Name: doc.ClientName, RedirectURIs: doc.RedirectURIs, Metadata: true},
		maxAgeOf(resp.Header.Get("Cache-Control")), nil
}

// displaySafe: valid UTF-8 and every rune printable — no control (Cc) or
// format (Cf, which includes the bidi overrides) character, no other
// whitespace than the ASCII space.
func displaySafe(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// printableASCII: every byte in 0x21..0x7e. What a URL shown to the operator
// may contain.
func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// maxAgeOf reads max-age from Cache-Control; no-store or no-cache, or no
// max-age at all, is 0 (do not cache).
func maxAgeOf(cc string) time.Duration {
	var age time.Duration
	for _, d := range strings.Split(cc, ",") {
		d = strings.TrimSpace(strings.ToLower(d))
		switch {
		case d == "no-store" || d == "no-cache":
			return 0
		case strings.HasPrefix(d, "max-age="):
			if n, err := strconv.Atoi(strings.TrimPrefix(d, "max-age=")); err == nil && n > 0 {
				age = time.Duration(n) * time.Second
			}
		}
	}
	return age
}

// forbidden are the ranges a client metadata URL may not resolve into (R7):
// loopback, private, CGNAT, 0/8, link-local, ULA, multicast, unspecified.
var forbidden = func() []*net.IPNet {
	var out []*net.IPNet
	for _, c := range []string{
		"127.0.0.0/8", "::1/128",
		"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
		"100.64.0.0/10",
		"0.0.0.0/8",
		"169.254.0.0/16", "fe80::/10",
		"fc00::/7",
		"224.0.0.0/4", "ff00::/8",
		"::/128",
	} {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic(err)
		}
		out = append(out, n)
	}
	return out
}()

// forbiddenIP reports whether ip is in a forbidden range. An IPv4-mapped IPv6
// address is judged as the IPv4 address it carries: net.IPNet.Contains does
// that conversion itself for a 4-byte network.
func forbiddenIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	for _, n := range forbidden {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
