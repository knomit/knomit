package web

import (
	"net/http"
	"strings"
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
// READ-ONLY MODE REDACTS. The endpoint is a read, so read-only does not
// refuse it — but read-only is the public DEMO mode, and these rows carry the
// operator's hostname, username, working directory, pids, agent branch and
// IP. Presence stays visible to a visitor; the operator's machine does not.
// See redactForDemo.
func handleHALClientSessions(b hal.URLBuilder, m *repos.Manager, store *sessions.Store, readOnly bool) http.HandlerFunc {
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
		// ONE name index for the whole response — three queries, fixed —
		// rather than a lookup per row. control.db runs at
		// SetMaxOpenConns(1) and the UI polls this endpoint every 30s with
		// potentially hundreds of rows, so a per-row lookup would serialise
		// the entire page behind a single connection.
		names := newBindingNames(m)
		items := make([]clientSessionView, 0, len(rows))
		for _, s := range rows {
			kind, uid, name := names.lookup(s.Binding)
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
			view := clientSessionView{
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
			}
			if readOnly {
				redactForDemo(&view)
			}
			items = append(items, view)
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

// redactForDemo strips everything that describes the OPERATOR's machine,
// leaving what presence is actually for: which clients are connected, to
// what, how recently, and how busy.
//
// Kept: id, instance_id (a hash, and the handle the UI groups by), state,
// transport, binding, the client's declared name/version, the timestamps and
// the request count. Removed: every bridge field (host, user, cwd, pid,
// parent, parent_pid, version), the agent branch — which embeds the hostname
// by construction — the remote address and the User-Agent, which carries the
// bridge version.
func redactForDemo(v *clientSessionView) {
	v.Bridge = clientSessionBridge{}
	v.Branch = ""
	v.RemoteAddr = ""
	v.UserAgent = ""
}

// bindingNames maps a binding uid to its current display name, for both
// kinds, built once per response by newBindingNames.
type bindingNames struct {
	repos  map[string]string
	lenses map[string]string
}

// newBindingNames snapshots every name a binding could resolve to. Repos come
// from the live instances first and the registry second, so a repo that is
// registered but not open — archived, or failed to open — still resolves;
// lenses come from the lens registry. A uid missing from both is not an
// error: the row outlives its repo or lens by design, and is rendered with
// its uid and a null name.
func newBindingNames(m *repos.Manager) bindingNames {
	idx := bindingNames{repos: map[string]string{}, lenses: map[string]string{}}
	if m == nil {
		return idx
	}
	m.ForEach(func(name string, ri *repos.RepoInstance) {
		if ri != nil && ri.UID() != "" {
			idx.repos[ri.UID()] = name
		}
	})
	if reg := m.Repos(); reg != nil {
		for _, state := range []repos.RepoState{repos.StateActive, repos.StateArchived} {
			recs, err := reg.List(state)
			if err != nil {
				continue // a name we cannot read renders as a uid, not as a failure
			}
			for _, rec := range recs {
				if _, ok := idx.repos[rec.UID]; !ok {
					idx.repos[rec.UID] = rec.Name
				}
			}
		}
	}
	if lr := m.LensRegistry(); lr != nil {
		if ls, err := lr.List(); err == nil {
			for _, l := range ls {
				idx.lenses[l.UID] = l.Name
			}
		}
	}
	return idx
}

// lookup splits a PinID ("repo:<uid>" | "lens:<uid>") and resolves the name.
// kind is "" for a value that is not a PinID.
func (b bindingNames) lookup(pin string) (kind, uid, name string) {
	switch {
	case strings.HasPrefix(pin, "repo:"):
		uid = strings.TrimPrefix(pin, "repo:")
		return "repo", uid, b.repos[uid]
	case strings.HasPrefix(pin, "lens:"):
		uid = strings.TrimPrefix(pin, "lens:")
		return "lens", uid, b.lenses[uid]
	}
	return "", "", ""
}
