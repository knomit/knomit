// Package chat serves the web chat: provider and model listing, chat sessions
// that talk to the knomit tools through an in-process MCP session, their
// approvals and grants, an SSE event stream, and the temp and persistent
// session records. Its only export is Mount.
package chat

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/rs/zerolog/log"

	"knomit/internal/corpus/repos"
	"knomit/internal/edge/mcp"
	"knomit/internal/edge/web/httpx"
	"knomit/internal/engine/llm"
	"knomit/internal/foundation/auth"
)

// Mount registers the chat routes on r, which the caller mounts at the API
// root (APIBase). srv is the MCP server every chat's in-process session talks
// to: permissions, the binding gate and validation are the ordinary ones, and
// chat never calls a handler or a corpus API directly. rm is the repo manager
// the session registers in, and reg opens the configured providers' models.
//
// sessionsDir is where the chats' temp files live: each process keeps one run
// directory there, guarded by an OS lock, and Mount sweeps the runs of dead
// owners before serving ("" uses a private directory under os.TempDir()).
// logDir is where the persistent session logs go (one JSONL per chat, never
// removed here); "" turns the log off.
//
// The chats live as long as ctx: when it ends, every chat is ended, its temp
// files and run directory removed. A non-nil released is how the caller waits
// for that: Mount adds one to it and marks it Done once everything is let go.
func Mount(ctx context.Context, r chi.Router, srv *mcpserver.MCPServer, rm *repos.Manager, reg *llm.Registry, sessionsDir, logDir string, released *sync.WaitGroup) {
	m := newManager(ctx, srv, rm, reg, sessionsDir, logDir, released)
	m.routes(r)
}

// manager owns the live chats.
type manager struct {
	life context.Context
	srv  *mcpserver.MCPServer
	rm   *repos.Manager
	reg  *llm.Registry

	base      string // the sessions directory; "" until a private one is made
	baseOwned bool
	logDir    string

	// openModel and price are the registry seams; tests replace openModel
	// with a scripted model (after the real registry has judged the request).
	openModel func(ctx context.Context, provider, model string) (llm.ChatModel, error)
	price     func(provider, model string) *llm.PriceCard

	mu     sync.Mutex
	chats  map[string]*chat
	closed bool

	// pageMu guards pages: the page events streams attached per owner. It is
	// taken after a chat's mu, never before.
	pageMu sync.Mutex
	pages  map[string]map[*pageSub]struct{}

	runMu   sync.Mutex
	run     string
	release func()

	// firstToken is the slowest time to a model's first sign of life seen by
	// this server run, per provider and model.
	ftMu       sync.Mutex
	firstToken map[string]time.Duration
}

// noteFirstToken keeps d when it is the slowest first-token time seen for the model.
func (m *manager) noteFirstToken(provider, model string, d time.Duration) {
	m.ftMu.Lock()
	defer m.ftMu.Unlock()
	if m.firstToken == nil {
		m.firstToken = map[string]time.Duration{}
	}
	k := provider + "\x00" + model
	if d > m.firstToken[k] {
		m.firstToken[k] = d
	}
}

// expectedFirstTokenMS is the slowest first-token time observed for the model
// in this server run, in milliseconds; 0 when none has been observed.
func (m *manager) expectedFirstTokenMS(provider, model string) int64 {
	m.ftMu.Lock()
	defer m.ftMu.Unlock()
	d := m.firstToken[provider+"\x00"+model]
	if d > 0 && d < time.Millisecond {
		return 1
	}
	return d.Milliseconds()
}

func newManager(ctx context.Context, srv *mcpserver.MCPServer, rm *repos.Manager, reg *llm.Registry, sessionsDir, logDir string, released *sync.WaitGroup) *manager {
	m := &manager{
		life: ctx, srv: srv, rm: rm, reg: reg, base: sessionsDir, logDir: logDir,
		chats: map[string]*chat{},
	}
	if reg != nil {
		m.openModel = reg.Open
		m.price = reg.Price
	}
	if sessionsDir != "" {
		// The first router build removes the runs of processes that have exited.
		if n, err := httpx.SweepDeadRuns(sessionsDir); err != nil {
			log.Warn().Err(err).Msg("chat: cannot list previous runs")
		} else if n > 0 {
			log.Info().Int("runs", n).Msg("chat: swept the temp folders of dead runs")
		}
	}
	if released != nil {
		released.Add(1)
	}
	context.AfterFunc(ctx, func() {
		m.shutdown()
		if released != nil {
			released.Done()
		}
	})
	return m
}

var errClosed = errors.New("chats have shut down")

// runDir claims the process's run directory on first use.
func (m *manager) runDir() (string, error) {
	m.runMu.Lock()
	defer m.runMu.Unlock()
	if m.run != "" {
		return m.run, nil
	}
	if m.life.Err() != nil {
		return "", errClosed
	}
	if m.base == "" {
		b, err := os.MkdirTemp("", "knomit-chat-sessions-")
		if err != nil {
			return "", fmt.Errorf("create chat temp dir: %w", err)
		}
		m.base, m.baseOwned = b, true
	}
	dir, release, err := httpx.AcquireRunDir(m.life, m.base)
	if err != nil {
		return "", err
	}
	m.run, m.release = dir, release
	return dir, nil
}

// shutdown ends every chat, then lets go of the run directory.
func (m *manager) shutdown() {
	m.mu.Lock()
	m.closed = true
	all := make([]*chat, 0, len(m.chats))
	for _, c := range m.chats {
		all = append(all, c)
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, c := range all {
		wg.Add(1)
		go func() { defer wg.Done(); c.end("shutdown") }()
	}
	wg.Wait()

	m.runMu.Lock()
	release, base, owned := m.release, m.base, m.baseOwned
	m.run, m.release = "", nil
	m.runMu.Unlock()
	if release != nil {
		release()
	}
	if owned && strings.HasPrefix(baseName(base), "knomit-chat-sessions-") {
		_ = os.RemoveAll(base)
	}
}

func baseName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

func (m *manager) forget(id string) {
	m.mu.Lock()
	delete(m.chats, id)
	m.mu.Unlock()
}

// pageCount is how many page streams owner has attached.
func (m *manager) pageCount(owner string) int {
	m.pageMu.Lock()
	defer m.pageMu.Unlock()
	return len(m.pages[owner])
}

// broadcast offers frame to every page stream of owner and reports whether any
// took it. A stream whose buffer is full is dropped (closed); its handler then
// returns and detaches, and the client reconnects.
func (m *manager) broadcast(owner string, frame []byte) bool {
	m.pageMu.Lock()
	defer m.pageMu.Unlock()
	delivered := false
	for s := range m.pages[owner] {
		select {
		case s.ch <- frame:
			delivered = true
		default:
			delete(m.pages[owner], s)
			close(s.ch)
		}
	}
	return delivered
}

// deliverTo offers frame to s, only while s is still attached: broadcast
// closes the channel of a stream it drops, and sending on it would panic. It
// reports whether s took the frame. Called with a chat's mu held (lock order:
// chat.mu, then pageMu).
func (m *manager) deliverTo(s *pageSub, frame []byte) bool {
	m.pageMu.Lock()
	defer m.pageMu.Unlock()
	if _, ok := m.pages[s.owner][s]; !ok {
		return false
	}
	select {
	case s.ch <- frame:
		return true
	default:
		return false
	}
}

// attachPage registers owner's page stream. Every chat of the owner stops its
// grace clock, and takes the terminal event no stream received into the stream.
func (m *manager) attachPage(owner string) *pageSub {
	s := &pageSub{owner: owner, ch: make(chan []byte, 1024)}
	m.pageMu.Lock()
	if m.pages == nil {
		m.pages = map[string]map[*pageSub]struct{}{}
	}
	if m.pages[owner] == nil {
		m.pages[owner] = map[*pageSub]struct{}{}
	}
	m.pages[owner][s] = struct{}{}
	m.pageMu.Unlock()
	for _, c := range m.listFor(owner) {
		c.adoptPage(s)
	}
	return s
}

// detachPage removes a page stream; the last one out of an owner starts the
// grace clock of every chat the owner has.
func (m *manager) detachPage(s *pageSub) {
	m.pageMu.Lock()
	if set := m.pages[s.owner]; set != nil {
		if _, ok := set[s]; ok {
			delete(set, s)
			close(s.ch)
		}
		if len(set) == 0 {
			delete(m.pages, s.owner)
		}
	}
	empty := len(m.pages[s.owner]) == 0
	m.pageMu.Unlock()
	if empty {
		for _, c := range m.listFor(s.owner) {
			c.startGrace()
		}
	}
}

// find returns the chat id if principal owns it.
func (m *manager) find(id, owner string) *chat {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.chats[id]
	if c == nil || c.owner != owner {
		return nil
	}
	return c
}

// listFor returns owner's chats sorted by id.
func (m *manager) listFor(owner string) []*chat {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*chat
	for _, c := range m.chats {
		if c.owner == owner {
			out = append(out, c)
		}
	}
	return out
}

// targetOf names the repo or lens a binding pin ("repo:<uid>" or
// "lens:<uid>") stands for, for the person to read. An unresolvable pin is
// shown as its own text.
func (m *manager) targetOf(pin string) (kind, name string) {
	kind, uid, _ := strings.Cut(pin, ":")
	switch kind {
	case "repo":
		if m.rm != nil {
			if rec, ok, err := m.rm.Repos().Get(uid); err == nil && ok {
				return "repo", rec.Name
			}
		}
	case "lens":
		if m.rm != nil {
			if l, ok, err := m.rm.LensRegistry().GetByUID(uid); err == nil && ok {
				return "lens", l.Name
			}
		}
	}
	return kind, pin
}

// newChatID is "chat-" and 32 lowercase hex digits (plan P24): the chat id,
// the MCP session id, the session log's stem and the trace id.
func newChatID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "chat-" + hex.EncodeToString(b[:]), nil
}

// create opens a chat for a principal.
func (m *manager) create(ctx context.Context, p auth.Principal, provider, model string, reach callerReach) (*chat, error) {
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return nil, errClosed
	}
	if m.openModel == nil {
		return nil, errors.New("no chat providers are configured")
	}
	cm, err := m.openModel(ctx, provider, model)
	if err != nil {
		return nil, err
	}
	run, err := m.runDir()
	if err != nil {
		return nil, err
	}
	id, err := newChatID()
	if err != nil {
		return nil, err
	}
	st, err := newChatStore(run, id)
	if err != nil {
		return nil, err
	}

	cctx, cancel := context.WithCancel(m.life)
	c := &chat{
		id: id, provider: provider, model: model, owner: p.String(), m: m,
		store: st, ctx: cctx, cancel: cancel,
		calls: map[string]llm.ToolUse{}, pending: map[string]*pendingApproval{},
		grants: map[grantKey]grantInfo{}, meta: map[string]callMeta{},
		done: make(chan struct{}),
	}
	fail := func(err error) (*chat, error) {
		cancel()
		st.remove()
		return nil, err
	}

	sess, instructions, tools, err := mcp.OpenInProcessSession(cctx, m.srv, m.rm, mcp.InProcessOptions{
		Principal: p, Ceiling: reach.ceiling, RepoReach: reach.repo, LensReach: reach.lens,
		SessionID: id, Endpoint: httpx.APIBase + "/chats/" + id,
		ClientName: "knomit-chat", ClientVersion: provider + "/" + model,
	})
	if err != nil {
		return fail(fmt.Errorf("open the MCP session: %w", err))
	}
	c.sess = sess

	b := llm.NewBuilder()
	if err := mcp.AddPrefix(b, instructions, tools); err != nil {
		_ = sess.Close(context.Background())
		return fail(err)
	}
	note := llm.Entry{ID: sessionContextID, Role: llm.RoleUser, Blocks: []llm.Block{{
		Kind: llm.BlockText,
		Text: fmt.Sprintf("Session context: this chat is %s. Today's date (UTC) is %s.", id, time.Now().UTC().Format("2006-01-02")),
	}}}
	if err := b.Add(llm.Item{ID: sessionContextID, Hook: llm.HookConversation, Entry: &note}); err != nil {
		_ = sess.Close(context.Background())
		return fail(err)
	}
	c.traceTools = traceTools(tools)

	lg, err := openSessionLog(m.logDir, id, c.callMetaOf)
	if err != nil {
		log.Warn().Err(err).Str("chat", id).Msg("chat: the session log is off for this chat")
		lg = nil
	}
	c.log = lg
	window, how, _ := contextOf(cm)
	c.log.Start(provider, model, window, how)

	var price *llm.PriceCard
	if m.price != nil {
		price = m.price(provider, model)
	}
	c.loop = &llm.Loop{
		Model: cm, Builder: b, Tools: sess, Gate: c, Sink: c.onLoopEvent,
		ArgsHook: c.argsHook, Log: c.logSink(), Price: price,
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		_ = sess.Close(context.Background())
		c.log.End("shutdown")
		return fail(errClosed)
	}
	m.chats[id] = c
	m.mu.Unlock()

	// A chat created while its owner's page stream is attached is covered by
	// it at once. With none attached, the grace clock starts now. The check
	// follows the registration above, so a stream that closes meanwhile finds
	// this chat when it starts the clocks, and one that attaches meanwhile is
	// caught by graceFired.
	if m.pageCount(c.owner) == 0 {
		c.startGrace()
	}
	return c, nil
}

// logSink returns the loop's SessionLog; a nil log must not become a non-nil
// interface holding a nil pointer the loop then calls.
func (c *chat) logSink() llm.SessionLog {
	if c.log == nil {
		return nil
	}
	return c.log
}

// traceTools lists the tools whose schema declares a `trace` argument.
func traceTools(tools []llm.ToolDef) map[string]bool {
	out := map[string]bool{}
	for _, t := range tools {
		var sch struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if json.Unmarshal(t.Schema, &sch) == nil {
			if _, ok := sch.Properties["trace"]; ok {
				out[t.Name] = true
			}
		}
	}
	return out
}

// callerReach is what the auth edge attached to the creating request; it
// travels with every call the chat makes, as mcpHTTPContext copies it.
type callerReach struct {
	ceiling *auth.Set
	repo    func(string) bool
	lens    func(string) bool
}
