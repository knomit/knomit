package web

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"knomit/internal/client/sessions"
	"knomit/internal/repos"
	knomitstore "knomit/internal/store"
	"knomit/internal/web/hal"
)

type clientSessionBinding struct {
	Kind string  `json:"kind"`
	UID  string  `json:"uid"`
	Name *string `json:"name"` // null when the repo/lens no longer resolves
}

// clientSessionMountRow is one URL-scoped mount this session has moved off the
// branch its URL names, by opening an experiment on it.
type clientSessionMountRow struct {
	// Mount is the pin the mount is keyed by: `repo:<uid>` or `lens:<uid>`.
	Mount string `json:"mount"`
	Kind  string `json:"kind"`
	UID   string `json:"uid"`
	// Name is null when the repo or lens no longer resolves.
	Name       *string `json:"name"`
	Experiment string  `json:"experiment"`
	// Branch is what this session actually writes to on that mount.
	Branch string `json:"branch"`
	SetAt  string `json:"set_at"`
}

// clientSessionBindingRow is one HANDLE the session has presented, inside the
// `bindings` array.
//
// One row per handle, NOT per target: two handles naming the same repo are two
// rows, because they are two callers. That is the whole reason the array exists
// — `binding` on the row above is the LAST pin seen, which stopped describing a
// session completely the moment one session id could serve several jobs at once.
type clientSessionBindingRow struct {
	// Handle is the opaque value the caller presented. Empty under ReadOnly —
	// see redactForDemo.
	Handle string  `json:"handle"`
	Kind   string  `json:"kind"`
	UID    string  `json:"uid"`
	Name   *string `json:"name"` // null when the repo/lens no longer resolves
	// Branch is the read branch the handle names; "" means the target's own.
	// Empty under ReadOnly, like the row-level branch, which it mirrors.
	Branch       string `json:"branch"`
	FirstSeenAt  string `json:"first_seen_at"`
	LastSeenAt   string `json:"last_seen_at"`
	RequestCount int    `json:"request_count"`
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
	ID         string         `json:"id"`
	InstanceID string         `json:"instance_id"`
	State      sessions.State `json:"state"`
	Transport  string         `json:"transport"`
	// Binding is the LAST pin this session was seen using. Kept as-is for
	// compatibility; Bindings is the complete picture.
	Binding clientSessionBinding `json:"binding"`
	// Bindings is every handle the session has presented, most recently used
	// first. Empty for a session that has only ever used URL-scoped mounts,
	// which present no handle.
	Bindings     []clientSessionBindingRow `json:"bindings"`
	// Mounts is what this session is writing to on its URL-SCOPED mounts, and
	// it is STORED rather than inferred from the URL. A URL-scoped mount names
	// a branch in its path, but a session that opened an experiment writes
	// somewhere else — so a table reading the path would confidently show the
	// wrong branch. Empty for a session with no experiment anywhere.
	Mounts []clientSessionMountRow `json:"mounts,omitempty"`
	Branch       string                    `json:"branch"`
	Client       clientSessionClient       `json:"client"`
	Bridge       clientSessionBridge       `json:"bridge"`
	RemoteAddr   string                    `json:"remote_addr"`
	UserAgent    string                    `json:"user_agent"`
	FirstSeenAt  string                    `json:"first_seen_at"`
	LastSeenAt   string                    `json:"last_seen_at"`
	EndedAt      *string                   `json:"ended_at"`
	RequestCount int                       `json:"request_count"`
}

type clientSessionPolicy struct {
	DeadAfterS   int64 `json:"dead_after_s"`
	HiddenAfterS int64 `json:"hidden_after_s"`
	RetentionS   int64 `json:"retention_s"`
	LiveWindowS  int64 `json:"live_window_s"`
	// Limit is the page size this response was built with, and MaxLimit the
	// largest a caller may ask for. Echoed for the same reason the thresholds
	// are: so the UI never hardcodes a number that can disagree with the
	// server.
	Limit    int `json:"limit"`
	MaxLimit int `json:"max_limit"`
}

// clientSessionsCollection is the HAL collection plus the policy echo, so the
// UI never hardcodes a threshold.
type clientSessionsCollection struct {
	Count int `json:"count"`
	// Truncated says MORE sessions matched than were returned. A caller must be
	// able to tell a truncated page from an exhausted one — a silently cut list
	// looks complete, which is worse than the unbounded read it replaced.
	Truncated bool                           `json:"truncated"`
	Policy    clientSessionPolicy            `json:"policy"`
	Links     hal.LinkMap                    `json:"_links"`
	Embedded  map[string][]clientSessionView `json:"_embedded"`
}

// handleHALClientSessions serves GET /api/v1/sessions — every MCP client
// session seen by this server, newest activity first. Global (not under a
// repo) because sessions cut across repos and lenses; filter with
// ?binding=repo:<uid>|lens:<uid>. Rows silent past the hidden threshold are
// omitted unless ?include=hidden, and the page is bounded by ?limit= (default
// sessions.DefaultListLimit, clamped to MaxListLimit). `truncated` says whether
// more matched than came back.
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
		// A malformed ?limit= is IGNORED rather than rejected: this is a
		// presence page, and the store clamps anyway, so failing the read over
		// a bad query string would cost more than it protects.
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		rows, truncated, err := store.List(r.Context(), sessions.Filter{
			Binding:       r.URL.Query().Get("binding"),
			IncludeHidden: r.URL.Query().Get("include") == "hidden",
			Now:           now,
			Limit:         limit,
		})
		if err != nil {
			hal.WriteProblem(w, http.StatusInternalServerError, "Failed to list client sessions", err.Error(), r.URL.Path)
			return
		}
		// ONE name index for the whole response — three queries, fixed —
		// rather than a lookup per row. control.db runs at
		// SetMaxOpenConns(1) with potentially hundreds of rows, so a per-row
		// lookup would serialise the entire page behind a single connection.
		//
		// The read rate is no longer just the 30s poll. Since the change
		// stream landed, an open Manage tab ALSO re-reads on a push, and the
		// binding sets that bound deliberately (see useClientSessionChanges):
		// a `touch` — the one kind that arrives at MCP request rate — is
		// trailing-only on a 5s window, while `init`/`end`/`purge` read at
		// once. So the worst case per open tab is roughly one read per 5s
		// plus one per arrival or departure, not one per MCP request.
		names := newBindingNames(m)
		// ONE query for every session's binding set, for the same reason the
		// name index is built once: control.db is SetMaxOpenConns(1), so a
		// per-row read would serialise the page behind a single connection.
		// A failure here DEGRADES — the sets are dropped and the rows still
		// render — because the last-seen binding is on the row itself and
		// losing the whole page over the richer view would be the worse trade.
		sids := make([]string, 0, len(rows))
		for _, s := range rows {
			sids = append(sids, s.ID)
		}
		sets, err := store.SessionBindings(r.Context(), sids)
		if err != nil {
			log.Warn().Err(err).Msg("client sessions: binding sets unavailable; serving rows without them")
			sets = map[string][]sessions.SessionBinding{}
		}
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
			// Most recently used first, as the store returns them.
			set := make([]clientSessionBindingRow, 0, len(sets[s.ID]))
			for _, sb := range sets[s.ID] {
				bKind, bUID, bName := names.lookup(sb.Binding)
				var bNamePtr *string
				if bName != "" {
					n := bName
					bNamePtr = &n
				}
				set = append(set, clientSessionBindingRow{
					Handle: sb.Handle, Kind: bKind, UID: bUID, Name: bNamePtr, Branch: sb.Branch,
					FirstSeenAt:  sb.FirstSeen.UTC().Format(time.RFC3339),
					LastSeenAt:   sb.LastSeen.UTC().Format(time.RFC3339),
					RequestCount: sb.RequestCount,
				})
			}
			// Attribution: what this session writes to on each URL-scoped
			// mount. Read per session rather than joined in the query, to keep
			// the store's accessor the one place that knows the table.
			var mounts []clientSessionMountRow
			if store != nil {
				if rows, merr := store.SessionMountExperiments(r.Context(), s.ID); merr == nil {
					for _, mr := range rows {
						mKind, mUID, mName := names.lookup(mr.MountUID)
						var mNamePtr *string
						if mName != "" {
							n := mName
							mNamePtr = &n
						}
						mounts = append(mounts, clientSessionMountRow{
							Mount: mr.MountUID, Kind: mKind, UID: mUID, Name: mNamePtr,
							Experiment: mr.Experiment,
							Branch:     knomitstore.ExperimentBranch(mr.Experiment),
							SetAt:      mr.SetAt.UTC().Format(time.RFC3339),
						})
					}
				} else {
					log.Warn().Err(merr).Str("mcp_session", s.ID).
						Msg("client sessions: mount experiments unreadable; the row omits them")
				}
			}

			view := clientSessionView{
				ID: s.ID, InstanceID: s.InstanceID, State: s.State, Transport: s.Transport,
				Binding:  clientSessionBinding{Kind: kind, UID: uid, Name: namePtr},
				Bindings: set,
				Mounts:   mounts,
				Branch:   s.Branch,
				Client:   clientSessionClient{Name: s.ClientName, Version: s.ClientVersion, Initialized: s.Initialized},
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
			Count:     len(items),
			Truncated: truncated,
			Policy: clientSessionPolicy{
				DeadAfterS: int64(p.DeadAfter.Seconds()), HiddenAfterS: int64(p.HiddenAfter.Seconds()),
				RetentionS: int64(p.Retention.Seconds()), LiveWindowS: int64(p.LiveWindow().Seconds()),
				Limit: sessions.ResolveListLimit(limit), MaxLimit: sessions.MaxListLimit,
			},
			Links: hal.LinkMap{
				"self": {Href: selfWithQuery(b.Sessions(), r)},
				// The change stream for this collection. A client that follows
				// it re-reads THIS url; the stream itself carries no rows.
				"events": {Href: b.Sessions() + "/events"},
			},
			Embedded: map[string][]clientSessionView{"sessions": items},
		})
	}
}

// handleHALClientSessionEvents serves GET /api/v1/sessions/events — an SSE
// stream of "row X changed" pings, so the Sessions page and the Manage badge
// stop waiting for their next 30s poll to show a session that has already
// arrived.
//
// This stream is between the BROWSER and the server. It is NOT the held
// stream that kb/decisions/mcp/client-sessions/liveness-last-seen rejects:
// that decision is about the MCP client (bridge or direct HTTP caller), which
// still holds nothing open, and presence is still derived from last_seen_at
// at read time. Nothing here makes presence exact; it makes the UI's copy of
// it prompt.
//
// The payload is an id and a kind, nothing else, so a consumer cannot render
// from it and must re-read the list — which is the endpoint that applies the
// ReadOnly redaction. That is what makes this stream safe to serve in
// read-only mode.
func handleHALClientSessionEvents(store *sessions.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			hal.WriteProblem(w, http.StatusServiceUnavailable, "Client sessions unavailable",
				"the client session registry is not open on this server", r.URL.Path)
			return
		}
		stream, ok := startSSE(w)
		if !ok {
			return
		}

		// Subscribe BEFORE announcing ready: a change published between the
		// two would otherwise fall in the gap that `ready` exists to close.
		events := store.Subscribe(r.Context())

		// One `ready` per connection. The client re-reads the list on a LATER
		// one, because a reconnect's gap may have dropped changes — the same
		// reason the branch stream replays its snapshot on connect.
		if !stream.Write("event: ready\ndata: {}\n\n") {
			return
		}

		keepalive := time.NewTicker(30 * time.Second)
		defer keepalive.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case e, ok := <-events:
				if !ok {
					return
				}
				c, ok := e.(sessions.Change)
				if !ok {
					continue
				}
				data, err := json.Marshal(c)
				if err != nil {
					continue
				}
				if !stream.Write("event: session\ndata: %s\n\n", data) {
					return
				}
			case <-keepalive.C:
				if !stream.Keepalive() {
					return
				}
			}
		}
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
	// The binding SET stays — which repos a visitor's demo instance is serving
	// is the presence story, and the pin and name are already public on the row
	// above. Two fields do not:
	//
	//   handle — a live routing capability. It is not an authorization boundary
	//   (anyone who can reach the endpoint can reach any repo by URL anyway),
	//   but publishing one caller's handle on an unauthenticated page invites
	//   another visitor to aim calls at that binding, and presence needs none
	//   of it. Redacting costs the demo nothing.
	//
	//   branch — redacted for exactly the reason the row-level branch is: an
	//   agent branch embeds the operator's hostname by construction.
	for i := range v.Bindings {
		v.Bindings[i].Handle = ""
		v.Bindings[i].Branch = ""
	}
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
