package chat

import (
	"errors"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/log"

	"knomit/internal/edge/web/hal"
	"knomit/internal/edge/web/httpx"
	"knomit/internal/engine/llm"
	"knomit/internal/foundation/auth"
)

// eventsKeepalive is how often an idle events stream gets a comment frame
// (plan P10).
const eventsKeepalive = 30 * time.Second

func (m *manager) routes(r chi.Router) {
	r.Get("/chat-providers", m.handleProviders)
	r.Get("/chats", m.handleList)
	r.Post("/chats", m.handleCreate)
	r.Get("/chats/{id}", m.handleGet)
	r.Delete("/chats/{id}", m.handleDelete)
	r.Get("/chats/{id}/events", m.handleEvents)
	r.Post("/chats/{id}/turns", m.handleTurn)
	r.Post("/chats/{id}/cancel", m.handleCancel)
	r.Post("/chats/{id}/approvals/{callId}", m.handleApproval)
	r.Get("/chats/{id}/grants", m.handleGrants)
	r.Get("/chats/{id}/calls/{callId}", m.handleCall)
}

// ---- views ----

type modelView struct {
	ID        string `json:"id"`
	Available bool   `json:"available"`
	Tools     bool   `json:"tools"`
	Reason    string `json:"reason,omitempty"`
}

type providerView struct {
	Name   string      `json:"name"`
	Kind   string      `json:"kind"`
	Models []modelView `json:"models"`
}

type chatItem struct {
	ID       string      `json:"id"`
	Provider string      `json:"provider"`
	Model    string      `json:"model"`
	Links    hal.LinkMap `json:"_links"`
}

func chatLinks(id string) hal.LinkMap {
	base := httpx.APIBase + "/chats/" + id
	return hal.LinkMap{
		"self":      {Href: base},
		"events":    {Href: base + "/events"},
		"turns":     {Href: base + "/turns"},
		"cancel":    {Href: base + "/cancel"},
		"approval":  {Href: base + "/approvals/{callId}", Templated: true},
		"grants":    {Href: base + "/grants"},
		"call":      {Href: base + "/calls/{callId}", Templated: true},
		"providers": {Href: httpx.APIBase + "/chat-providers"},
	}
}

func itemOf(c *chat) chatItem {
	return chatItem{ID: c.id, Provider: c.provider, Model: c.model, Links: chatLinks(c.id)}
}

// ---- helpers ----

// caller returns the signed-in principal's key, or writes a 401.
func caller(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	p, ok := auth.FromContext(r.Context())
	if !ok || p.IsZero() {
		hal.WriteProblem(w, http.StatusUnauthorized, "Unauthorized", "chat needs a signed-in principal", r.URL.Path)
		return auth.Principal{}, false
	}
	return p, true
}

// chatOf resolves {id} for the caller; any other principal's chat is a 404.
func (m *manager) chatOf(w http.ResponseWriter, r *http.Request) (*chat, bool) {
	p, ok := caller(w, r)
	if !ok {
		return nil, false
	}
	c := m.find(chi.URLParam(r, "id"), p.String())
	if c == nil {
		hal.WriteProblem(w, http.StatusNotFound, "Not Found", "no such chat", r.URL.Path)
		return nil, false
	}
	return c, true
}

// ---- handlers ----

func (m *manager) handleProviders(w http.ResponseWriter, r *http.Request) {
	if _, ok := caller(w, r); !ok {
		return
	}
	var out []providerView
	if m.reg != nil {
		for _, p := range m.reg.List(r.Context()) {
			pv := providerView{Name: p.Name, Kind: p.Kind, Models: []modelView{}}
			for _, ms := range p.Models {
				pv.Models = append(pv.Models, modelView(ms))
			}
			out = append(out, pv)
		}
	}
	if out == nil {
		out = []providerView{}
	}
	hal.WriteHAL(w, http.StatusOK, hal.CollectionView[providerView]{
		Count:    len(out),
		Links:    hal.LinkMap{"self": {Href: httpx.APIBase + "/chat-providers"}, "chats": {Href: httpx.APIBase + "/chats"}},
		Embedded: map[string][]providerView{"providers": out},
	})
}

func (m *manager) handleList(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	items := []chatItem{}
	for _, c := range m.listFor(p.String()) {
		items = append(items, itemOf(c))
	}
	sortChatItems(items)
	hal.WriteHAL(w, http.StatusOK, hal.CollectionView[chatItem]{
		Count:    len(items),
		Links:    hal.LinkMap{"self": {Href: httpx.APIBase + "/chats"}},
		Embedded: map[string][]chatItem{"chats": items},
	})
}

func sortChatItems(items []chatItem) {
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
}

type createRequest struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

func (m *manager) handleCreate(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	var req createRequest
	if !httpx.DecodeJSON(w, r, &req, 1<<16) {
		return
	}
	if req.Provider == "" || req.Model == "" {
		hal.WriteProblem(w, http.StatusBadRequest, "Invalid request", "provider and model are required", r.URL.Path)
		return
	}
	var reach callerReach
	if set, ok := auth.CeilingFromContext(r.Context()); ok {
		reach.ceiling = &set
	}
	if f, ok := auth.RepoReachFromContext(r.Context()); ok {
		reach.repo = f
	}
	if f, ok := auth.LensReachFromContext(r.Context()); ok {
		reach.lens = f
	}
	c, err := m.create(r.Context(), p, req.Provider, req.Model, reach)
	switch {
	case err == nil:
	case errors.Is(err, llm.ErrUnknownModel):
		hal.WriteProblem(w, http.StatusBadRequest, "Unknown provider or model", err.Error(), r.URL.Path)
		return
	case errors.Is(err, errClosed):
		hal.WriteProblem(w, http.StatusServiceUnavailable, "Chat unavailable", "knomit is shutting down", r.URL.Path)
		return
	default:
		// err never carries a key: the registry names providers, not secrets.
		log.Warn().Err(err).Str("provider", req.Provider).Str("model", req.Model).Msg("chat: could not start a chat")
		hal.WriteProblem(w, http.StatusUnprocessableEntity, "Cannot start the chat", err.Error(), r.URL.Path)
		return
	}
	hal.WriteHALCreated(w, httpx.APIBase+"/chats/"+c.id, itemOf(c))
}

type chatView struct {
	chatItem
	Running bool               `json:"running"`
	Items   []viewItem         `json:"items"`
	Pending []*pendingApproval `json:"pending_approvals"`
	Grants  []grantInfo        `json:"grants"`
}

func (m *manager) handleGet(w http.ResponseWriter, r *http.Request) {
	c, ok := m.chatOf(w, r)
	if !ok {
		return
	}
	c.mu.Lock()
	v := chatView{chatItem: itemOf(c), Running: c.running, Items: append([]viewItem{}, c.view...), Pending: []*pendingApproval{}}
	for _, p := range c.pending {
		v.Pending = append(v.Pending, p)
	}
	c.mu.Unlock()
	v.Grants = c.grantList()
	hal.WriteHAL(w, http.StatusOK, v)
}

func (m *manager) handleDelete(w http.ResponseWriter, r *http.Request) {
	c, ok := m.chatOf(w, r)
	if !ok {
		return
	}
	c.end("deleted")
	w.WriteHeader(http.StatusNoContent)
}

func (m *manager) handleEvents(w http.ResponseWriter, r *http.Request) {
	c, ok := m.chatOf(w, r)
	if !ok {
		return
	}
	sub, ok := c.attach()
	if !ok {
		hal.WriteProblem(w, http.StatusNotFound, "Not Found", "no such chat", r.URL.Path)
		return
	}
	defer c.detach(sub)
	stream, ok := httpx.StartSSE(w)
	if !ok {
		return
	}
	// Every connect starts with ready; the client then refetches the chat.
	// There is no id: and no Last-Event-ID replay (plan P10).
	if !stream.Write("event: ready\ndata: {}\n\n") {
		return
	}
	keepalive := time.NewTicker(eventsKeepalive)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			if !stream.Keepalive() {
				return
			}
		case frame, open := <-sub.ch:
			if !open {
				return
			}
			if !stream.Write("%s", frame) {
				return
			}
		}
	}
}

type turnRequest struct {
	Text string `json:"text"`
}

func (m *manager) handleTurn(w http.ResponseWriter, r *http.Request) {
	c, ok := m.chatOf(w, r)
	if !ok {
		return
	}
	var req turnRequest
	if !httpx.DecodeJSON(w, r, &req, 1<<20) {
		return
	}
	if req.Text == "" {
		hal.WriteProblem(w, http.StatusBadRequest, "Invalid request", "text is required", r.URL.Path)
		return
	}
	switch err := c.startTurn(req.Text); {
	case err == nil:
		hal.WriteHAL(w, http.StatusAccepted, map[string]any{"_links": chatLinks(c.id)})
	case errors.Is(err, errBusy):
		hal.WriteProblem(w, http.StatusConflict, "A turn is running", "wait for the running turn to finish, or cancel it", r.URL.Path)
	default:
		hal.WriteProblem(w, http.StatusNotFound, "Not Found", "no such chat", r.URL.Path)
	}
}

func (m *manager) handleCancel(w http.ResponseWriter, r *http.Request) {
	c, ok := m.chatOf(w, r)
	if !ok {
		return
	}
	c.cancelTurn()
	w.WriteHeader(http.StatusNoContent)
}

type approvalRequest struct {
	Decision string `json:"decision"`
}

func (m *manager) handleApproval(w http.ResponseWriter, r *http.Request) {
	c, ok := m.chatOf(w, r)
	if !ok {
		return
	}
	var req approvalRequest
	if !httpx.DecodeJSON(w, r, &req, 1<<12) {
		return
	}
	switch req.Decision {
	case "allow", "decline", "allow_session":
	default:
		hal.WriteProblem(w, http.StatusBadRequest, "Invalid request", `decision must be "allow", "decline" or "allow_session"`, r.URL.Path)
		return
	}
	found, err := c.answerApproval(chi.URLParam(r, "callId"), req.Decision)
	switch {
	case !found:
		hal.WriteProblem(w, http.StatusNotFound, "Not Found", "no approval is pending for that call", r.URL.Path)
	case err != nil:
		hal.WriteProblem(w, http.StatusBadRequest, "Cannot allow for the session", err.Error(), r.URL.Path)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (m *manager) handleGrants(w http.ResponseWriter, r *http.Request) {
	c, ok := m.chatOf(w, r)
	if !ok {
		return
	}
	gs := c.grantList()
	hal.WriteHAL(w, http.StatusOK, hal.CollectionView[grantInfo]{
		Count:    len(gs),
		Links:    hal.LinkMap{"self": {Href: httpx.APIBase + "/chats/" + c.id + "/grants"}},
		Embedded: map[string][]grantInfo{"grants": gs},
	})
}

func (m *manager) handleCall(w http.ResponseWriter, r *http.Request) {
	c, ok := m.chatOf(w, r)
	if !ok {
		return
	}
	rec, err := c.store.getCall(chi.URLParam(r, "callId"))
	if errors.Is(err, os.ErrNotExist) {
		hal.WriteProblem(w, http.StatusNotFound, "Not Found", "no such call", r.URL.Path)
		return
	}
	if err != nil {
		hal.WriteProblem(w, http.StatusInternalServerError, "Unreadable call", err.Error(), r.URL.Path)
		return
	}
	hal.WriteHAL(w, http.StatusOK, rec)
}
