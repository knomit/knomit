package web

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"knomit/internal/edge/oauth"
	"knomit/internal/edge/web/hal"
	"knomit/internal/edge/web/httpx"
)

// mountOAuthGrants adds the approved logins to the PLAIN API router, beside
// the pending requests and behind the same gate: list the token families that
// can still be used, and revoke one. A revoke ends the whole family, as a
// replayed refresh token does, so the client is signed out at once.
func (s *Server) mountOAuthGrants(r chi.Router) {
	iss := s.OAuthIssuer
	r.Route("/oauth/grants", func(r chi.Router) {
		r.Use(s.requireLocalOrSameOriginAdmin)
		r.Get("/", func(w http.ResponseWriter, req *http.Request) {
			list, err := iss.Logins(req.Context())
			if err != nil {
				hal.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", err.Error(), req.URL.Path)
				return
			}
			out := make([]grantView, 0, len(list))
			for _, l := range list {
				out = append(out, viewGrant(l))
			}
			hal.WriteHAL(w, http.StatusOK, map[string]any{
				"_links": map[string]any{"self": map[string]string{"href": httpx.APIBase + "/oauth/grants"}},
				"grants": out,
			})
		})
		r.Post("/{id}/revoke", func(w http.ResponseWriter, req *http.Request) {
			err := iss.RevokeLogin(req.Context(), chi.URLParam(req, "id"))
			switch {
			case errors.Is(err, oauth.ErrUnknownFamily):
				hal.WriteProblem(w, http.StatusNotFound, "Not Found", err.Error(), req.URL.Path)
			case err != nil:
				hal.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", err.Error(), req.URL.Path)
			default:
				w.WriteHeader(http.StatusNoContent)
			}
		})
	})
}

// grantView is one approved login. client_name is what the CLIENT called
// itself when it asked ("" for a login approved before it was recorded: show
// the client id). last_used_at is when the login last got a token (the code
// exchange or a refresh), null before its first.
type grantView struct {
	Links            map[string]any `json:"_links"`
	ID               string         `json:"id"`
	ClientID         string         `json:"client_id"`
	ClientName       string         `json:"client_name"`
	Subject          string         `json:"subject"`
	Scopes           []string       `json:"scopes"`
	Resource         string         `json:"resource"`
	ApprovedBy       string         `json:"approved_by"`
	CreatedAt        time.Time      `json:"created_at"`
	LastUsedAt       *time.Time     `json:"last_used_at"`
	RefreshExpiresAt time.Time      `json:"refresh_expires_at"`
}

func viewGrant(l oauth.Login) grantView {
	scopes := l.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	v := grantView{
		Links: map[string]any{
			"revoke": map[string]string{"href": httpx.APIBase + "/oauth/grants/" + l.ID + "/revoke"},
		},
		ID: l.ID, ClientID: l.ClientID, ClientName: l.ClientName, Subject: l.Subject,
		Scopes: scopes, Resource: l.Resource, ApprovedBy: l.ApprovedBy,
		CreatedAt: l.CreatedAt.UTC(), RefreshExpiresAt: l.RefreshExpiresAt.UTC(),
	}
	if !l.LastUsedAt.IsZero() {
		at := l.LastUsedAt.UTC()
		v.LastUsedAt = &at
	}
	return v
}
