package knomitapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"knomit/internal/auth"
	"knomit/internal/config"
	"knomit/internal/oauth"
	"knomit/internal/store/migrate"
)

// issuerFixture is a knomit issuer in-process: the real oauth package on a
// real control.db, served at an httptest URL that IS the issuer (a loopback
// http issuer is valid), plus one protected endpoint judged by the real
// verifier. tokenHits counts /oauth/token calls so "one refresh" is asserted.
type issuerFixture struct {
	srv       *httptest.Server
	iss       *oauth.Issuer
	store     *oauth.Store
	tokenHits atomic.Int32
}

func newIssuerFixture(t *testing.T, accessTTL time.Duration) *issuerFixture {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "control.db")+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if err := migrate.Control(db); err != nil {
		t.Fatal(err)
	}
	f := &issuerFixture{store: oauth.NewStore(db, accessTTL, 14*24*time.Hour)}
	grants := auth.NewSQLGrants(db)
	var h http.Handler
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	t.Cleanup(f.srv.Close)
	f.iss = oauth.NewIssuer(oauth.Options{
		Issuer: f.srv.URL, Store: f.store, Grants: grants,
		Clients: oauth.NewResolver(config.Defaults().OAuth.EffectiveClients()),
	})
	v := oauth.NewVerifier(f.srv.URL, f.store)
	routes := f.iss.Routes()
	h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/oauth/token":
			f.tokenHits.Add(1)
			routes.ServeHTTP(w, r)
		case oauth.IsPublicPath(r.URL.Path):
			routes.ServeHTTP(w, r)
		case r.URL.Path == "/proxy-challenge":
			w.Header().Set("WWW-Authenticate", `Basic realm="proxy"`)
			w.WriteHeader(http.StatusUnauthorized)
		case r.URL.Path == "/protected":
			tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			p, _, err := v.Verify(r.Context(), tok, r.URL.Path)
			if err != nil {
				w.Header().Set("WWW-Authenticate", `Bearer realm="knomit", error="invalid_token"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			body, _ := io.ReadAll(r.Body)
			_, _ = io.WriteString(w, p.String()+" "+string(body))
		default:
			http.NotFound(w, r)
		}
	})
	return f
}

// browser plays the user: it opens the authorize URL, the operator approves
// the request it parked, and it follows /wait to kb's loopback callback.
// mutate, when set, rewrites the callback URL before following it.
func (f *issuerFixture) browser(t *testing.T, mutate func(*url.URL)) func(string) error {
	return func(authorizeURL string) error {
		go func() {
			noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			resp, err := noFollow.Get(authorizeURL)
			if err != nil {
				t.Error(err)
				return
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("authorize: %d", resp.StatusCode)
				return
			}
			list, _ := f.iss.Pending(context.Background())
			if len(list) != 1 {
				t.Errorf("pending: %d", len(list))
				return
			}
			if _, err := f.iss.Approve(context.Background(), list[0].ID, "laptop", nil, "test"); err != nil {
				t.Error(err)
				return
			}
			resp, err = noFollow.Get(f.srv.URL + "/oauth/authorize/" + list[0].ID + "/wait")
			if err != nil {
				t.Error(err)
				return
			}
			resp.Body.Close()
			cb, _ := url.Parse(resp.Header.Get("Location"))
			if mutate != nil {
				mutate(cb)
			}
			if resp, err := http.Get(cb.String()); err == nil {
				resp.Body.Close()
			}
		}()
		return nil
	}
}

func useHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("KNOMIT_HOME", home)
	return home
}

func login(t *testing.T, f *issuerFixture, mutate func(*url.URL)) (*Credentials, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return Login(ctx, f.srv.URL, LoginOptions{OpenBrowser: f.browser(t, mutate), Out: io.Discard})
}

func TestLogin_FullFlowWritesCredentialsAt0600(t *testing.T) {
	home := useHome(t)
	f := newIssuerFixture(t, 2*time.Hour)
	creds, err := login(t, f, nil)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if creds.Issuer != f.srv.URL || creds.Resource != f.srv.URL || creds.AccessToken == "" || creds.RefreshToken == "" ||
		creds.ClientID != "kb" || creds.Scope != "read write" {
		t.Fatalf("credentials = %+v", creds)
	}
	u, _ := url.Parse(f.srv.URL)
	path := filepath.Join(home, "credentials", "127.0.0.1_"+u.Port())
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("credentials file: %v", err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("credentials mode %v, want 0600", fi.Mode().Perm())
	}
	raw, _ := os.ReadFile(path)
	var onDisk map[string]any
	if err := json.Unmarshal(raw, &onDisk); err != nil || onDisk["access_token"] != creds.AccessToken || onDisk["issuer"] != f.srv.URL {
		t.Fatalf("file content: %s", raw)
	}

	// The bridge's clients now carry the token to that host: the explicit-URL
	// client and the socket-preferring one (no socket here, so it dials TCP).
	for name, c := range map[string]*http.Client{
		"explicit": NewHTTPClient("", true, 5*time.Second),
		"socket":   NewHTTPClient("", false, 5*time.Second),
	} {
		resp, err := c.Get(f.srv.URL + "/protected")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.HasPrefix(string(b), "host:laptop@token") {
			t.Fatalf("%s client: %d %s", name, resp.StatusCode, b)
		}
	}
}

// A 401 invalid_token makes the client refresh ONCE, save the rotated pair
// and replay the request — body included.
func TestBearer_ExpiredAccessRefreshesOnceAndReplays(t *testing.T) {
	useHome(t)
	f := newIssuerFixture(t, time.Second)
	first, err := login(t, f, nil)
	if err != nil {
		t.Fatal(err)
	}
	hitsAfterLogin := f.tokenHits.Load()
	time.Sleep(2100 * time.Millisecond) // the access token's second-granular expiry passes

	resp, err := NewHTTPClient("", true, 5*time.Second).Post(f.srv.URL+"/protected", "text/plain", strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(b) != "host:laptop@token payload" {
		t.Fatalf("after refresh: %d %q", resp.StatusCode, b)
	}
	if n := f.tokenHits.Load() - hitsAfterLogin; n != 1 {
		t.Fatalf("token endpoint hit %d times, want exactly one refresh", n)
	}
	u, _ := url.Parse(f.srv.URL)
	now, err := LoadCredentials(u)
	if err != nil || now.RefreshToken == first.RefreshToken || now.AccessToken == first.AccessToken {
		t.Fatalf("rotated pair not saved: %+v %v", now, err)
	}
}

// A refresh that fails (the family was revoked) is tried once, and the
// caller gets the 401 — no loop, and the credentials stay for `kb login`.
func TestBearer_RefreshFailsCleanly(t *testing.T) {
	useHome(t)
	f := newIssuerFixture(t, 2*time.Hour)
	creds, err := login(t, f, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Revoke(context.Background(), creds.RefreshToken); err != nil {
		t.Fatal(err)
	}
	before := f.tokenHits.Load()
	resp, err := NewHTTPClient("", true, 5*time.Second).Get(f.srv.URL + "/protected")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d, want the 401", resp.StatusCode)
	}
	if n := f.tokenHits.Load() - before; n != 1 {
		t.Fatalf("token endpoint hit %d times, want one failed refresh", n)
	}
}

// Another kb process already rotated the pair and wrote the file: this one
// must use what is on disk, not refresh with its stale token — presenting a
// rotated refresh token would revoke the whole family for every process.
func TestBearer_UsesARefreshAnotherProcessSaved(t *testing.T) {
	useHome(t)
	f := newIssuerFixture(t, 2*time.Hour)
	creds, err := login(t, f, nil)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(f.srv.URL)
	stale := *creds
	// "Another process" rotates and saves; this process still holds `stale`.
	fresh, err := refreshWith(context.Background(), http.DefaultClient, creds)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveCredentials(u, fresh); err != nil {
		t.Fatal(err)
	}
	before := f.tokenHits.Load()
	got, err := refreshShared(context.Background(), u, stale.AccessToken)
	if err != nil || got.AccessToken != fresh.AccessToken {
		t.Fatalf("refreshShared = %+v, %v; want the pair already on disk", got, err)
	}
	if f.tokenHits.Load() != before {
		t.Fatal("refreshed again with a stale token (the family would be revoked)")
	}
	if _, err := f.store.LookupAccess(context.Background(), fresh.AccessToken); err != nil {
		t.Fatalf("the family was revoked: %v", err)
	}
}

// Only a 401 that says invalid_token is a reason to refresh: a 401 from
// anything else (a proxy's own challenge) must not rotate the pair.
func TestBearer_OnlyInvalidTokenRefreshes(t *testing.T) {
	useHome(t)
	f := newIssuerFixture(t, 2*time.Hour)
	if _, err := login(t, f, nil); err != nil {
		t.Fatal(err)
	}
	before := f.tokenHits.Load()
	resp, err := NewHTTPClient("", true, 5*time.Second).Get(f.srv.URL + "/proxy-challenge")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || f.tokenHits.Load() != before {
		t.Fatalf("status %d, token hits %d: a non-invalid_token 401 must not refresh", resp.StatusCode, f.tokenHits.Load()-before)
	}
}

// No credentials for a host: the header is not sent at all.
func TestBearer_NoCredentialsNoHeader(t *testing.T) {
	useHome(t)
	var got atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Store(r.Header.Get("Authorization"))
	}))
	defer srv.Close()
	resp, err := NewHTTPClient("", true, 5*time.Second).Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got.Load().(string) != "" {
		t.Fatalf("sent Authorization %q to a host with no credentials", got.Load())
	}
}

// The callback must carry the state kb sent and the issuer kb discovered.
func TestLogin_RefusesStateAndIssMismatch(t *testing.T) {
	for name, mutate := range map[string]func(*url.URL){
		"state": func(u *url.URL) { q := u.Query(); q.Set("state", "forged"); u.RawQuery = q.Encode() },
		"iss":   func(u *url.URL) { q := u.Query(); q.Set("iss", "https://evil.example"); u.RawQuery = q.Encode() },
		"no iss": func(u *url.URL) {
			q := u.Query()
			q.Del("iss")
			u.RawQuery = q.Encode()
		},
	} {
		t.Run(name, func(t *testing.T) {
			useHome(t)
			f := newIssuerFixture(t, 2*time.Hour)
			if _, err := login(t, f, mutate); err == nil {
				t.Fatal("login succeeded")
			}
			u, _ := url.Parse(f.srv.URL)
			if _, err := LoadCredentials(u); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("credentials written after a refused login: %v", err)
			}
		})
	}
}

// Discovery refusals: kb talks only to an issuer that advertises S256 and
// the iss parameter, whose metadata names itself, at the base URL's origin
// unless the user agrees.
func TestLogin_DiscoveryRefusals(t *testing.T) {
	serve := func(t *testing.T, prm, as func(base string) string) string {
		var base string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/.well-known/oauth-protected-resource":
				_, _ = io.WriteString(w, prm(base))
			case "/.well-known/oauth-authorization-server":
				_, _ = io.WriteString(w, as(base))
			default:
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(srv.Close)
		base = srv.URL
		return base
	}
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		b := "http://" + r.Host
		_, _ = io.WriteString(w, `{"issuer":"`+b+`","authorization_endpoint":"`+b+`/oauth/authorize","token_endpoint":"`+b+`/oauth/token","code_challenge_methods_supported":["S256"],"authorization_response_iss_parameter_supported":true}`)
	}))
	defer foreign.Close()
	goodPRM := func(b string) string { return `{"resource":"` + b + `","authorization_servers":["` + b + `"]}` }
	as := func(extra string) func(string) string {
		return func(b string) string {
			return `{"issuer":"` + b + `","authorization_endpoint":"` + b + `/oauth/authorize","token_endpoint":"` + b + `/oauth/token"` + extra + `}`
		}
	}
	for name, tc := range map[string]struct {
		prm, as func(string) string
	}{
		"no S256":      {goodPRM, as(`,"code_challenge_methods_supported":["plain"],"authorization_response_iss_parameter_supported":true`)},
		"no iss param": {goodPRM, as(`,"code_challenge_methods_supported":["S256"]`)},
		"issuer differs": {goodPRM, func(b string) string {
			return `{"issuer":"` + b + `/other","authorization_endpoint":"x","token_endpoint":"y","code_challenge_methods_supported":["S256"],"authorization_response_iss_parameter_supported":true}`
		}},
		"resource differs": {func(b string) string { return `{"resource":"` + b + `/x","authorization_servers":["` + b + `"]}` }, as(`,"code_challenge_methods_supported":["S256"],"authorization_response_iss_parameter_supported":true`)},
		// A REACHABLE issuer with valid metadata at another origin: only the
		// origin rule (and the unanswered Confirm) stops it.
		"foreign issuer": {func(b string) string {
			return `{"resource":"` + b + `","authorization_servers":["` + foreign.URL + `"]}`
		}, as(`,"code_challenge_methods_supported":["S256"],"authorization_response_iss_parameter_supported":true`)},
	} {
		t.Run(name, func(t *testing.T) {
			useHome(t)
			base := serve(t, tc.prm, tc.as)
			opened := false
			// A short Timeout: if a refusal regresses, Login would otherwise
			// wait ten minutes for a browser that never calls back.
			_, err := Login(context.Background(), base, LoginOptions{
				OpenBrowser: func(string) error { opened = true; return nil },
				Out:         io.Discard,
				Confirm:     func(string) bool { return false },
				Timeout:     2 * time.Second,
			})
			if err == nil || opened {
				t.Fatalf("err=%v opened=%v: discovery must refuse before any browser opens", err, opened)
			}
		})
	}
}

func TestLogout_RemovesCredentialsAndRevokes(t *testing.T) {
	useHome(t)
	f := newIssuerFixture(t, 2*time.Hour)
	creds, err := login(t, f, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := Logout(context.Background(), f.srv.URL); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(f.srv.URL)
	if _, err := LoadCredentials(u); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("credentials remain: %v", err)
	}
	if _, err := f.store.LookupAccess(context.Background(), creds.AccessToken); err == nil {
		t.Fatal("logout left the token usable on the server")
	}
}

func TestCredentialsPath_WindowsSafeName(t *testing.T) {
	home := useHome(t)
	for raw, want := range map[string]string{
		"https://knomit.example.com":       "knomit.example.com_443",
		"http://Localhost:19278/some/path": "localhost_19278",
		"http://[::1]:8080":                "--1_8080",
	} {
		u, _ := url.Parse(raw)
		got, err := CredentialsPath(u)
		if err != nil || got != filepath.Join(home, "credentials", want) {
			t.Errorf("%s: %q %v, want %q", raw, got, err, want)
		}
		if strings.ContainsAny(filepath.Base(got), `:<>"|?*\/`) {
			t.Errorf("%s: %q is not a valid Windows file name", raw, got)
		}
	}
}

// Several kb bridges hitting the expiry at once: the lock serialises them,
// the first refreshes, the rest find its pair on disk. Exactly one refresh,
// and the family survives. flock (and LockFileEx) conflict between two open
// files even in ONE process, so goroutines stand in for processes here.
func TestBearer_ConcurrentRefreshesAreSerialised(t *testing.T) {
	useHome(t)
	f := newIssuerFixture(t, 2*time.Hour)
	creds, err := login(t, f, nil)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(f.srv.URL)
	before := f.tokenHits.Load()
	const n = 6
	got := make([]*Credentials, n)
	errs := make([]error, n)
	done := make(chan int)
	for i := 0; i < n; i++ {
		go func(i int) {
			got[i], errs[i] = refreshShared(context.Background(), u, creds.AccessToken)
			done <- i
		}(i)
	}
	for i := 0; i < n; i++ {
		<-done
	}
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("refresh %d: %v", i, errs[i])
		}
		if got[i].AccessToken != got[0].AccessToken {
			t.Fatalf("refresh %d got a different pair", i)
		}
	}
	if hits := f.tokenHits.Load() - before; hits != 1 {
		t.Fatalf("%d refreshes, want exactly one", hits)
	}
	if _, err := f.store.LookupAccess(context.Background(), got[0].AccessToken); err != nil {
		t.Fatalf("the family did not survive: %v", err)
	}
}
