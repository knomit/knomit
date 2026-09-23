package web

import (
	"encoding/json"
	"errors"
	"net/http"
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
		if p.Via != auth.LocalVia || p.ID == "" {
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
		r.Use(RequireLocalVia)
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
