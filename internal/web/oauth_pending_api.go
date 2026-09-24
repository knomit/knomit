package web

import (
	"encoding/json"
	"errors"
	"mime"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"knomit/internal/auth"
	"knomit/internal/oauth"
	"knomit/internal/web/hal"
)

// RequireLocalVia admits only a principal the local authenticated listener
// vouched for — a unix-socket uid, or a named-pipe SID on Windows
// (auth.LocalVia) — and refuses everyone else with 403, whatever they hold.
// The OAuth approval endpoints use it instead of Require(admin) (R5): the
// anonymous loopback principal holds admin by default, and a browser can
// reach loopback TCP but not the socket, so "admin" would let any page the
// operator visits approve a request.
func RequireLocalVia(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := auth.FromContext(r.Context())
		if p.Via != auth.LocalVia {
			hal.WriteProblem(w, http.StatusForbidden, "Permission denied",
				"this operation is only available over the local authenticated listener (the unix socket, or the named pipe on Windows); principal "+p.String()+" did not arrive over it",
				r.URL.Path)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// mountOAuthPending adds the operator's side of consent path 1 to the PLAIN
// API router (never the OAuth listener's): list, approve, deny. `knomit
// oauth pending|approve|deny` calls these over the local listener.
func (s *Server) mountOAuthPending(r chi.Router) {
	iss := s.OAuthIssuer
	r.Route("/oauth/pending", func(r chi.Router) {
		r.Use(s.requireLocalOrSameOriginAdmin)
		r.Get("/", func(w http.ResponseWriter, req *http.Request) {
			list, err := iss.Pending(req.Context())
			if err != nil {
				hal.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", err.Error(), req.URL.Path)
				return
			}
			out := make([]pendingView, 0, len(list))
			for _, p := range list {
				out = append(out, viewPending(p))
			}
			hal.WriteHAL(w, http.StatusOK, map[string]any{
				"_links":  map[string]any{"self": map[string]string{"href": APIBase + "/oauth/pending"}},
				"pending": out,
			})
		})
		r.Post("/{id}/approve", func(w http.ResponseWriter, req *http.Request) {
			var body struct {
				Subject string   `json:"subject"`
				Scopes  []string `json:"scopes"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 64<<10)).Decode(&body); err != nil {
				hal.WriteProblem(w, http.StatusBadRequest, "Bad Request", "body must be {\"subject\": ..., \"scopes\": [...]}", req.URL.Path)
				return
			}
			by, _ := auth.FromContext(req.Context())
			p, err := iss.Approve(req.Context(), chi.URLParam(req, "id"), body.Subject, body.Scopes, by.String())
			if err != nil {
				writePendingError(w, req, err)
				return
			}
			hal.WriteHAL(w, http.StatusOK, viewPending(p))
		})
		r.Post("/{id}/deny", func(w http.ResponseWriter, req *http.Request) {
			by, _ := auth.FromContext(req.Context())
			if err := iss.Deny(req.Context(), chi.URLParam(req, "id"), by.String()); err != nil {
				writePendingError(w, req, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		})
	})
}

type pendingView struct {
	Links       map[string]any `json:"_links"`
	ID          string         `json:"id"`
	ClientID    string         `json:"client_id"`
	ClientName  string         `json:"client_name"`
	RedirectURI string         `json:"redirect_uri"`
	Scopes      []string       `json:"scopes"`
	Resource    string         `json:"resource"`
	RemoteAddr  string         `json:"remote_addr"`
	UserAgent   string         `json:"user_agent"`
	CreatedAt   time.Time      `json:"created_at"`
	ExpiresAt   time.Time      `json:"expires_at"`
	Decision    string         `json:"decision,omitempty"`
	Subject     string         `json:"subject,omitempty"`
	Ceiling     []string       `json:"ceiling,omitempty"`
}

func viewPending(p oauth.Pending) pendingView {
	scopes := p.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	return pendingView{
		Links: map[string]any{
			"approve": map[string]string{"href": APIBase + "/oauth/pending/" + p.ID + "/approve"},
			"deny":    map[string]string{"href": APIBase + "/oauth/pending/" + p.ID + "/deny"},
		},
		ID: p.ID, ClientID: p.ClientID, ClientName: p.ClientName, RedirectURI: p.RedirectURI,
		Scopes: scopes, Resource: p.Resource, RemoteAddr: p.RemoteAddr, UserAgent: p.UserAgent,
		CreatedAt: p.CreatedAt.UTC(), ExpiresAt: p.ExpiresAt.UTC(),
		Decision: p.Decision, Subject: p.Subject, Ceiling: p.Ceiling,
	}
}

func writePendingError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, oauth.ErrUnknownPending):
		hal.WriteProblem(w, http.StatusNotFound, "Not Found", err.Error(), r.URL.Path)
	case errors.Is(err, oauth.ErrNotPending), errors.Is(err, oauth.ErrExpired):
		hal.WriteProblem(w, http.StatusConflict, "Conflict", err.Error(), r.URL.Path)
	case errors.Is(err, oauth.ErrInvalidScope), errors.Is(err, oauth.ErrInvalidSubject):
		hal.WriteProblem(w, http.StatusBadRequest, "Bad Request", err.Error(), r.URL.Path)
	default:
		hal.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", err.Error(), r.URL.Path)
	}
}

// KnomitClientHeader is the custom header the web UI sends on its OAuth
// approval calls (web/src/api.ts spells it identically; a test on each side
// pins the spelling). A cross-origin page cannot send it without a CORS
// preflight, and that preflight is refused: no CORS middleware on `knomit
// serve`, and on the desktop build only the Wails origins are allowlisted.
const (
	KnomitClientHeader = "X-Knomit-Client"
	KnomitClientWeb    = "web"
)

// requireLocalOrSameOriginAdmin gates the approval endpoints (F19 phase 3b,
// Task 1; rulings R2, R3 and the master's own-origin correction). It admits
//
//   - a local-listener principal (socket or pipe), exactly as RequireLocalVia
//     did in 3a, with no further proof; or
//   - the anonymous loopback principal (anonymous@none from a loopback peer,
//     never on the TLS listener) that holds admin, AND whose request carries
//     the browser proof (browserProof).
//
// Nothing else: a certificate principal holding admin by row, or a bearer
// principal, is refused whatever headers it sends — every header is
// forgeable by a non-browser client, so the proof means something only for
// the one principal a browser can be.
//
// Why admin is not enough on its own: loopback-anonymous holds admin by
// default, and any page the operator visits can reach loopback TCP. The
// proof is what a page on another origin cannot produce.
func (s *Server) requireLocalOrSameOriginAdmin(next http.Handler) http.Handler {
	g := s.grants()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := auth.FromContext(r.Context())
		if p.Via == auth.LocalVia {
			next.ServeHTTP(w, r)
			return
		}
		refuse := func(why string) {
			hal.WriteProblem(w, http.StatusForbidden, "Permission denied",
				"this operation is available over the local authenticated listener (the unix socket, or the named pipe on Windows), or from the web UI on this listener; principal "+p.String()+" was refused: "+why,
				r.URL.Path)
		}
		if p.Kind != auth.KindAnonymous || p.Via != auth.ViaNone || auth.IsTLSListener(r.Context()) || !isLoopback(r.RemoteAddr) {
			refuse("only the anonymous loopback principal of the plain listener may use the browser path")
			return
		}
		if !auth.Allowed(r.Context(), g, p, auth.Admin) {
			refuse("the anonymous loopback principal does not hold admin ([auth].loopback_default)")
			return
		}
		if why := browserProof(r); why != "" {
			refuse(why)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// browserProof returns "" when r is what the web UI's own fetch sends, else
// a reason naming the first missing proof:
//
//   - Host is exactly localhost, 127.0.0.1 or [::1] with the port the
//     connection arrived on (http.LocalAddrContextKey — the bind may be
//     0.0.0.0 or port 0, so config cannot supply it). This is the DNS
//     rebinding defence: a rebound page is same-origin with ITSELF, and its
//     Host is the attacker's name.
//   - X-Knomit-Client: web, which no cross-origin page can send without a
//     preflight that is refused.
//   - Sec-Fetch-Site, when present, is same-origin.
//   - Origin, when present, is exactly http://<Host>. It is REQUIRED on a
//     mutation (browsers omit it only on same-origin GET/HEAD).
//   - A mutation's Content-Type is application/json, which no HTML form can
//     send.
//
// The Wails origins the desktop allowlists for CORS are never "own origin":
// Chrome resolves *.localhost to loopback, so any local process on port 80
// could present them.
func browserProof(r *http.Request) string {
	host, ok := ownHost(r)
	if !ok {
		return "the Host header " + strconv.Quote(r.Host) + " is not localhost, 127.0.0.1 or [::1] with this listener's port"
	}
	if r.Header.Get(KnomitClientHeader) != KnomitClientWeb {
		return "the " + KnomitClientHeader + ": " + KnomitClientWeb + " header is missing"
	}
	if sfs := r.Header.Get("Sec-Fetch-Site"); sfs != "" && sfs != "same-origin" {
		return "Sec-Fetch-Site is " + strconv.Quote(sfs) + ", not same-origin"
	}
	origin, hasOrigin := r.Header["Origin"]
	mutation := r.Method != http.MethodGet && r.Method != http.MethodHead
	if mutation && !hasOrigin {
		return "the Origin header is missing on a " + r.Method
	}
	if hasOrigin && (len(origin) != 1 || origin[0] != "http://"+host) {
		return "the Origin " + strconv.Quote(r.Header.Get("Origin")) + " is not this listener's own origin http://" + host
	}
	if mutation {
		mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mt != "application/json" {
			return "the Content-Type " + strconv.Quote(r.Header.Get("Content-Type")) + " is not application/json"
		}
	}
	return ""
}

// ownHost returns r.Host when it names this listener by a loopback spelling
// and the port the connection arrived on. A Host without a port means 80.
func ownHost(r *http.Request) (string, bool) {
	la, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok {
		return "", false
	}
	_, lport, err := net.SplitHostPort(la.String())
	if err != nil {
		return "", false
	}
	name, port := r.Host, "80"
	if h, p, err := net.SplitHostPort(r.Host); err == nil {
		name, port = h, p
	} else if len(name) > 0 && name[0] == '[' {
		return "", false // "[::1]" with no port parses only through SplitHostPort
	}
	switch name {
	case "localhost", "127.0.0.1", "::1":
	default:
		return "", false
	}
	if port != lport {
		return "", false
	}
	return r.Host, true
}
