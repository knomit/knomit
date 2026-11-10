package web

// The unified-auth management routes and the public login/pairing pages
// (plan P3, Task 3.5). DARK until the flip (P4):
//
//   - /api/v1/auth/* (links, tokens, sessions, bindings) sits behind
//     httpx.RequireLocalVia: only the unix socket / named pipe reaches it,
//     exactly like the fleet operations. Its registry rows say auth:manage,
//     which P4 enforces in place of RequireLocalVia.
//   - GET /auth/link/{token}, POST /auth/pair and GET /auth/pair/{id}/wait are
//     on the OUTER router and public. What they hand out is a session cookie,
//     which nothing on the main port reads before P4. A link can only be
//     minted over the socket, and before P4 nothing can approve a pairing (no
//     route or CLI verb calls Issuer.ApprovePairing), so a pairing started
//     here can never complete.
//
// None of these routes derives a URL from the request: the link URL is a
// listed external address (or the derived default), and the public pages
// answer only on a Host that matches a listed address.

import (
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/log"

	"knomit/internal/edge/oauth"
	"knomit/internal/edge/web/hal"
	"knomit/internal/edge/web/httpx"
	"knomit/internal/foundation/auth"
	"knomit/internal/foundation/authz"
	"knomit/internal/foundation/config"
)

// mountAuthAPI adds /auth to the API router, socket-only.
func (s *Server) mountAuthAPI(r chi.Router) {
	r.Route("/auth", func(r chi.Router) {
		r.Use(httpx.RequireLocalVia)
		r.Use(s.requireAuthStores)
		r.Post("/links", s.handleAuthLinkCreate)
		r.Get("/tokens", s.handleAuthTokenList)
		r.Post("/tokens", s.handleAuthTokenCreate)
		r.Delete("/tokens/{id}", s.handleAuthTokenRevoke)
		r.Get("/sessions", s.handleAuthSessionList)
		r.Delete("/sessions/{id}", s.handleAuthSessionRevoke)
		r.Get("/bindings", s.handleAuthBindingList)
		r.Post("/bindings", s.handleAuthBindingCreate)
		r.Delete("/bindings/{principal}/{role}", s.handleAuthBindingDelete)
	})
}

// mountAuthPages adds the public login and pairing pages to the outer router.
func (s *Server) mountAuthPages(r chi.Router) {
	r.Get("/auth/link/{token}", s.handleLinkPage)
	r.Post("/auth/pair", s.handlePairStart)
	r.Get("/auth/pair/{id}/wait", s.handlePairWait)
}

func (s *Server) authStoresReady() bool {
	return s.AuthStore != nil && s.AuthIssuer != nil && s.AuthzStore != nil
}

func (s *Server) requireAuthStores(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authStoresReady() {
			hal.WriteProblem(w, http.StatusServiceUnavailable, "Service Unavailable",
				"this server was built without the credential stores", r.URL.Path)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// localCaller is the authz principal of the socket/pipe caller. RequireLocalVia
// has already admitted only a local peer; the principal is read from the peer
// the kernel reported, never from anything the client sent.
func localCaller(w http.ResponseWriter, r *http.Request) (authz.Principal, bool) {
	peer, ok := auth.PeerFromContext(r.Context())
	if !ok {
		hal.WriteProblem(w, http.StatusForbidden, "Permission denied",
			"this operation is only available over the local authenticated listener", r.URL.Path)
		return authz.Principal{}, false
	}
	return authz.FromPeer(peer), true
}

// --- links ------------------------------------------------------------------

func (s *Server) handleAuthLinkCreate(w http.ResponseWriter, r *http.Request) {
	by, ok := localCaller(w, r)
	if !ok {
		return
	}
	if len(s.Issuers) == 0 {
		hal.WriteProblem(w, http.StatusServiceUnavailable, "Service Unavailable", "no external address is configured or derivable", r.URL.Path)
		return
	}
	base := s.Issuers[0]
	if want := r.URL.Query().Get("url"); want != "" {
		u, err := urlForMatch(want)
		if err != nil {
			hal.WriteProblem(w, http.StatusBadRequest, "Bad Request", "url: "+err.Error(), r.URL.Path)
			return
		}
		m, ok := config.MatchIssuer(s.Issuers, u)
		if !ok || m.Scheme != strings.ToLower(u.URL.Scheme) {
			hal.WriteProblem(w, http.StatusBadRequest, "Bad Request",
				"url "+strconv.Quote(want)+" is not one of this instance's external_addresses", r.URL.Path)
			return
		}
		base = m
	}
	tok, exp, err := s.AuthStore.MintLink(r.Context(), by)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	hal.WriteHAL(w, http.StatusCreated, map[string]any{
		"url":        base.String() + "/auth/link/" + tok,
		"expires_at": exp.UTC(),
	})
}

// urlForMatch turns a ?url= value into a request MatchIssuer can read.
func urlForMatch(raw string) (*http.Request, error) {
	if err := config.ValidateExternalAddress(strings.TrimSuffix(raw, "/")); err != nil {
		return nil, err
	}
	r, err := http.NewRequest(http.MethodGet, strings.TrimSuffix(raw, "/"), nil)
	if err != nil {
		return nil, err
	}
	return r, nil
}

// --- tokens -----------------------------------------------------------------

// parseLifetime reads "90d", "12h", "30m": a Go duration, or whole days.
func parseLifetime(s string) (time.Duration, error) {
	if d, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(d)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("%q is not a positive number of days", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%q is not a positive lifetime (use e.g. 90d, 12h or 30m)", s)
	}
	return d, nil
}

type patView struct {
	ID        string     `json:"id"`
	Label     string     `json:"label"`
	Role      string     `json:"role"`
	CreatedBy string     `json:"created_by"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	LastUsed  *time.Time `json:"last_used_at,omitempty"`
}

func viewPAT(p oauth.PAT) patView {
	v := patView{ID: p.ID, Label: p.Label, Role: string(p.Role), CreatedBy: p.CreatedBy, CreatedAt: p.Created.UTC(), ExpiresAt: p.Expires.UTC()}
	if !p.LastUsed.IsZero() {
		t := p.LastUsed.UTC()
		v.LastUsed = &t
	}
	return v
}

func (s *Server) handleAuthTokenList(w http.ResponseWriter, r *http.Request) {
	list, err := s.AuthStore.ListPATs(r.Context())
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	out := make([]patView, 0, len(list))
	for _, p := range list {
		out = append(out, viewPAT(p))
	}
	hal.WriteHAL(w, http.StatusOK, map[string]any{"tokens": out})
}

func (s *Server) handleAuthTokenCreate(w http.ResponseWriter, r *http.Request) {
	by, ok := localCaller(w, r)
	if !ok {
		return
	}
	var body httpx.AuthTokenCreateBody
	if !httpx.DecodeJSON(w, r, &body, httpx.AuthBodyMaxBytes) {
		return
	}
	role, err := authz.ParseRole(body.Role)
	if err != nil || !slices.Contains(authz.TokenRoles(), role) {
		hal.WriteProblem(w, http.StatusBadRequest, "Bad Request",
			fmt.Sprintf("role must be one of %v", authz.TokenRoles()), r.URL.Path)
		return
	}
	lt := s.AuthStore.Lifetimes()
	life := lt.PATDefault
	if body.ExpiresIn != "" {
		if life, err = parseLifetime(body.ExpiresIn); err != nil {
			hal.WriteProblem(w, http.StatusBadRequest, "Bad Request", "expires_in: "+err.Error(), r.URL.Path)
			return
		}
	}
	if life > lt.PATMax {
		hal.WriteProblem(w, http.StatusBadRequest, "Bad Request",
			fmt.Sprintf("expires_in may be at most %v ([auth].pat_max_ttl)", lt.PATMax), r.URL.Path)
		return
	}
	secret, pat, err := s.AuthStore.CreatePAT(r.Context(), role, body.Label, time.Now().Add(life), by)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	log.Info().Str("id", pat.ID).Str("role", string(role)).Str("by", by.String()).Msg("auth: personal access token created")
	v := viewPAT(pat)
	hal.WriteHAL(w, http.StatusCreated, map[string]any{
		"id": v.ID, "label": v.Label, "role": v.Role, "created_by": v.CreatedBy,
		"created_at": v.CreatedAt, "expires_at": v.ExpiresAt,
		// The secret, exactly once: it is stored only as its hash.
		"secret": secret,
	})
}

func (s *Server) handleAuthTokenRevoke(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.AuthStore.RevokePAT(r.Context(), id); err != nil {
		if errors.Is(err, oauth.ErrNotFound) {
			hal.WriteProblem(w, http.StatusNotFound, "Not Found", "no live token "+strconv.Quote(id), r.URL.Path)
			return
		}
		writeInternal(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- sessions ---------------------------------------------------------------

func (s *Server) handleAuthSessionList(w http.ResponseWriter, r *http.Request) {
	list, err := s.AuthStore.ListSessions(r.Context())
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	type view struct {
		ID         string    `json:"id"`
		Principal  string    `json:"principal"`
		UserAgent  string    `json:"user_agent"`
		CreatedAt  time.Time `json:"created_at"`
		LastSeenAt time.Time `json:"last_seen_at"`
		ExpiresAt  time.Time `json:"expires_at"`
	}
	out := make([]view, 0, len(list))
	for _, x := range list {
		out = append(out, view{x.ID, x.Principal.String(), x.UserAgent, x.Created.UTC(), x.LastSeen.UTC(), x.AbsoluteExpiry.UTC()})
	}
	hal.WriteHAL(w, http.StatusOK, map[string]any{"sessions": out})
}

func (s *Server) handleAuthSessionRevoke(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.AuthStore.RevokeSession(r.Context(), id); err != nil {
		if errors.Is(err, oauth.ErrNotFound) {
			hal.WriteProblem(w, http.StatusNotFound, "Not Found", "no live session "+strconv.Quote(id), r.URL.Path)
			return
		}
		writeInternal(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- bindings ---------------------------------------------------------------

func (s *Server) handleAuthBindingList(w http.ResponseWriter, r *http.Request) {
	list, err := s.AuthzStore.Bindings(r.Context())
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	type view struct {
		Principal string    `json:"principal"`
		Role      string    `json:"role"`
		Label     string    `json:"label"`
		By        string    `json:"by"`
		At        time.Time `json:"at"`
	}
	out := make([]view, 0, len(list))
	for _, b := range list {
		out = append(out, view{b.Principal.String(), string(b.Role), b.Label, b.By, b.At.UTC()})
	}
	hal.WriteHAL(w, http.StatusOK, map[string]any{"bindings": out})
}

// bindable parses a principal and role a person may bind or unbind by hand:
// token principals are bound when their token is minted or approved and
// freed by revoking it, never here.
func bindable(principal, role string) (authz.Principal, authz.Role, error) {
	p, err := authz.ParsePrincipal(principal)
	if err != nil {
		return authz.Principal{}, "", err
	}
	if p.Kind == authz.KindToken {
		return authz.Principal{}, "", errors.New("a token's role is set when it is minted or approved; revoke the token instead")
	}
	r, err := authz.ParseRole(role)
	if err != nil {
		return authz.Principal{}, "", err
	}
	return p, r, nil
}

func (s *Server) handleAuthBindingCreate(w http.ResponseWriter, r *http.Request) {
	by, ok := localCaller(w, r)
	if !ok {
		return
	}
	var body httpx.AuthBindingBody
	if !httpx.DecodeJSON(w, r, &body, httpx.AuthBodyMaxBytes) {
		return
	}
	p, role, err := bindable(body.Principal, body.Role)
	if err == nil {
		err = s.AuthzStore.Bind(r.Context(), authz.Binding{Principal: p, Role: role, Label: strings.TrimSpace(body.Label), By: by.String()})
	}
	if err != nil {
		// Every refusal Bind makes is about the request (owner, node,
		// anonymous beyond guest, a second token role).
		hal.WriteProblem(w, http.StatusBadRequest, "Bad Request", err.Error(), r.URL.Path)
		return
	}
	log.Info().Str("principal", p.String()).Str("role", string(role)).Str("by", by.String()).Msg("auth: role bound")
	hal.WriteHAL(w, http.StatusCreated, map[string]any{"principal": p.String(), "role": string(role)})
}

func (s *Server) handleAuthBindingDelete(w http.ResponseWriter, r *http.Request) {
	by, ok := localCaller(w, r)
	if !ok {
		return
	}
	p, role, err := bindable(chi.URLParam(r, "principal"), chi.URLParam(r, "role"))
	if err != nil {
		hal.WriteProblem(w, http.StatusBadRequest, "Bad Request", err.Error(), r.URL.Path)
		return
	}
	if err := s.AuthzStore.Unbind(r.Context(), p, role); err != nil {
		writeInternal(w, r, err)
		return
	}
	log.Info().Str("principal", p.String()).Str("role", string(role)).Str("by", by.String()).Msg("auth: role unbound")
	w.WriteHeader(http.StatusNoContent)
}

func writeInternal(w http.ResponseWriter, r *http.Request, err error) {
	log.Error().Err(err).Str("path", r.URL.Path).Msg("auth api")
	hal.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "the request could not be completed", r.URL.Path)
}

// --- public pages -------------------------------------------------------------

// pageBodyMax bounds a public page's request body: none of them reads one.
const pageBodyMax = 1 << 10

var authPageTmpl = template.Must(template.New("auth").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>knomit sign-in</title>
<style>body{font:16px/1.5 system-ui,sans-serif;max-width:40em;margin:4em auto;padding:0 1em}</style>
</head><body><h1>knomit</h1><p>{{.}}</p></body></html>
`))

// authPage writes a small HTML page with the OAuth pages' headers: no
// caching, no framing, no script, no referrer (a link token is in the URL).
func authPage(w http.ResponseWriter, status int, msg string) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'")
	h.Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	_ = authPageTmpl.Execute(w, msg)
}

const unlistedHostMsg = "This address is not one of this knomit instance's external addresses. Open the link on the address it names, or add this address to external_addresses in knomit.toml."

// handleLinkPage consumes a one-time login link and starts a session. The
// Host must match a listed external address BEFORE the link is spent: it
// sets the cookie's Secure flag, and a refused request must leave the link
// usable on the right address.
func (s *Server) handleLinkPage(w http.ResponseWriter, r *http.Request) {
	if !s.authStoresReady() {
		authPage(w, http.StatusServiceUnavailable, "Sign-in is not available on this server.")
		return
	}
	iss, ok := config.MatchIssuer(s.Issuers, r)
	if !ok {
		authPage(w, http.StatusNotFound, unlistedHostMsg)
		return
	}
	p, err := s.AuthStore.ConsumeLink(r.Context(), chi.URLParam(r, "token"))
	if err != nil {
		if !errors.Is(err, oauth.ErrInvalid) {
			log.Error().Err(err).Msg("auth: consume login link")
			authPage(w, http.StatusInternalServerError, "The sign-in could not be completed.")
			return
		}
		authPage(w, http.StatusGone, "This login link has expired or was already used. Run `knomit auth link` for a new one.")
		return
	}
	cookie, _, err := s.AuthStore.CreateSession(r.Context(), p, r.UserAgent())
	if err != nil {
		log.Error().Err(err).Msg("auth: create session")
		authPage(w, http.StatusInternalServerError, "The sign-in could not be completed.")
		return
	}
	http.SetCookie(w, newSessionCookie(iss, cookie, int(s.AuthStore.Lifetimes().SessionAbsolute/time.Second)))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	log.Info().Str("principal", p.String()).Str("remote", r.RemoteAddr).Msg("auth: signed in by login link")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// boundBody refuses a body over pageBodyMax with 413 and reports whether to
// go on. The public pages read no body; a client that sends one is bounded.
func boundBody(w http.ResponseWriter, r *http.Request) bool {
	if r.Body == nil || r.Body == http.NoBody {
		return true
	}
	buf := make([]byte, 512)
	rd := http.MaxBytesReader(w, r.Body, pageBodyMax)
	for {
		_, err := rd.Read(buf)
		if err == nil {
			continue
		}
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			hal.WriteProblem(w, http.StatusRequestEntityTooLarge, "Request Entity Too Large", "this endpoint takes no body", r.URL.Path)
			return false
		}
		return true
	}
}

func clientIP(r *http.Request) string {
	host := r.RemoteAddr
	if i := strings.LastIndexByte(host, ':'); i > 0 {
		host = host[:i]
	}
	return strings.Trim(host, "[]")
}

// handlePairStart starts a pairing for a browser with no session.
func (s *Server) handlePairStart(w http.ResponseWriter, r *http.Request) {
	if !boundBody(w, r) {
		return
	}
	if !s.authStoresReady() {
		hal.WriteProblem(w, http.StatusServiceUnavailable, "Service Unavailable", "pairing is not available on this server", r.URL.Path)
		return
	}
	if _, ok := config.MatchIssuer(s.Issuers, r); !ok {
		hal.WriteProblem(w, http.StatusNotFound, "Not Found", unlistedHostMsg, r.URL.Path)
		return
	}
	id, code, err := s.AuthIssuer.StartPairing(r.Context(), r.UserAgent(), clientIP(r))
	if errors.Is(err, oauth.ErrTooManyPending) {
		w.Header().Set("Retry-After", "60")
		hal.WriteProblem(w, http.StatusTooManyRequests, "Too Many Requests", "too many browsers are waiting to be paired; try again later", r.URL.Path)
		return
	}
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	hal.WriteHAL(w, http.StatusCreated, map[string]any{
		"id":   id,
		"code": code,
		"wait": "/auth/pair/" + id + "/wait",
	})
}

// handlePairWait long-polls a pairing. 202 while undecided (poll again), 200
// with the session cookie once approved, 403 denied, 410 expired or already
// collected, 404 unknown.
func (s *Server) handlePairWait(w http.ResponseWriter, r *http.Request) {
	if !s.authStoresReady() {
		hal.WriteProblem(w, http.StatusServiceUnavailable, "Service Unavailable", "pairing is not available on this server", r.URL.Path)
		return
	}
	iss, ok := config.MatchIssuer(s.Issuers, r)
	if !ok {
		hal.WriteProblem(w, http.StatusNotFound, "Not Found", unlistedHostMsg, r.URL.Path)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	cookie, err := s.AuthIssuer.WaitPairing(r.Context(), chi.URLParam(r, "id"))
	switch {
	case err == nil:
		http.SetCookie(w, newSessionCookie(iss, cookie, int(s.AuthStore.Lifetimes().SessionAbsolute/time.Second)))
		hal.WriteHAL(w, http.StatusOK, map[string]any{"status": "approved"})
	case errors.Is(err, oauth.ErrNotPending):
		hal.WriteHAL(w, http.StatusAccepted, map[string]any{"status": "waiting"})
	case errors.Is(err, oauth.ErrDenied):
		hal.WriteProblem(w, http.StatusForbidden, "Pairing denied", "this browser's pairing request was denied", r.URL.Path)
	case errors.Is(err, oauth.ErrExpired), errors.Is(err, oauth.ErrCollected):
		hal.WriteProblem(w, http.StatusGone, "Gone", "this pairing request has expired or was already completed; start again", r.URL.Path)
	case errors.Is(err, oauth.ErrUnknownPending):
		hal.WriteProblem(w, http.StatusNotFound, "Not Found", "no such pairing request", r.URL.Path)
	case r.Context().Err() != nil:
		// The browser went away.
	default:
		writeInternal(w, r, err)
	}
}
