package web

import (
	"net/http"
	"time"

	"knomit/internal/client/sessions"
	"knomit/internal/repos"
	"knomit/internal/web/hal"
)

type clientSessionBinding struct {
	Kind string  `json:"kind"`
	UID  string  `json:"uid"`
	Name *string `json:"name"` // null when the repo/lens no longer resolves
}

type clientSessionClient struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Initialized bool   `json:"initialized"`
}

type clientSessionBridge struct {
	Host      string `json:"host"`
	User      string `json:"user"`
	Cwd       string `json:"cwd"`
	PID       int    `json:"pid"`
	Parent    string `json:"parent"`
	ParentPID int    `json:"parent_pid"`
	Version   string `json:"version"`
}

type clientSessionView struct {
	ID           string               `json:"id"`
	InstanceID   string               `json:"instance_id"`
	State        sessions.State       `json:"state"`
	Transport    string               `json:"transport"`
	Binding      clientSessionBinding `json:"binding"`
	Branch       string               `json:"branch"`
	Client       clientSessionClient  `json:"client"`
	Bridge       clientSessionBridge  `json:"bridge"`
	RemoteAddr   string               `json:"remote_addr"`
	UserAgent    string               `json:"user_agent"`
	FirstSeenAt  string               `json:"first_seen_at"`
	LastSeenAt   string               `json:"last_seen_at"`
	EndedAt      *string              `json:"ended_at"`
	RequestCount int                  `json:"request_count"`
}

type clientSessionPolicy struct {
	DeadAfterS   int64 `json:"dead_after_s"`
	HiddenAfterS int64 `json:"hidden_after_s"`
	RetentionS   int64 `json:"retention_s"`
	LiveWindowS  int64 `json:"live_window_s"`
}

// clientSessionsCollection is the HAL collection plus the policy echo, so the
// UI never hardcodes a threshold.
type clientSessionsCollection struct {
	Count    int                            `json:"count"`
	Policy   clientSessionPolicy            `json:"policy"`
	Links    hal.LinkMap                    `json:"_links"`
	Embedded map[string][]clientSessionView `json:"_embedded"`
}

// handleHALClientSessions serves GET /api/v1/sessions — every MCP client
// session seen by this server, newest activity first. Global (not under a
// repo) because sessions cut across repos and lenses; filter with
// ?binding=repo:<uid>|lens:<uid>. Rows silent past the hidden threshold are
// omitted unless ?include=hidden.
//
// It is a read, so read-only mode changes nothing about it.
func handleHALClientSessions(b hal.URLBuilder, m *repos.Manager, store *sessions.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			hal.WriteProblem(w, http.StatusServiceUnavailable, "Client sessions unavailable",
				"the client session registry is not open on this server", r.URL.Path)
			return
		}
		now := time.Now()
		rows, err := store.List(r.Context(), sessions.Filter{
			Binding:       r.URL.Query().Get("binding"),
			IncludeHidden: r.URL.Query().Get("include") == "hidden",
			Now:           now,
		})
		if err != nil {
			hal.WriteProblem(w, http.StatusInternalServerError, "Failed to list client sessions", err.Error(), r.URL.Path)
			return
		}
		items := make([]clientSessionView, 0, len(rows))
		for _, s := range rows {
			kind, uid, name := m.ResolveBindingName(s.Binding)
			var namePtr *string
			if name != "" {
				n := name
				namePtr = &n
			}
			var ended *string
			if s.Ended != nil {
				e := s.Ended.UTC().Format(time.RFC3339)
				ended = &e
			}
			items = append(items, clientSessionView{
				ID: s.ID, InstanceID: s.InstanceID, State: s.State, Transport: s.Transport,
				Binding: clientSessionBinding{Kind: kind, UID: uid, Name: namePtr},
				Branch:  s.Branch,
				Client:  clientSessionClient{Name: s.ClientName, Version: s.ClientVersion, Initialized: s.Initialized},
				Bridge: clientSessionBridge{Host: s.Host, User: s.User, Cwd: s.Cwd, PID: s.PID,
					Parent: s.ParentApp, ParentPID: s.ParentPID, Version: s.BridgeVersion},
				RemoteAddr: s.RemoteAddr, UserAgent: s.UserAgent,
				FirstSeenAt: s.FirstSeen.UTC().Format(time.RFC3339),
				LastSeenAt:  s.LastSeen.UTC().Format(time.RFC3339),
				EndedAt:     ended, RequestCount: s.RequestCount,
			})
		}
		p := store.Policy()
		hal.WriteHAL(w, http.StatusOK, clientSessionsCollection{
			Count: len(items),
			Policy: clientSessionPolicy{
				DeadAfterS: int64(p.DeadAfter.Seconds()), HiddenAfterS: int64(p.HiddenAfter.Seconds()),
				RetentionS: int64(p.Retention.Seconds()), LiveWindowS: int64(p.LiveWindow().Seconds()),
			},
			Links:    hal.LinkMap{"self": {Href: selfWithQuery(b.Sessions(), r)}},
			Embedded: map[string][]clientSessionView{"sessions": items},
		})
	}
}
