package oauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"knomit/internal/config"
)

// --- redirect matching -----------------------------------------------------

func TestClient_RedirectAllowed(t *testing.T) {
	c := Client{ID: "x", RedirectURIs: []string{
		"https://app.example/cb",
		"http://127.0.0.1/callback",
		"http://[::1]/callback",
		"http://localhost:8765/cb",
	}}
	for uri, want := range map[string]bool{
		"https://app.example/cb":           true,
		"https://app.example/cb/":          false, // exact, not prefix
		"https://app.example/cb?x=1":       false,
		"https://app.example:8443/cb":      false,
		"http://app.example/cb":            false,
		"http://127.0.0.1:54321/callback":  true, // RFC 8252 §7.3: loopback port is ignored
		"http://127.0.0.1/callback":        true,
		"http://[::1]:54321/callback":      true,
		"http://127.0.0.1:54321/other":     false, // path still exact
		"http://127.0.0.1:54321/callback?": false,
		"https://127.0.0.1:54321/callback": false, // scheme still exact
		"http://localhost:8765/cb":         true,  // exact match of what is registered
		"http://localhost:9999/cb":         true,  // phase 3b R1: localhost is a port wildcard too
		"http://localhost:9999/other":      false,
		"http://127.0.0.1:54321/callback#": false,
		"":                                 false,
	} {
		if got := c.RedirectAllowed(uri); got != want {
			t.Errorf("RedirectAllowed(%q) = %v, want %v", uri, got, want)
		}
	}
}

// Phase 3b R1: a registered http://localhost[:port]/<path> accepts any port,
// as the loopback literals do, because Claude Code's CIMD registers
// http://localhost/callback and presents http://localhost:<random>/callback.
// Everything else stays exact: the host must be the same loopback spelling
// (localhost never matches a 127.0.0.1 registration or the reverse), and
// nothing that merely looks like localhost is loopback.
//
// Sabotage (run 2026-09-24): dropping "localhost" from isLoopbackHost fails
// the first row and the Claude Code CIMD test; widening it to
// strings.HasPrefix(h, "localhost") fails only the LOOKALIKE REGISTRATION
// block below — a lookalike REDIRECT is already refused by the same-host
// comparison, so the rows above cannot see that sabotage.
func TestClient_RedirectAllowed_LocalhostAnyPort(t *testing.T) {
	c := Client{ID: "cc", RedirectURIs: []string{"http://localhost/callback"}}
	for uri, want := range map[string]bool{
		"http://localhost:52346/callback":              true,
		"http://localhost/callback":                    true,
		"http://localhost:52346/callback/":             false, // path exact
		"http://localhost:52346/callback?x=1":          false, // query exact
		"https://localhost:52346/callback":             false, // scheme exact
		"http://127.0.0.1:52346/callback":              false, // another loopback spelling is another URI
		"http://localhost.:52346/callback":             false,
		"http://LOCALHOST:52346/callback":              false,
		"http://localhost.evil.example:52346/callback": false,
		"http://sub.localhost:52346/callback":          false,
		"http://user@localhost:52346/callback":         false,
	} {
		if got := c.RedirectAllowed(uri); got != want {
			t.Errorf("RedirectAllowed(%q) = %v, want %v", uri, got, want)
		}
	}
	// A registration that merely looks like localhost is an ordinary host:
	// exact match only, never a port wildcard.
	for _, reg := range []string{
		"http://localhost./callback", "http://localhost.evil.example/callback",
		"http://localhostx/callback", "http://LOCALHOST/callback",
	} {
		look := Client{ID: "look", RedirectURIs: []string{reg}}
		u, _ := url.Parse(reg)
		other := "http://" + u.Hostname() + ":52346/callback"
		if look.RedirectAllowed(other) {
			t.Errorf("registration %q became a port wildcard: accepted %q", reg, other)
		}
	}
	// And a 127.0.0.1 registration still refuses a localhost redirect.
	lit := Client{ID: "kb", RedirectURIs: []string{"http://127.0.0.1/callback"}}
	if lit.RedirectAllowed("http://localhost:52346/callback") {
		t.Error("a 127.0.0.1 registration accepted a localhost redirect")
	}
}

// --- the address predicate (R7) --------------------------------------------

func TestForbiddenIP(t *testing.T) {
	for _, s := range []string{
		"127.0.0.1", "127.9.9.9", "::1", // loopback
		"10.1.2.3", "172.16.0.1", "172.31.255.255", "192.168.0.1", // private
		"100.64.0.1", "100.127.255.255", // CGNAT
		"0.0.0.0", "0.1.2.3", // 0/8
		"169.254.169.254", "fe80::1", // link-local (the metadata endpoint)
		"fc00::1", "fd12:3456::1", // ULA
		"224.0.0.1", "ff02::1", // multicast
		"::",               // unspecified
		"::ffff:10.0.0.1",  // IPv4-mapped private
		"::ffff:127.0.0.1", // IPv4-mapped loopback
	} {
		if !forbiddenIP(net.ParseIP(s)) {
			t.Errorf("%s allowed; want forbidden", s)
		}
	}
	for _, s := range []string{"93.184.216.34", "172.32.0.1", "100.128.0.1", "2606:4700::1111", "11.0.0.1"} {
		if forbiddenIP(net.ParseIP(s)) {
			t.Errorf("%s forbidden; want allowed", s)
		}
	}
}

// --- CIMD fixture ----------------------------------------------------------

// cimdFixture is an in-process HTTPS server for "client.example", with a
// certificate from a CA the fetcher is told to trust, reachable because the
// fetcher's lookup is pointed at 127.0.0.1 and its address predicate is
// widened to allow exactly that. Guard tests narrow them back one at a time.
type cimdFixture struct {
	srv     *httptest.Server
	roots   *x509.CertPool
	url     string // https://client.example:<port>/cimd.json
	hits    atomic.Int32
	handler atomic.Value // http.HandlerFunc
}

func newCIMDFixture(t *testing.T) *cimdFixture {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	caCert, _ := x509.ParseCertificate(caDER)
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "client.example"},
		DNSNames:  []string{"client.example"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, _ := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)

	f := &cimdFixture{roots: x509.NewCertPool()}
	f.roots.AddCert(caCert)
	f.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		f.handler.Load().(http.HandlerFunc)(w, r)
	}))
	f.srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}}}
	// The untrusted-certificate case makes the server log a handshake error;
	// it is the expected outcome, not noise worth printing.
	f.srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	f.srv.StartTLS()
	t.Cleanup(f.srv.Close)
	_, port, _ := net.SplitHostPort(f.srv.Listener.Addr().String())
	f.url = "https://client.example:" + port + "/cimd.json"
	f.serve(f.document(f.url), "max-age=600")
	return f
}

func (f *cimdFixture) document(clientID string) string {
	return fmt.Sprintf(`{"client_id":%q,"client_name":"Example","redirect_uris":["http://127.0.0.1/cb"],"token_endpoint_auth_method":"none"}`, clientID)
}

func (f *cimdFixture) serve(body, cacheControl string) {
	f.handler.Store(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if cacheControl != "" {
			w.Header().Set("Cache-Control", cacheControl)
		}
		_, _ = w.Write([]byte(body))
	}))
}

// fetcher returns a fetcher that can reach the fixture: lookup answers
// 127.0.0.1 for client.example and the predicate permits exactly 127.0.0.1.
func (f *cimdFixture) fetcher() *cimdFetcher {
	fe := newCIMDFetcher()
	fe.roots = f.roots
	fe.lookup = func(_ context.Context, host string) ([]net.IP, error) {
		if host != "client.example" {
			return nil, fmt.Errorf("no such host %q", host)
		}
		return []net.IP{net.ParseIP("127.0.0.1")}, nil
	}
	fe.allow = func(ip net.IP) bool { return ip.Equal(net.ParseIP("127.0.0.1")) }
	return fe
}

func newResolverFor(fe *cimdFetcher, c *clock, static ...config.OAuthClient) *Resolver {
	r := NewResolver(static)
	r.fetch = fe
	r.now = c.now
	return r
}

// --- resolution ------------------------------------------------------------

func TestResolver_PreRegisteredWins(t *testing.T) {
	f := newCIMDFixture(t)
	c := &clock{t: time.Unix(1_790_000_000, 0)}
	// A pre-registered client whose id is ALSO a fetchable CIMD URL: the
	// configured registration wins and nothing is fetched.
	r := newResolverFor(f.fetcher(), c, config.OAuthClient{ID: f.url, Name: "configured", RedirectURIs: []string{"https://app.example/cb"}})
	got, err := r.Resolve(context.Background(), f.url)
	if err != nil || got.Name != "configured" {
		t.Fatalf("Resolve = %+v, %v", got, err)
	}
	if f.hits.Load() != 0 {
		t.Fatal("pre-registered client fetched a metadata document")
	}
}

// claudeCodeCIMD is Claude Code's client metadata document exactly as
// https://claude.ai/oauth/claude-code-client-metadata served it on
// 2026-09-24 (Claude Code 2.1.281), with only client_id swapped for the
// fixture's URL. Claude Code then sent redirect_uri
// http://localhost:52346/callback (phase 3b worker notes, Q4).
const claudeCodeCIMD = `{"client_id":%q,"client_name":"Claude Code","client_uri":"https://claude.ai","redirect_uris":["http://localhost/callback","http://127.0.0.1/callback"],"grant_types":["authorization_code","refresh_token"],"response_types":["code"],"token_endpoint_auth_method":"none"}`

func TestResolver_ClaudeCodeCIMDAcceptsItsLocalhostRedirect(t *testing.T) {
	f := newCIMDFixture(t)
	f.serve(fmt.Sprintf(claudeCodeCIMD, f.url), "public, max-age=300")
	r := newResolverFor(f.fetcher(), &clock{t: time.Unix(1_790_000_000, 0)})
	got, err := r.Resolve(context.Background(), f.url)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Name != "Claude Code" || !got.RedirectAllowed("http://localhost:52346/callback") {
		t.Fatalf("client %+v refuses Claude Code's redirect http://localhost:52346/callback", got)
	}
}

func TestResolver_CIMDHappyPathAndCache(t *testing.T) {
	f := newCIMDFixture(t)
	c := &clock{t: time.Unix(1_790_000_000, 0)}
	r := newResolverFor(f.fetcher(), c)
	got, err := r.Resolve(context.Background(), f.url)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ID != f.url || got.Name != "Example" || !got.RedirectAllowed("http://127.0.0.1:5555/cb") {
		t.Fatalf("client = %+v", got)
	}
	// max-age=600 honoured: a second resolve inside it does not fetch.
	c.add(599 * time.Second)
	if _, err := r.Resolve(context.Background(), f.url); err != nil || f.hits.Load() != 1 {
		t.Fatalf("inside max-age: err=%v hits=%d, want 1", err, f.hits.Load())
	}
	c.add(time.Second)
	if _, err := r.Resolve(context.Background(), f.url); err != nil || f.hits.Load() != 2 {
		t.Fatalf("at max-age: err=%v hits=%d, want a refetch", err, f.hits.Load())
	}
}

func TestResolver_CacheCappedAtOneHour(t *testing.T) {
	f := newCIMDFixture(t)
	f.serve(f.document(f.url), "max-age=86400")
	c := &clock{t: time.Unix(1_790_000_000, 0)}
	r := newResolverFor(f.fetcher(), c)
	if _, err := r.Resolve(context.Background(), f.url); err != nil {
		t.Fatal(err)
	}
	c.add(time.Hour - time.Second)
	_, _ = r.Resolve(context.Background(), f.url)
	if f.hits.Load() != 1 {
		t.Fatalf("inside the cap: hits=%d", f.hits.Load())
	}
	c.add(time.Second)
	_, _ = r.Resolve(context.Background(), f.url)
	if f.hits.Load() != 2 {
		t.Fatalf("a day's max-age must be capped at 1h: hits=%d", f.hits.Load())
	}
}

func TestResolver_NoCacheWithoutMaxAge(t *testing.T) {
	f := newCIMDFixture(t)
	f.serve(f.document(f.url), "max-age=600, no-store") // no-store wins over max-age
	c := &clock{t: time.Unix(1_790_000_000, 0)}
	r := newResolverFor(f.fetcher(), c)
	_, _ = r.Resolve(context.Background(), f.url)
	_, _ = r.Resolve(context.Background(), f.url)
	if f.hits.Load() != 2 {
		t.Fatalf("no-store cached: hits=%d", f.hits.Load())
	}
}

func TestResolver_UnknownClient(t *testing.T) {
	r := NewResolver(nil)
	r.fetch.lookup = func(_ context.Context, host string) ([]net.IP, error) {
		t.Errorf("looked up %q: an id that is not a CIMD URL must be refused before any network", host)
		return nil, errors.New("no")
	}
	for _, id := range []string{"", "some-app", "http://client.example/cimd.json", "https://client.example", "https://client.example/", "https://client.example/c#f", "https://u:p@client.example/c", "https://client.example/c?x=1"} {
		if _, err := r.Resolve(context.Background(), id); !errors.Is(err, ErrInvalidClient) {
			t.Errorf("Resolve(%q): want ErrInvalidClient, got %v", id, err)
		}
	}
}

// Each guard, alone. The fixture's fetcher can reach the server; each case
// takes away exactly one thing and asserts the fetch is refused — so removing
// that one guard from the code turns its case green-to-red.
func TestCIMD_Guards(t *testing.T) {
	cases := []struct {
		name  string
		setup func(f *cimdFixture, fe *cimdFetcher)
		want  error // a more specific error than ErrInvalidClient, when two guards could both refuse
	}{
		// The resolve-time check, not the dialer's: errResolvedForbidden names
		// which one refused. Without it the dialer would still refuse, with
		// errDialForbidden, and this case would go red.
		{"private address", func(_ *cimdFixture, fe *cimdFetcher) {
			fe.allow = func(ip net.IP) bool { return !forbiddenIP(ip) }
			fe.lookup = func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("10.0.0.1")}, nil }
		}, errResolvedForbidden},
		{"any resolved address forbidden", func(_ *cimdFixture, fe *cimdFetcher) {
			fe.allow = func(ip net.IP) bool { return !forbiddenIP(ip) || ip.Equal(net.ParseIP("127.0.0.1")) }
			fe.lookup = func(context.Context, string) ([]net.IP, error) {
				return []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("10.0.0.1")}, nil
			}
		}, errResolvedForbidden},
		{"redirect", func(f *cimdFixture, _ *cimdFetcher) {
			f.handler.Store(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/moved.json" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(f.document(f.url)))
					return
				}
				http.Redirect(w, r, "/moved.json", http.StatusFound)
			}))
		}, nil},
		{"oversize", func(f *cimdFixture, _ *cimdFetcher) {
			pad := strings.Repeat(" ", 64<<10)
			f.serve(f.document(f.url)+pad, "")
		}, nil},
		{"slow", func(f *cimdFixture, fe *cimdFetcher) {
			fe.timeout = 200 * time.Millisecond
			f.handler.Store(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				time.Sleep(time.Second)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(f.document(f.url)))
			}))
		}, nil},
		// The deadline covers DNS too: a lookup that answers only after the
		// timeout must not be waited for.
		{"slow dns", func(_ *cimdFixture, fe *cimdFetcher) {
			fe.timeout = 200 * time.Millisecond
			fe.lookup = func(ctx context.Context, _ string) ([]net.IP, error) {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(2 * time.Second):
					return []net.IP{net.ParseIP("127.0.0.1")}, nil
				}
			}
		}, nil},
		{"not json", func(f *cimdFixture, _ *cimdFetcher) {
			f.handler.Store(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				_, _ = w.Write([]byte(f.document(f.url)))
			}))
		}, nil},
		{"untrusted certificate", func(_ *cimdFixture, fe *cimdFetcher) {
			fe.roots = nil // system roots: the test CA is not among them
		}, nil},
		{"client_id mismatch", func(f *cimdFixture, _ *cimdFetcher) {
			f.serve(f.document("https://other.example/cimd.json"), "")
		}, nil},
		{"no redirect uris", func(f *cimdFixture, _ *cimdFetcher) {
			f.serve(fmt.Sprintf(`{"client_id":%q}`, f.url), "")
		}, nil},
		// Nothing from a CIMD document reaches a terminal unescaped (review
		// B2): a client_name or redirect URI carrying control or format
		// characters is refused at ingest, by name.
		{"client_name ESC", func(f *cimdFixture, _ *cimdFetcher) {
			f.serve(hostileDoc(f.url, "Claude Code\\u001b[8m", "http://127.0.0.1/cb"), "")
		}, errUnsafeClientMetadata},
		{"client_name CR LF", func(f *cimdFixture, _ *cimdFetcher) {
			f.serve(hostileDoc(f.url, "Claude Code\\r\\n  redirect\\thttp://127.0.0.1/cb", "http://127.0.0.1/cb"), "")
		}, errUnsafeClientMetadata},
		{"client_name TAB", func(f *cimdFixture, _ *cimdFetcher) {
			f.serve(hostileDoc(f.url, "a\\tb", "http://127.0.0.1/cb"), "")
		}, errUnsafeClientMetadata},
		{"client_name bidi override", func(f *cimdFixture, _ *cimdFetcher) {
			f.serve(hostileDoc(f.url, "edoC edualC\\u202e", "http://127.0.0.1/cb"), "")
		}, errUnsafeClientMetadata},
		{"client_name too long", func(f *cimdFixture, _ *cimdFetcher) {
			f.serve(hostileDoc(f.url, strings.Repeat("n", maxClientNameRunes+1), "http://127.0.0.1/cb"), "")
		}, errUnsafeClientMetadata},
		{"redirect uri with a format character", func(f *cimdFixture, _ *cimdFetcher) {
			f.serve(hostileDoc(f.url, "Example", "http://127.0.0.1/c\\u202eb"), "")
		}, errUnsafeClientMetadata},
		{"confidential client", func(f *cimdFixture, _ *cimdFetcher) {
			f.serve(fmt.Sprintf(`{"client_id":%q,"redirect_uris":["http://127.0.0.1/cb"],"token_endpoint_auth_method":"private_key_jwt"}`, f.url), "")
		}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCIMDFixture(t)
			fe := f.fetcher()
			tc.setup(f, fe)
			r := newResolverFor(fe, &clock{t: time.Unix(1_790_000_000, 0)})
			_, err := r.Resolve(context.Background(), f.url)
			if !errors.Is(err, ErrInvalidClient) {
				t.Fatalf("want ErrInvalidClient, got %v", err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("refused, but not by the guard under test: want %v, got %v", tc.want, err)
			}
		})
	}
}

// The dialer re-checks the address it actually connects to (the rebinding
// defence's second half): handed a forbidden literal directly, it refuses
// before connecting, whatever the lookup said.
func TestCIMD_DialerRechecksAddress(t *testing.T) {
	f := newCIMDFixture(t)
	fe := newCIMDFetcher() // production predicate
	_, port, _ := net.SplitHostPort(f.srv.Listener.Addr().String())
	conn, err := fe.dialer().DialContext(context.Background(), "tcp", net.JoinHostPort("127.0.0.1", port))
	if err == nil {
		conn.Close()
		t.Fatal("dialer connected to a loopback address")
	}
	if !errors.Is(err, errDialForbidden) {
		t.Fatalf("refused, but not by the address re-check: %v", err)
	}
	if f.hits.Load() != 0 {
		t.Fatal("request reached the server")
	}
}

// http:// and a CIMD URL without a path are not metadata documents at all.
func TestCIMD_OnlyHTTPS(t *testing.T) {
	f := newCIMDFixture(t)
	fe := f.fetcher()
	fe.lookup = func(context.Context, string) ([]net.IP, error) {
		t.Error("an http client_id reached DNS: it must be refused as not-a-CIMD first")
		return []net.IP{net.ParseIP("127.0.0.1")}, nil
	}
	r := newResolverFor(fe, &clock{t: time.Unix(1_790_000_000, 0)})
	u, _ := url.Parse(f.url)
	u.Scheme = "http"
	if _, err := r.Resolve(context.Background(), u.String()); !errors.Is(err, ErrInvalidClient) {
		t.Fatalf("http CIMD: %v", err)
	}
	if f.hits.Load() != 0 {
		t.Fatal("fetched over http")
	}
}

// hostileDoc builds a document with JSON-escaped name and redirect.
func hostileDoc(id, name, redirect string) string {
	return `{"client_id":"` + id + `","client_name":"` + name + `","redirect_uris":["` + redirect + `"]}`
}

// A printable name, including non-ASCII letters, passes.
func TestCIMD_PrintableNamePasses(t *testing.T) {
	f := newCIMDFixture(t)
	f.serve(hostileDoc(f.url, "Clóde Cöde — 日本", "http://127.0.0.1/cb"), "")
	c, err := newResolverFor(f.fetcher(), &clock{t: time.Unix(1_790_000_000, 0)}).Resolve(context.Background(), f.url)
	if err != nil || c.Name != "Clóde Cöde — 日本" {
		t.Fatalf("%+v %v", c, err)
	}
}
