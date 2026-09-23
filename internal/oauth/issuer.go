package oauth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"knomit/internal/auth"
)

var (
	ErrInvalidScope   = errors.New("oauth: invalid scope")
	ErrInvalidSubject = errors.New("oauth: subject must be 1-128 characters of letters, digits and . _ @ + - (starting with a letter or digit)")
)

// GrantWriter is the part of auth.SQLGrants an approval needs: it writes the
// grants for the approved subject so a fresh approval works without a second
// command (`knomit grants` can narrow them later).
type GrantWriter interface {
	Grant(ctx context.Context, p auth.Principal, perm auth.Permission, grantedBy string) error
}

// TokenPrincipal is the one spelling of a token subject's principal:
// host:<subject>@token. Grants are keyed by its String().
func TokenPrincipal(subject string) auth.Principal {
	return auth.Principal{Kind: auth.KindHost, ID: subject, Via: auth.ViaToken}
}

type Options struct {
	Issuer  string // normalised (config.NormalizeIssuer)
	Store   *Store
	Clients *Resolver
	Grants  GrantWriter
	// WaitTimeout is how long one /wait long-poll parks before answering
	// "still waiting" (the page then polls again). Zero means 30 s.
	WaitTimeout time.Duration
}

// Issuer is the authorization server: the public OAuth routes, and the
// operator's decisions (Approve, Deny, Pending), which the plain listener
// exposes to local principals only.
type Issuer struct {
	issuer      string
	store       *Store
	clients     *Resolver
	grants      GrantWriter
	waitTimeout time.Duration
	waiters     waiters
}

func NewIssuer(o Options) *Issuer {
	wt := o.WaitTimeout
	if wt == 0 {
		wt = 30 * time.Second
	}
	return &Issuer{issuer: o.Issuer, store: o.Store, clients: o.Clients, grants: o.Grants, waitTimeout: wt,
		waiters: waiters{m: map[string]waiter{}}}
}

// Name returns the issuer URL.
func (i *Issuer) Name() string { return i.issuer }

// Routes serves the PUBLIC surface: both discovery documents, authorize,
// wait, token and revoke. It carries no authentication by construction —
// the OAuth listener mounts it beside, never under, the bearer middleware.
//
// /.well-known/ is dispatched before the ServeMux, whose path cleaning would
// answer a non-canonical metadata path with a 301 instead of letting
// WellKnown refuse it.
func (i *Issuer) Routes() http.Handler {
	wk := WellKnown(i.issuer)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /oauth/authorize", i.authorize)
	mux.HandleFunc("GET /oauth/authorize/{id}/wait", i.wait)
	mux.HandleFunc("POST /oauth/token", i.token)
	mux.HandleFunc("POST /oauth/revoke", i.revoke)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/.well-known/") {
			wk.ServeHTTP(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// IsPublicPath reports whether path belongs to Routes, for a router that has
// to send it there rather than through its authenticated tree.
func IsPublicPath(path string) bool {
	return strings.HasPrefix(path, "/.well-known/oauth-") || strings.HasPrefix(path, "/oauth/")
}

// --- /oauth/authorize ---------------------------------------------------------

var challengeRE = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

func (i *Issuer) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	one := func(k string) (string, bool) {
		v := q[k]
		if len(v) > 1 {
			return "", false
		}
		if len(v) == 0 {
			return "", true
		}
		return v[0], true
	}

	// Until the client and its redirect URI validate, every error is a PAGE:
	// sending anything to an unvalidated redirect URI is an open redirector.
	// A repeated client_id or redirect_uri reads as "" here, and "" is
	// refused just below as an unknown client or an unregistered redirect.
	clientID, _ := one("client_id")
	redirect, _ := one("redirect_uri")
	client, err := i.clients.Resolve(r.Context(), clientID)
	if err != nil {
		log.Info().Err(err).Str("client_id", clientID).Msg("oauth: authorize refused: client")
		errorPage(w, http.StatusBadRequest, "Unknown client.")
		return
	}
	if redirect == "" || !client.RedirectAllowed(redirect) {
		errorPage(w, http.StatusBadRequest, "The redirect_uri is not registered for this client.")
		return
	}

	state, stateOK := one("state")
	fail := func(code, desc string) {
		redirectWith(w, r, redirect, i.issuer, state, url.Values{"error": {code}, "error_description": {desc}})
	}
	if !stateOK {
		state = ""
		fail("invalid_request", "state appears more than once")
		return
	}
	vals := map[string]string{}
	for _, k := range []string{"response_type", "code_challenge", "code_challenge_method", "scope", "resource"} {
		v, ok := one(k)
		if !ok {
			fail("invalid_request", k+" appears more than once")
			return
		}
		vals[k] = v
	}
	if vals["response_type"] != "code" {
		fail("unsupported_response_type", "only response_type=code is supported")
		return
	}
	if vals["code_challenge_method"] != "S256" {
		fail("invalid_request", "PKCE with code_challenge_method=S256 is required")
		return
	}
	if !challengeRE.MatchString(vals["code_challenge"]) {
		fail("invalid_request", "code_challenge must be the 43-character base64url S256 challenge")
		return
	}
	if !resourceUnder(i.issuer, vals["resource"]) {
		fail("invalid_target", "resource must be "+i.issuer+" or a URL under it")
		return
	}
	scopes := strings.Fields(vals["scope"])
	for _, s := range scopes {
		if !slices.Contains(ScopesSupported, s) {
			fail("invalid_scope", "unsupported scope "+s)
			return
		}
	}

	p, err := i.store.CreatePending(r.Context(), Pending{
		ClientID: client.ID, ClientName: client.Name, RedirectURI: redirect, Scopes: scopes,
		CodeChallenge: vals["code_challenge"], Resource: vals["resource"], State: state,
		RemoteAddr: r.RemoteAddr, UserAgent: r.UserAgent(),
	})
	if errors.Is(err, ErrTooManyPending) {
		fail("temporarily_unavailable", "too many authorization requests are waiting; try again later")
		return
	}
	if err != nil {
		log.Error().Err(err).Msg("oauth: park authorization request")
		errorPage(w, http.StatusInternalServerError, "The request could not be recorded.")
		return
	}
	log.Info().Str("id", p.ID).Str("client_id", p.ClientID).Str("remote", p.RemoteAddr).
		Msg("oauth: authorization request waiting for approval")
	i.waitingPage(w, p.ID)
}

// --- /oauth/authorize/{id}/wait -------------------------------------------------

func (i *Issuer) wait(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := r.Context()
	deadline := time.Now().Add(i.waitTimeout)
	for {
		ch := i.waiters.wait(id) // before reading, so a decision in between still wakes us
		p, err := i.store.GetPending(ctx, id)
		if errors.Is(err, ErrUnknownPending) {
			errorPage(w, http.StatusNotFound, "No such authorization request.")
			return
		}
		if err != nil {
			errorPage(w, http.StatusInternalServerError, "The request could not be read.")
			return
		}
		// A collected request has a decision too, so it breaks out here and
		// Collect answers ErrCollected: one place says "already completed".
		if p.Decision != "" {
			break
		}
		if p.Expired(i.store.now()) {
			redirectWith(w, r, p.RedirectURI, i.issuer, p.State, url.Values{"error": {"access_denied"}, "error_description": {"the request expired before it was approved"}})
			return
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			i.waitingPage(w, id)
			return
		}
		t := time.NewTimer(remaining)
		select {
		case <-ch:
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return
		}
		t.Stop()
	}

	code, p, err := i.store.Collect(ctx, id)
	switch {
	case errors.Is(err, ErrCollected):
		errorPage(w, http.StatusGone, "This authorization request has already been completed.")
	case errors.Is(err, ErrExpired):
		redirectWith(w, r, p.RedirectURI, i.issuer, p.State, url.Values{"error": {"access_denied"}, "error_description": {"the request expired before it was completed"}})
	case err != nil:
		log.Error().Err(err).Str("id", id).Msg("oauth: collect decision")
		errorPage(w, http.StatusInternalServerError, "The request could not be completed.")
	case p.Decision == DecisionApproved:
		redirectWith(w, r, p.RedirectURI, i.issuer, p.State, url.Values{"code": {code}})
	default:
		redirectWith(w, r, p.RedirectURI, i.issuer, p.State, url.Values{"error": {"access_denied"}, "error_description": {"the operator denied the request"}})
	}
}

// waiters wakes /wait long-polls when an operator decides. Entries for
// requests nobody decides are pruned once they are older than a request can
// live; closing one early only makes its waiters re-read the store.
type waiters struct {
	mu sync.Mutex
	m  map[string]waiter
}

type waiter struct {
	ch      chan struct{}
	created time.Time
}

func (w *waiters) wait(id string) <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.m) > 2*maxLivePending {
		for k, e := range w.m {
			if time.Since(e.created) > pendingTTL {
				close(e.ch)
				delete(w.m, k)
			}
		}
	}
	e, ok := w.m[id]
	if !ok {
		e = waiter{ch: make(chan struct{}), created: time.Now()}
		w.m[id] = e
	}
	return e.ch
}

func (w *waiters) notify(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if e, ok := w.m[id]; ok {
		close(e.ch)
		delete(w.m, id)
	}
}

// --- decisions (local principals only; the plain listener gates them) --------

var subjectRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@+-]{0,127}$`)

// Pending lists the requests waiting for a decision.
func (i *Issuer) Pending(ctx context.Context) ([]Pending, error) { return i.store.ListPending(ctx) }

// Approve decides a request in favour of subject. The ceiling is scopes when
// given (any supported permission name, never admin), else the requested
// scopes ∩ {read, write}, else read. It writes the grants for
// host:<subject>@token first, recorded as granted by `by`, and then the
// decision — so a decision that loses a race leaves grants the operator
// intended anyway, never a token with nothing behind it.
func (i *Issuer) Approve(ctx context.Context, id, subject string, scopes []string, by string) (Pending, error) {
	if !subjectRE.MatchString(subject) {
		return Pending{}, ErrInvalidSubject
	}
	p, err := i.store.GetPending(ctx, id)
	if err != nil {
		return Pending{}, err
	}
	if p.Decision != "" {
		return Pending{}, ErrNotPending
	}
	if p.Expired(i.store.now()) {
		return Pending{}, ErrExpired
	}
	ceiling, err := ceilingFor(p.Scopes, scopes)
	if err != nil {
		return Pending{}, err
	}
	principal := TokenPrincipal(subject)
	for _, perm := range ceiling {
		if err := i.grants.Grant(ctx, principal, auth.Permission(perm), by); err != nil {
			return Pending{}, err
		}
	}
	if err := i.store.Decide(ctx, id, DecisionApproved, subject, ceiling, by); err != nil {
		return Pending{}, err
	}
	i.waiters.notify(id)
	log.Info().Str("id", id).Str("principal", principal.String()).Strs("ceiling", ceiling).Str("by", by).
		Msg("oauth: authorization request approved")
	return i.store.GetPending(ctx, id)
}

func ceilingFor(requested, given []string) ([]string, error) {
	var want []string
	switch {
	case given != nil:
		if len(given) == 0 {
			return nil, ErrInvalidScope
		}
		for _, s := range given {
			if !slices.Contains(ScopesSupported, s) {
				return nil, ErrInvalidScope
			}
		}
		want = given
	default:
		for _, s := range requested {
			if s == "read" || s == "write" {
				want = append(want, s)
			}
		}
		if len(want) == 0 {
			want = []string{"read"}
		}
	}
	// One order, ScopesSupported's, whatever order they were given in.
	var out []string
	for _, s := range ScopesSupported {
		if slices.Contains(want, s) {
			out = append(out, s)
		}
	}
	return out, nil
}

// Deny decides a request against the client.
func (i *Issuer) Deny(ctx context.Context, id, by string) error {
	if err := i.store.Decide(ctx, id, DecisionDenied, "", nil, by); err != nil {
		return err
	}
	i.waiters.notify(id)
	log.Info().Str("id", id).Str("by", by).Msg("oauth: authorization request denied")
	return nil
}

// --- /oauth/token ---------------------------------------------------------------

const maxFormBytes = 64 << 10

var verifierRE = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)

func (i *Issuer) token(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		tokenError(w, "invalid_request", "the body must be an application/x-www-form-urlencoded form")
		return
	}
	form := r.PostForm
	for k, v := range form {
		if len(v) > 1 {
			tokenError(w, "invalid_request", k+" appears more than once")
			return
		}
	}
	need := func(keys ...string) bool {
		for _, k := range keys {
			if form.Get(k) == "" {
				tokenError(w, "invalid_request", k+" is required")
				return false
			}
		}
		return true
	}
	ctx := r.Context()
	var (
		out Issued
		err error
	)
	switch form.Get("grant_type") {
	case "authorization_code":
		if !need("code", "redirect_uri", "client_id", "code_verifier") {
			return
		}
		verifier := form.Get("code_verifier")
		if !verifierRE.MatchString(verifier) {
			tokenError(w, "invalid_request", "code_verifier must be 43-128 unreserved characters")
			return
		}
		c, cerr := i.store.ConsumeCode(ctx, form.Get("code"))
		if cerr != nil {
			log.Info().Err(cerr).Msg("oauth: code exchange refused")
			tokenError(w, "invalid_grant", "the authorization code is invalid, expired or already used")
			return
		}
		sum := sha256.Sum256([]byte(verifier))
		challenge := base64.RawURLEncoding.EncodeToString(sum[:])
		switch {
		case c.ClientID != form.Get("client_id"), c.RedirectURI != form.Get("redirect_uri"):
			tokenError(w, "invalid_grant", "the code was issued to another client or redirect_uri")
			return
		case subtle.ConstantTimeCompare([]byte(challenge), []byte(c.CodeChallenge)) != 1:
			tokenError(w, "invalid_grant", "code_verifier does not match the code_challenge")
			return
		case form.Get("resource") != "" && form.Get("resource") != c.Resource:
			tokenError(w, "invalid_target", "resource differs from the one authorized")
			return
		}
		out, err = i.store.IssuePair(ctx, c.Family.ID)
	case "refresh_token":
		if !need("refresh_token", "client_id") {
			return
		}
		out, err = i.store.Refresh(ctx, form.Get("refresh_token"), form.Get("client_id"), form.Get("resource"), strings.Fields(form.Get("scope")))
		switch {
		case errors.Is(err, ErrWrongAudience):
			tokenError(w, "invalid_target", "resource differs from the one authorized")
			return
		case errors.Is(err, ErrInvalidScope):
			tokenError(w, "invalid_scope", "a refresh cannot widen the granted scope")
			return
		case err != nil && !isStoreFailure(err):
			log.Info().Err(err).Msg("oauth: refresh refused")
			tokenError(w, "invalid_grant", "the refresh token is invalid, expired or revoked")
			return
		}
	case "":
		tokenError(w, "invalid_request", "grant_type is required")
		return
	default:
		tokenError(w, "unsupported_grant_type", "grant_type must be authorization_code or refresh_token")
		return
	}
	if err != nil {
		log.Error().Err(err).Msg("oauth: issue tokens")
		noStore(w)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error"})
		return
	}
	noStore(w)
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  out.Access,
		"token_type":    "Bearer",
		"expires_in":    int64(out.AccessExpiresAt.Sub(i.store.now()) / time.Second),
		"refresh_token": out.Refresh,
		"scope":         strings.Join(out.Family.Scopes, " "),
	})
}

// isStoreFailure tells a database failure (a 500) from a refusal (a 400).
func isStoreFailure(err error) bool {
	for _, known := range []error{ErrUnknownToken, ErrExpired, ErrRevoked, ErrReused, ErrWrongClient, ErrWrongAudience, ErrInvalidScope} {
		if errors.Is(err, known) {
			return false
		}
	}
	return true
}

func noStore(w http.ResponseWriter) {
	w.Header().Set("Pragma", "no-cache")
}

func tokenError(w http.ResponseWriter, code, desc string) {
	noStore(w)
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": code, "error_description": desc})
}

// --- /oauth/revoke (RFC 7009) -----------------------------------------------------

func (i *Issuer) revoke(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil || r.PostForm.Get("token") == "" {
		tokenError(w, "invalid_request", "token is required")
		return
	}
	if err := i.store.Revoke(r.Context(), r.PostForm.Get("token")); err != nil {
		log.Error().Err(err).Msg("oauth: revoke")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "temporarily_unavailable"})
		return
	}
	noStore(w)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
}

// --- responses ------------------------------------------------------------------

// redirectWith sends the browser to redirect (already validated) with params,
// plus state when the client sent one and iss always (RFC 9207: the client's
// mix-up defence needs it on errors too).
func redirectWith(w http.ResponseWriter, r *http.Request, redirect, issuer, state string, params url.Values) {
	u, err := url.Parse(redirect)
	if err != nil {
		errorPage(w, http.StatusBadRequest, "The redirect_uri cannot be parsed.")
		return
	}
	q := u.Query()
	for k, v := range params {
		q[k] = v
	}
	if state != "" {
		q.Set("state", state)
	}
	q.Set("iss", issuer)
	u.RawQuery = q.Encode()
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, u.String(), http.StatusFound)
}

var pageTmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>knomit authorization</title>
{{if .Refresh}}<meta http-equiv="refresh" content="1;url={{.Refresh}}">{{end}}
<style>body{font:16px/1.5 system-ui,sans-serif;max-width:40em;margin:4em auto;padding:0 1em}code{font-size:1.1em}</style>
</head><body>
<h1>knomit</h1>
<p>{{.Message}}</p>
{{if .ID}}<p>Request <code>{{.ID}}</code> is waiting for approval on the instance. The operator approves it with</p>
<pre>knomit oauth approve {{.ID}} --as &lt;name&gt;</pre>
<p>This page continues by itself once it is decided.</p>
<p><a href="{{.Refresh}}">Check now</a></p>{{end}}
</body></html>
`))

func (i *Issuer) waitingPage(w http.ResponseWriter, id string) {
	renderPage(w, http.StatusOK, pageData{
		Message: "Authorization requested.",
		ID:      id,
		Refresh: i.issuer + "/oauth/authorize/" + url.PathEscape(id) + "/wait",
	})
}

func errorPage(w http.ResponseWriter, status int, msg string) {
	renderPage(w, status, pageData{Message: msg})
}

type pageData struct{ Message, ID, Refresh string }

func renderPage(w http.ResponseWriter, status int, d pageData) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'")
	h.Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	_ = pageTmpl.Execute(w, d)
}
