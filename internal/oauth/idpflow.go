package oauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"knomit/internal/config"
	"knomit/internal/oauth/idp"
)

// Consent path 3 (F19 phase 3c): a human approves a waiting request by
// proving an identity at ONE external provider whose stable id is on the
// operator's allow list. It is a SECOND OAuth flow in which knomit is the
// client; the two flows share nothing but the pending id.
//
//	/oauth/authorize          parks the request and, with a provider
//	                          configured, sets a per-request browser-binding
//	                          cookie whose hash goes on the row
//	GET  /oauth/idp/start/{id}  cookie must match → state + PKCE → provider
//	GET  /oauth/idp/callback    state single-use and cookie-bound → exchange,
//	                          one lookup, allow list → a CONSENT PAGE
//	POST /oauth/idp/decide      Origin + cookie + single-use form token →
//	                          Approve or Deny → 303 to /wait
//
// The callback never decides in the human's favour: GitHub completes its
// own step silently for a user who authorized the app before, so a link a
// victim clicks would otherwise approve an attacker's request with no
// knomit screen at all (3c review B1). What approves is the explicit POST
// from the consent page. The cookie makes a forwarded start link fail in
// every browser but the one that parked the request; it is a binding, not
// a session, and nothing reads it as identity.

// IDPOptions turns consent path 3 on.
type IDPOptions struct {
	Provider idp.Provider
	// Allowed is the parsed [oauth.idp].allowed_subjects.
	Allowed []config.AllowedSubject
}

const (
	// stateTTL bounds one trip to the provider and back; confirmTTL how
	// long a consent page may be answered. Both also end with the request.
	stateTTL   = 10 * time.Minute
	confirmTTL = 5 * time.Minute

	bindingCookiePrefix = "knomit_idp_"
	maxDecideForm       = 4 << 10
	maxLoginShown       = 64
)

// bindingCookieName is per request, so a second authorization parked in
// the same browser (a client with two knomit servers) does not overwrite
// the first's cookie.
func bindingCookieName(pendingID string) string {
	h := sha256.Sum256([]byte(pendingID))
	return bindingCookiePrefix + hex.EncodeToString(h[:8])
}

type idpFlow struct {
	provider idp.Provider
	label    string              // "GitHub", for pages
	allowed  map[string][]string // subject -> scope cap (nil: none)
	callback string              // <issuer>/oauth/idp/callback
	origin   string              // the issuer's origin, for the Origin check
	path     string              // the cookie Path the BROWSER sees
	secure   bool

	mu      sync.Mutex
	byID    map[string]*signIn // pending id -> its one live sign-in
	byState map[string]*signIn
	byToken map[string]*signIn // hashSecret(confirmation token)
}

// signIn is one trip through the provider for one request.
type signIn struct {
	pendingID, state, verifier, binding string
	stateExpires                        time.Time
	// Set by the callback once the identity is known and allowed.
	tokenHash      string
	subject        idp.Subject
	ceiling        []string
	confirmExpires time.Time
}

func newIDPFlow(issuer string, o *IDPOptions) *idpFlow {
	_, p := issuerParts(issuer)
	fl := &idpFlow{
		provider: o.Provider, label: providerLabel(o.Provider.Name()), allowed: map[string][]string{},
		callback: issuer + "/oauth/idp/callback", origin: originOf(issuer), path: p + "/oauth/idp/",
		secure: strings.HasPrefix(issuer, "https://"),
		byID:   map[string]*signIn{}, byState: map[string]*signIn{}, byToken: map[string]*signIn{},
	}
	for _, a := range o.Allowed {
		fl.allowed[a.Subject] = a.Scopes
	}
	return fl
}

func providerLabel(name string) string {
	if name == "github" {
		return "GitHub"
	}
	return name
}

// live is the number of sign-ins held (tests).
func (fl *idpFlow) live() int { fl.mu.Lock(); defer fl.mu.Unlock(); return len(fl.byID) }

// drop forgets s. byID is cleared only if it still points at s: a sign-in
// that a newer start replaced must not take its successor with it.
func (fl *idpFlow) drop(s *signIn) {
	if fl.byID[s.pendingID] == s {
		delete(fl.byID, s.pendingID)
	}
	delete(fl.byState, s.state)
	if s.tokenHash != "" {
		delete(fl.byToken, s.tokenHash)
	}
}

func (fl *idpFlow) prune(now time.Time) {
	for _, s := range fl.byID {
		if !now.Before(s.stateExpires) && (s.tokenHash == "" || !now.Before(s.confirmExpires)) {
			fl.drop(s)
		}
	}
}

// bindingCookie is set by /oauth/authorize; its hash is the row's binding.
func (fl *idpFlow) bindingCookie(pendingID, value string) *http.Cookie {
	return &http.Cookie{Name: bindingCookieName(pendingID), Value: value, Path: fl.path,
		MaxAge: int(pendingTTL / time.Second), HttpOnly: true, Secure: fl.secure, SameSite: http.SameSiteLaxMode}
}

// bound reports whether r carries the cookie that parked p.
func bound(r *http.Request, p Pending) bool {
	c, err := r.Cookie(bindingCookieName(p.ID))
	return err == nil && p.IDPBinding != "" &&
		subtle.ConstantTimeCompare([]byte(hashSecret(c.Value)), []byte(p.IDPBinding)) == 1
}

// decidedBy is the audit name of an identity-provider decision, also the
// grants' granted_by: idp:github:<numeric id>.
func decidedBy(sub idp.Subject) string {
	prov, num, _ := strings.Cut(sub.ID, "-")
	return "idp:" + prov + ":" + num
}

// refreshAllowed is the Store's refresh check (ruling W9): a family whose
// request was approved through the provider refreshes only while that id
// is still on the allow list — and not at all once [oauth.idp] is gone.
func (i *Issuer) refreshAllowed(f Family) bool {
	by, ok := strings.CutPrefix(f.ApprovedBy, "idp:")
	if !ok {
		return true
	}
	prov, num, _ := strings.Cut(by, ":")
	if i.idp == nil || prov != i.idp.provider.Name() {
		return false
	}
	_, listed := i.idp.allowed[prov+"-"+num]
	return listed
}

func shownLogin(login string) string {
	if len(login) > maxLoginShown {
		login = login[:maxLoginShown]
	}
	return login
}

// --- GET /oauth/idp/start/{id} --------------------------------------------

func (i *Issuer) idpStart(w http.ResponseWriter, r *http.Request) {
	fl, id := i.idp, r.PathValue("id")
	if !pendingIDRE.MatchString(id) {
		errorPage(w, http.StatusNotFound, "No such authorization request.")
		return
	}
	p, err := i.store.GetPending(r.Context(), id)
	if errors.Is(err, ErrUnknownPending) {
		errorPage(w, http.StatusNotFound, "No such authorization request.")
		return
	}
	if err != nil {
		errorPage(w, http.StatusInternalServerError, "The request could not be read.")
		return
	}
	if !bound(r, p) {
		errorPage(w, http.StatusForbidden, "This sign-in link works only in the browser that started the authorization. Start it again from the program that is asking.")
		return
	}
	now := i.store.now()
	if p.Decision != "" || p.Expired(now) {
		errorPage(w, http.StatusGone, "This authorization request has already been decided or has expired.")
		return
	}
	state, err1 := newSecret()
	verifier, err2 := newSecret()
	if err1 != nil || err2 != nil {
		errorPage(w, http.StatusInternalServerError, "The sign-in could not be started.")
		return
	}
	expires := now.Add(stateTTL)
	if p.ExpiresAt.Before(expires) {
		expires = p.ExpiresAt
	}
	s := &signIn{pendingID: id, state: state, verifier: verifier, binding: p.IDPBinding, stateExpires: expires}
	fl.mu.Lock()
	fl.prune(now)
	if old := fl.byID[id]; old != nil {
		fl.drop(old) // one live sign-in per request
	}
	fl.byID[id], fl.byState[state] = s, s
	fl.mu.Unlock()

	sum := sha256.Sum256([]byte(verifier))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, fl.provider.AuthorizeURL(state, base64.RawURLEncoding.EncodeToString(sum[:]), fl.callback), http.StatusFound)
}

// --- GET /oauth/idp/callback ------------------------------------------------

func (i *Issuer) idpCallback(w http.ResponseWriter, r *http.Request) {
	fl, q := i.idp, r.URL.Query()
	// An `iss` means this is an authorization response from an issuer —
	// knomit's own, misrouted to a client registered with this URL — not
	// GitHub's (which sends none). Mix-up defence: refuse.
	if _, has := q["iss"]; has {
		errorPage(w, http.StatusBadRequest, "This is not a response to a sign-in this instance started.")
		return
	}
	now := i.store.now()
	fl.mu.Lock()
	s := fl.byState[q.Get("state")]
	if s != nil {
		delete(fl.byState, s.state) // single use, whatever happens next
	}
	fl.mu.Unlock()
	bad := func() {
		errorPage(w, http.StatusBadRequest, "This sign-in is not valid in this browser or has expired. Start it again from the waiting page.")
	}
	if s == nil || !now.Before(s.stateExpires) {
		bad()
		return
	}
	p, err := i.store.GetPending(r.Context(), s.pendingID)
	if err != nil || !bound(r, p) || p.IDPBinding != s.binding {
		bad()
		return
	}
	forget := func() { fl.mu.Lock(); fl.drop(s); fl.mu.Unlock() }
	waitURL := i.issuer + "/oauth/authorize/" + url.PathEscape(p.ID) + "/wait"
	if e := q.Get("error"); e != "" {
		forget()
		if e == "access_denied" {
			renderPage(w, http.StatusOK, pageData{Message: "Sign-in at " + fl.label + " was cancelled. Nothing was decided.", Refresh: waitURL})
			return
		}
		log.Info().Str("id", p.ID).Str("error", strconv.Quote(shownLogin(e))).Msg("oauth idp: provider refused the sign-in")
		errorPage(w, http.StatusBadGateway, fl.label+" refused the sign-in. Nothing was decided.")
		return
	}
	if p.Decision != "" || p.Expired(now) {
		forget()
		errorPage(w, http.StatusGone, "This authorization request has already been decided or has expired.")
		return
	}
	sub, err := fl.provider.Identify(r.Context(), q.Get("code"), s.verifier, fl.callback)
	// A start in this browser while Identify ran replaced this sign-in (one
	// live sign-in per request). The newer one is the one that counts: this
	// callback decides nothing and touches none of the newer one's state
	// (review I1). This early check saves the work below; the binding one is
	// made again, atomically with the token registration, at the end.
	fl.mu.Lock()
	superseded := fl.byID[s.pendingID] != s
	fl.mu.Unlock()
	if superseded {
		errorPage(w, http.StatusConflict, "A newer sign-in for this request replaced this one. Nothing was decided here; finish the newer one.")
		return
	}
	if err != nil {
		forget()
		log.Info().Err(err).Str("id", p.ID).Msg("oauth idp: identity not confirmed")
		errorPage(w, http.StatusBadGateway, fl.label+" did not confirm who you are. Nothing was decided.")
		return
	}
	refuse := func(why, logMsg string) {
		forget()
		log.Info().Str("id", p.ID).Str("subject", sub.ID).Str("login", strconv.Quote(shownLogin(sub.Login))).Msg(logMsg)
		if err := i.Deny(r.Context(), p.ID, decidedBy(sub)); err != nil {
			errorPage(w, http.StatusConflict, "This authorization request can no longer be decided.")
			return
		}
		renderPage(w, http.StatusForbidden, pageData{Message: why, Refresh: waitURL})
	}
	cap, listed := fl.allowed[sub.ID]
	if !listed {
		refuse(fl.label+" account "+shownLogin(sub.Login)+" ("+sub.ID+") is not allowed to approve requests on this instance. The request was refused.",
			"oauth idp: identity not on the allow list; add it to [oauth.idp].allowed_subjects to allow it")
		return
	}
	ceiling := DefaultCeiling(p.Scopes)
	if cap != nil {
		var in []string
		for _, c := range ceiling {
			for _, k := range cap {
				if c == k {
					in = append(in, c)
				}
			}
		}
		ceiling = in
	}
	if len(ceiling) == 0 {
		refuse("What this request asks for is not permitted for this identity. The request was refused.",
			"oauth idp: request asks for nothing this identity is permitted")
		return
	}
	token, err := newSecret()
	if err != nil {
		forget()
		errorPage(w, http.StatusInternalServerError, "The sign-in could not be completed.")
		return
	}
	expires := now.Add(confirmTTL)
	if p.ExpiresAt.Before(expires) {
		expires = p.ExpiresAt
	}
	// R5: a subject granted before keeps the grants the operator left, and
	// those cap the token below this ceiling; the page must not promise more.
	prior, err := i.grants.EverGrantedAny(r.Context(), TokenPrincipal(sub.ID))
	if err != nil {
		forget()
		errorPage(w, http.StatusInternalServerError, "The sign-in could not be completed.")
		return
	}
	// The superseded check and the registration are ONE locked step, after
	// every read above: a start landing anywhere before this point replaces
	// this sign-in, and one landing after it drops the token registered here
	// (3c gate B1; the earlier check after Identify only saves the work).
	fl.mu.Lock()
	if fl.byID[s.pendingID] != s {
		fl.mu.Unlock()
		errorPage(w, http.StatusConflict, "A newer sign-in for this request replaced this one. Nothing was decided here; finish the newer one.")
		return
	}
	s.tokenHash, s.subject, s.ceiling, s.confirmExpires = hashSecret(token), sub, ceiling, expires
	fl.byToken[s.tokenHash] = s
	fl.mu.Unlock()
	i.consentPage(w, p, sub, ceiling, prior, token)
}

// --- POST /oauth/idp/decide -------------------------------------------------

func (i *Issuer) idpDecide(w http.ResponseWriter, r *http.Request) {
	fl := i.idp
	// Same-origin proof: the form is served by the issuer, so a browser
	// posting it sends Origin = the issuer's origin. Fetch metadata, when
	// sent, must agree.
	if r.Header.Get("Origin") != fl.origin || (r.Header.Get("Sec-Fetch-Site") != "" && r.Header.Get("Sec-Fetch-Site") != "same-origin") {
		errorPage(w, http.StatusForbidden, "This decision must come from the approval page on this instance.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxDecideForm)
	if err := r.ParseForm(); err != nil {
		errorPage(w, http.StatusBadRequest, "The decision could not be read.")
		return
	}
	decision := r.PostForm.Get("decision")
	if decision != "approve" && decision != "deny" {
		errorPage(w, http.StatusBadRequest, "The decision must be approve or deny.")
		return
	}
	now := i.store.now()
	fl.mu.Lock()
	s := fl.byToken[hashSecret(r.PostForm.Get("token"))]
	fl.mu.Unlock()
	if s == nil || !now.Before(s.confirmExpires) {
		errorPage(w, http.StatusBadRequest, "This approval page has expired or was already answered. Start again from the waiting page.")
		return
	}
	p, err := i.store.GetPending(r.Context(), s.pendingID)
	if err != nil || !bound(r, p) || p.IDPBinding != s.binding {
		errorPage(w, http.StatusForbidden, "This decision must come from the browser that started the authorization.")
		return
	}
	fl.mu.Lock()
	if fl.byToken[s.tokenHash] != s { // answered by a racing POST
		fl.mu.Unlock()
		errorPage(w, http.StatusBadRequest, "This approval page was already answered.")
		return
	}
	fl.drop(s)
	fl.mu.Unlock()

	by := decidedBy(s.subject)
	if decision == "approve" {
		_, err = i.Approve(r.Context(), p.ID, s.subject.ID, s.ceiling, by)
	} else {
		err = i.Deny(r.Context(), p.ID, by)
	}
	if err != nil {
		log.Info().Err(err).Str("id", p.ID).Msg("oauth idp: decision refused")
		errorPage(w, http.StatusConflict, "This authorization request can no longer be decided.")
		return
	}
	gone := fl.bindingCookie(p.ID, "")
	gone.MaxAge = -1
	http.SetCookie(w, gone)
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, i.issuer+"/oauth/authorize/"+url.PathEscape(p.ID)+"/wait", http.StatusSeeOther)
}

// --- pages ---------------------------------------------------------------------

// consentTmpl leads with where the code will go and for which client, in
// plain words, before anything else and before any button: when a victim
// opens an attacker's authorize URL themselves, the cookie holds, the
// provider says who they are, and this sentence is the only defence left.
// No script, no autofocus, no auto-submit.
var consentTmpl = template.Must(template.New("consent").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>knomit authorization</title>
<style>body{font:16px/1.5 system-ui,sans-serif;max-width:40em;margin:4em auto;padding:0 1em}code{font-size:1.05em}dt{font-weight:600;margin-top:.6em}dd{margin:0;word-break:break-all}.lead{font-size:1.15em}</style>
</head><body>
<h1>knomit</h1>
<p class="lead">Approving will send a login code to <strong>{{.Host}}</strong> for client <code>{{.ClientID}}</code>.</p>
<p>Approve only if you started this sign-in yourself, from a program you trust, and {{.Host}} is where that program listens. Whoever receives the code gets a token that acts as you on this instance.</p>
<dl>
<dt>Code goes to</dt><dd><code>{{.RedirectURI}}</code></dd>
<dt>Client (as it describes itself)</dt><dd>{{.ClientName}}</dd>
<dt>For</dt><dd><code>{{.Resource}}</code></dd>
<dt>Access it will get, at most</dt><dd><code>{{.Ceiling}}</code></dd>
</dl>
{{if .Prior}}<p>{{.Subject}} was granted before on this instance, so its tokens keep the grants the operator left, which may be less than this. The operator widens them with <code>knomit grants add</code>.</p>{{end}}
<p>You are signed in at {{.Label}} as <strong>{{.Login}}</strong> ({{.Subject}}).</p>
<form method="post" action="{{.Action}}">
<input type="hidden" name="token" value="{{.Token}}">
<button type="submit" name="decision" value="deny">Deny</button>
<button type="submit" name="decision" value="approve">Approve</button>
</form>
</body></html>
`))

type consentData struct {
	Host, ClientID, RedirectURI, ClientName, Resource, Ceiling string
	Label, Login, Subject, Action, Token                       string
	Prior                                                      bool
}

func (i *Issuer) consentPage(w http.ResponseWriter, p Pending, sub idp.Subject, ceiling []string, prior bool, token string) {
	pageHeaders(w)
	// NOT no-referrer here: under Fetch's "append a request Origin header",
	// a form POST from a no-referrer page sends Origin: null even to its own
	// origin, and the decide POST's Origin check would refuse every real
	// browser (review C1, reproduced in Chromium). strict-origin still keeps
	// this page's URL — the callback's query — from ever leaving as a
	// Referer: at most the origin is sent.
	w.Header().Set("Referrer-Policy", "strict-origin")
	w.WriteHeader(http.StatusOK)
	_ = consentTmpl.Execute(w, consentData{
		Host: redirectHost(p.RedirectURI), ClientID: p.ClientID, RedirectURI: p.RedirectURI, ClientName: p.ClientName,
		Resource: p.Resource, Ceiling: strings.Join(ceiling, " "),
		Label: i.idp.label, Login: shownLogin(sub.Login), Subject: sub.ID, Prior: prior,
		Action: i.issuer + "/oauth/idp/decide", Token: token,
	})
}

// originOf is the issuer's origin as a browser serializes it in an
// Origin header: without the scheme's default port, which an issuer may be
// written with (review M4).
func originOf(issuer string) string {
	u, err := url.Parse(issuer)
	if err != nil {
		return issuer
	}
	host := u.Host
	if (u.Scheme == "https" && u.Port() == "443") || (u.Scheme == "http" && u.Port() == "80") {
		host = strings.TrimSuffix(host, ":"+u.Port())
	}
	return u.Scheme + "://" + host
}
