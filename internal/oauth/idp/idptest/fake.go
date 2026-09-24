// Package idptest is an in-process fake of GitHub's OAuth-app endpoints for
// tests (F19 phase 3c): authorize, token exchange, the user endpoint, token
// revocation and the public user lookup. No test anywhere talks to the
// real GitHub. It lives in its own package, like net/http/httptest, so both
// internal/oauth/idp and internal/oauth can drive it.
//
// It is strict where GitHub is and records what a test needs to assert: the
// code, verifier, client secret and redirect_uri must arrive in the POST
// BODY (never the URL), PKCE S256 is checked, and every access token handed
// out, every revocation and every request URL is kept.
package idptest

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// User is who the fake says is at the browser.
type User struct {
	ID    int64
	Login string
}

// Fake is a running fake provider. Set its exported fields before the flow
// under test runs; read the recorded ones after.
type Fake struct {
	*httptest.Server
	ClientID, ClientSecret string

	mu sync.Mutex
	// User is the account the next authorize "signs in" as.
	user User
	// Deny makes authorize answer error=access_denied (the user pressed
	// Cancel at the provider).
	deny bool
	// ExpiringTokens makes the token response carry a refresh_token even
	// though nobody asked for one, as GitHub does for apps with expiring
	// tokens.
	expiring bool
	// TokenError, when set, is returned as HTTP 200 {"error": …} by the
	// token endpoint, the way GitHub reports a bad code.
	tokenError string
	// UserBody, when set, replaces the /user response body verbatim.
	userBody string
	// RevokeStatus, when non-zero, is the revocation endpoint's answer.
	revokeStatus int

	codes   map[string]grant // code -> what authorize saw
	tokens  map[string]User  // access token -> user
	issued  []string         // every access and refresh token handed out
	revoked []string
	urls    []string // every request's full URL, in order
	users   map[string]User
	auths   int // authorize requests served
}

type grant struct {
	user                        User
	challenge, method, redirect string
}

// New starts a fake with one account, id 583231 login "octocat", and the
// given client credentials.
func New(t testing.TB, clientID, clientSecret string) *Fake {
	t.Helper()
	f := &Fake{ClientID: clientID, ClientSecret: clientSecret,
		user:  User{ID: 583231, Login: "octocat"},
		codes: map[string]grant{}, tokens: map[string]User{},
		users: map[string]User{"octocat": {ID: 583231, Login: "octocat"}},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login/oauth/authorize", f.authorize)
	mux.HandleFunc("POST /login/oauth/access_token", f.token)
	mux.HandleFunc("GET /user", f.me)
	mux.HandleFunc("DELETE /applications/{client}/token", f.revoke)
	mux.HandleFunc("GET /users/{login}", f.lookup)
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.urls = append(f.urls, r.URL.String())
		f.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *Fake) SetUser(u User)             { f.mu.Lock(); f.user = u; f.users[u.Login] = u; f.mu.Unlock() }
func (f *Fake) SetDeny(b bool)             { f.mu.Lock(); f.deny = b; f.mu.Unlock() }
func (f *Fake) SetExpiringTokens(b bool)   { f.mu.Lock(); f.expiring = b; f.mu.Unlock() }
func (f *Fake) SetTokenError(code string)  { f.mu.Lock(); f.tokenError = code; f.mu.Unlock() }
func (f *Fake) SetUserBody(body string)    { f.mu.Lock(); f.userBody = body; f.mu.Unlock() }
func (f *Fake) SetRevokeStatus(status int) { f.mu.Lock(); f.revokeStatus = status; f.mu.Unlock() }

// Issued returns every access and refresh token the fake handed out.
func (f *Fake) Issued() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.issued...)
}

// Revoked returns the access tokens revoked through the API.
func (f *Fake) Revoked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.revoked...)
}

// URLs returns every request URL the fake served, query included.
func (f *Fake) URLs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.urls...)
}

// Authorizations counts authorize requests: 0 means the browser never
// reached the provider.
func (f *Fake) Authorizations() int { f.mu.Lock(); defer f.mu.Unlock(); return f.auths }

// Codes returns the authorization codes the fake minted and not yet redeemed.
func (f *Fake) Codes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for c := range f.codes {
		out = append(out, c)
	}
	return out
}

func random() string {
	b := make([]byte, 20)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// authorize is GitHub's user-facing step with the user already signed in
// and the app already authorized: no screen, straight back.
func (f *Fake) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auths++
	redirect := q.Get("redirect_uri")
	if q.Get("client_id") != f.ClientID || redirect == "" {
		http.Error(w, "bad client or redirect", http.StatusBadRequest)
		return
	}
	u, _ := url.Parse(redirect)
	back := u.Query()
	back.Set("state", q.Get("state"))
	if f.deny {
		back.Set("error", "access_denied")
		back.Set("error_description", "The user has denied your application access.")
	} else {
		code := random()
		f.codes[code] = grant{user: f.user, challenge: q.Get("code_challenge"), method: q.Get("code_challenge_method"), redirect: redirect}
		back.Set("code", code)
	}
	u.RawQuery = back.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (f *Fake) token(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	// Secrets in the URL would end up in logs and error strings: refuse.
	if r.URL.RawQuery != "" {
		http.Error(w, "parameters must be in the body", http.StatusBadRequest)
		return
	}
	if r.Header.Get("Accept") != "application/json" {
		http.Error(w, "this fake speaks JSON only", http.StatusNotAcceptable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	fail := func(code string) {
		_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": "fake says " + code, "error_uri": "https://docs.example/" + code})
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tokenError != "" {
		fail(f.tokenError)
		return
	}
	if r.PostForm.Get("client_id") != f.ClientID || r.PostForm.Get("client_secret") != f.ClientSecret {
		fail("incorrect_client_credentials")
		return
	}
	code := r.PostForm.Get("code")
	g, ok := f.codes[code]
	if !ok {
		fail("bad_verification_code")
		return
	}
	delete(f.codes, code)
	if r.PostForm.Get("redirect_uri") != g.redirect {
		fail("redirect_uri_mismatch")
		return
	}
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if g.method != "S256" || base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
		fail("bad_verification_code")
		return
	}
	tok := "gho_" + random()
	f.tokens[tok] = g.user
	f.issued = append(f.issued, tok)
	resp := map[string]any{"access_token": tok, "token_type": "bearer", "scope": ""}
	if f.expiring {
		rt := "ghr_" + random()
		f.issued = append(f.issued, rt)
		resp["refresh_token"], resp["expires_in"], resp["refresh_token_expires_in"] = rt, 28800, 15897600
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func (f *Fake) me(w http.ResponseWriter, r *http.Request) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	u, known := f.tokens[tok]
	body := f.userBody
	f.mu.Unlock()
	if !ok || !known {
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if body != "" {
		_, _ = w.Write([]byte(body))
		return
	}
	_, _ = w.Write([]byte(`{"login":` + strconv.Quote(u.Login) + `,"id":` + strconv.FormatInt(u.ID, 10) + `,"type":"User"}`))
}

func (f *Fake) revoke(w http.ResponseWriter, r *http.Request) {
	id, secret, ok := r.BasicAuth()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.revokeStatus != 0 {
		w.WriteHeader(f.revokeStatus)
		return
	}
	if !ok || id != f.ClientID || secret != f.ClientSecret || r.PathValue("client") != f.ClientID {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var body struct {
		AccessToken string `json:"access_token"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}
	if _, known := f.tokens[body.AccessToken]; !known {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	delete(f.tokens, body.AccessToken)
	f.revoked = append(f.revoked, body.AccessToken)
	w.WriteHeader(http.StatusNoContent)
}

func (f *Fake) lookup(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	u, ok := f.users[strings.ToLower(r.PathValue("login"))]
	f.mu.Unlock()
	if !ok {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write([]byte(`{"login":` + strconv.Quote(u.Login) + `,"id":` + strconv.FormatInt(u.ID, 10) + `}`))
}
