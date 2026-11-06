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
	r.Get("/chats/{id}/context", m.handleContext)
	r.Get("/chats/{id}/grants", m.handleGrants)
	r.Delete("/chats/{id}/grants", m.handleRevokeGrant)
	r.Get("/chats/{id}/calls/{callId}", m.handleCall)
}

// ---- views ----

type modelView struct {
	ID        string `json:"id"`
	Name      string `json:"name"` // display name; the id when the config gives none
	Available bool   `json:"available"`
	Tools     bool   `json:"tools"`
	Reason    string `json:"reason,omitempty"`
	// ExpectedFirstTokenMS is the slowest time to a first reply this model took
	// in this server run; absent until one has been observed.
	ExpectedFirstTokenMS int64 `json:"expected_first_token_ms,omitempty"`
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
	Name     string      `json:"name"` // the model's display name, resolved now from the config
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
		"context":   {Href: base + "/context"},
		"call":      {Href: base + "/calls/{callId}", Templated: true},
		"providers": {Href: httpx.APIBase + "/chat-providers"},
	}
}

// itemOf describes a chat. The name is never stored in the chat: it is read
// from the current config each time, and a model no longer configured shows
// its id.
func itemOf(c *chat) chatItem {
	return chatItem{ID: c.id, Provider: c.provider, Model: c.model, Name: c.m.modelName(c.provider, c.model), Links: chatLinks(c.id)}
}

// modelName is the display name of provider's model id; the id when there is
// no registry or the config gives no name.
func (m *manager) modelName(provider, model string) string {
	if m == nil || m.reg == nil {
		return model
	}
	return m.reg.ModelName(provider, model)
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
				pv.Models = append(pv.Models, modelView{ID: ms.ID, Name: ms.Name, Available: ms.Available, Tools: ms.Tools, Reason: ms.Reason, ExpectedFirstTokenMS: m.expectedFirstTokenMS(p.Name, ms.ID)})
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
	Running bool `json:"running"`
	// UsageSeq is the seq of the last usage event the items include, so a page
	// that holds this snapshot ignores the live usage events it already has.
	UsageSeq int `json:"usage_seq"`
	// PromptTokens is the size of the last request's prompt (input + cache read
	// + cache write): what the context meter shows as used. 0 before a request.
	PromptTokens int64 `json:"prompt_tokens"`
	// ExpectedFirstTokenMS: see modelView.
	ExpectedFirstTokenMS int64              `json:"expected_first_token_ms,omitempty"`
	Items                []viewItem         `json:"items"`
	Pending              []*pendingApproval `json:"pending_approvals"`
	Grants               []grantInfo        `json:"grants"`
}

func (m *manager) handleGet(w http.ResponseWriter, r *http.Request) {
	c, ok := m.chatOf(w, r)
	if !ok {
		return
	}
	c.mu.Lock()
	v := chatView{chatItem: itemOf(c), Running: c.running, UsageSeq: c.usageSeq, PromptTokens: c.lastPrompt, ExpectedFirstTokenMS: m.expectedFirstTokenMS(c.provider, c.model), Items: append([]viewItem{}, c.view...), Pending: []*pendingApproval{}}
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
	// SendID names this send so a Stop that overtakes it can withdraw it (see
	// cancelRequest). Optional.
	SendID string `json:"send_id,omitempty"`
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
	turn, err := c.startTurn(req.Text, req.SendID)
	switch {
	case err == nil:
		// `turn` is the view index of the new turn: the client routes live
		// events by it. The turn is already in the view when this answers.
		hal.WriteHAL(w, http.StatusAccepted, map[string]any{"turn": turn, "_links": chatLinks(c.id)})
	case errors.Is(err, errWithdrawn):
		hal.WriteProblem(w, http.StatusConflict, "Send withdrawn", "the send was stopped before the server accepted it", r.URL.Path)
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
	// An optional body {"send_id"} names a send the page has not seen accepted
	// yet: Stop then withdraws that send as well as ending a running turn that
	// came from it. Without one, Stop ends the running turn, as it always did.
	var req cancelRequest
	if r.ContentLength != 0 {
		if !httpx.DecodeJSON(w, r, &req, 1<<12) {
			return
		}
	}
	if req.SendID != "" {
		c.cancelSend(req.SendID)
	} else {
		c.cancelTurn()
	}
	w.WriteHeader(http.StatusNoContent)
}

type cancelRequest struct {
	SendID string `json:"send_id,omitempty"`
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

// contextView is GET .../context: the chat model's context window in tokens,
// how it was resolved ("config", "memory" or "trained"; "" when the model does
// not say) and whether Ollama reported part of the model on the CPU after the
// first real request (read live, so it is false until that request is done).
type contextView struct {
	Window int    `json:"window"`
	How    string `json:"how,omitempty"`
	Spill  bool   `json:"spill"`
	// PromptTokens: see chatView.
	PromptTokens int64       `json:"prompt_tokens"`
	Links        hal.LinkMap `json:"_links"`
}

// contextOf is the model's ContextInfo, or its Caps window with no "how" when
// the model does not implement llm.ContextInfoer.
func contextOf(m llm.ChatModel) (window int, how string, spill bool) {
	if ci, ok := m.(llm.ContextInfoer); ok {
		return ci.ContextInfo()
	}
	return m.Caps().ContextWindow, "", false
}

func (m *manager) handleContext(w http.ResponseWriter, r *http.Request) {
	c, ok := m.chatOf(w, r)
	if !ok {
		return
	}
	window, how, spill := contextOf(c.loop.Model)
	c.mu.Lock()
	prompt := c.lastPrompt
	c.mu.Unlock()
	hal.WriteHAL(w, http.StatusOK, contextView{
		Window: window, How: how, Spill: spill, PromptTokens: prompt,
		Links: hal.LinkMap{"self": {Href: httpx.APIBase + "/chats/" + c.id + "/context"}},
	})
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

// handleRevokeGrant takes back one "Allow for session" grant: the body names a
// grant as GET .../grants lists it ({target, kind, tool, action}). Only that
// grant goes; the next call of that tool and action on that target asks again.
// Like every chat route it is the owner's alone (chatOf answers 404 for any
// other principal) and it sits outside the write gate, because it changes the
// chat's own approvals and no knowledge base. A grant that is not held is 404.
func (m *manager) handleRevokeGrant(w http.ResponseWriter, r *http.Request) {
	c, ok := m.chatOf(w, r)
	if !ok {
		return
	}
	var req grantInfo
	if !httpx.DecodeJSON(w, r, &req, 1<<12) {
		return
	}
	if req.Target == "" || req.Kind == "" || req.Tool == "" || req.Action == "" {
		hal.WriteProblem(w, http.StatusBadRequest, "Invalid request", "target, kind, tool and action are required", r.URL.Path)
		return
	}
	if !c.revokeGrant(req) {
		hal.WriteProblem(w, http.StatusNotFound, "Not Found", "no such grant", r.URL.Path)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
