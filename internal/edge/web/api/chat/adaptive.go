package chat

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/rs/zerolog/log"

	"knomit/internal/edge/mcp"
	"knomit/internal/engine/llm"
	"knomit/internal/engine/store/model"
)

// The adaptive tool path of a chat: for a provider with no prompt cache to
// protect (Registry.Adaptive), the loop gets a ToolWindow (built by
// mcp.NewChatToolWindow) and sends the loader plus the loaded tools. This file
// is the chat's side of it: the ranker that guesses the tools of a turn, and
// the view of the window's state the page and the session log show.

// Ranker kinds, as the session log's preload record names them.
const (
	rankerEmbedding = "embedding"
	rankerLexical   = "lexical"
	// rankerBlended is the embedding and lexical rankings fused by rank. A turn
	// whose embedding ranker failed is logged as lexical: the guess was the
	// lexical ranker's alone.
	rankerBlended = "blended"
)

// turnTools is a turn's view of the loaded tools, on its user item and on
// turn_done: counts, because the page words them. Loaded and Evicted are this
// turn's changes; LoadedNow, Total (the catalogue) and Tokens (definition
// tokens, bytes/4, of the loaded set) are the window's state after them.
type turnTools struct {
	Loaded    int `json:"loaded"`
	Evicted   int `json:"evicted"`
	LoadedNow int `json:"loaded_now"`
	Total     int `json:"total"`
	Tokens    int `json:"tokens"`
}

// rankRecord is what the ranker did for one turn's guess.
type rankRecord struct {
	Kind         string           // the ranker that produced Ranked: embedding or lexical
	Ranked       []llm.ScoredTool // best first
	EmbeddingErr string           // why the embedding ranker was passed over, if it was
}

// rankTap records what the rankers answer so the session log can say which one
// guessed and with what scores. It is the only thing between the loop and the
// rankers; the loop never sees it.
type rankTap struct {
	mu  sync.Mutex
	rec rankRecord
	ran bool
}

// wrap is r, noting each answer under kind.
func (t *rankTap) wrap(kind string, r llm.ToolRanker) llm.ToolRanker {
	return rankFunc(func(ctx context.Context, msg string) ([]llm.ScoredTool, error) {
		out, err := r.Rank(ctx, msg)
		t.mu.Lock()
		defer t.mu.Unlock()
		t.ran = true
		switch {
		case err != nil && kind == rankerEmbedding:
			t.rec.EmbeddingErr = rankErrorClass(err.Error())
		case err == nil:
			t.rec.Kind, t.rec.Ranked = kind, out
		}
		return out, err
	})
}

// watchEmbedding is r, noting only its failure (as the class of the error): the
// blended ranker around it records what the turn's guess was.
func (t *rankTap) watchEmbedding(r llm.ToolRanker) llm.ToolRanker {
	return rankFunc(func(ctx context.Context, msg string) ([]llm.ScoredTool, error) {
		out, err := r.Rank(ctx, msg)
		if err != nil {
			t.mu.Lock()
			t.rec.EmbeddingErr = rankErrorClass(err.Error())
			t.mu.Unlock()
		}
		return out, err
	})
}

// wrapBlend is the blended ranker, noting each answer: blended, or lexical when
// the embedding ranker inside it failed and the guess is the lexical one alone.
func (t *rankTap) wrapBlend(blend llm.ToolRanker) llm.ToolRanker {
	return rankFunc(func(ctx context.Context, msg string) ([]llm.ScoredTool, error) {
		out, err := blend.Rank(ctx, msg)
		t.mu.Lock()
		defer t.mu.Unlock()
		t.ran = true
		if err == nil {
			t.rec.Kind, t.rec.Ranked = rankerBlended, out
			if t.rec.EmbeddingErr != "" {
				t.rec.Kind = rankerLexical
			}
		}
		return out, err
	})
}

// take returns what the ranker did since the last take, and whether it ran.
func (t *rankTap) take() (rankRecord, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	rec, ran := t.rec, t.ran
	t.rec, t.ran = rankRecord{}, false
	return rec, ran
}

// Classes of a ranker failure, as the session log names them.
const (
	rankErrTimeout  = "timeout"
	rankErrCanceled = "canceled"
	rankErrOther    = "error"
)

// rankErrorClass is the class of a ranker failure: the session log records it
// instead of the error's text, which an embedder fills with model paths and
// other local detail. "" stays "".
func rankErrorClass(msg string) string {
	switch low := strings.ToLower(msg); {
	case msg == "":
		return ""
	case strings.Contains(low, "deadline exceeded"), strings.Contains(low, "timeout"), strings.Contains(low, "timed out"):
		return rankErrTimeout
	case strings.Contains(low, "canceled"), strings.Contains(low, "cancelled"):
		return rankErrCanceled
	}
	return rankErrOther
}

type rankFunc func(ctx context.Context, msg string) ([]llm.ScoredTool, error)

func (f rankFunc) Rank(ctx context.Context, msg string) ([]llm.ScoredTool, error) { return f(ctx, msg) }

// adaptiveState is what an adaptive chat keeps beside its loop: the ranker's
// tap, the fixed sizes the page and the log are worded from, and the loaded set
// as the last LoopToolsChanged left it. nil on a static chat. Everything below
// the fixed fields is guarded by chat.mu.
type adaptiveState struct {
	tap     *rankTap
	ranker  string   // the ranker the chat started with
	total   int      // tools in the catalogue
	usable  int      // window less the reply reserve; 0 when the model reports no window
	budget  int      // the preload budget P in tokens
	loaded  int      // tools loaded now
	tokens  int      // definition tokens of the loaded set
	loadRow []string // what the loader's current call loaded, for its transcript row
}

// loggedRanked is how many of the ranker's best guesses the preload record
// keeps: enough to see what came next after the picks, not the whole ranking.
const loggedRanked = 5

// turnView is the turn's tools as the page shows them: this turn's changes so
// far and the window's state now. The caller holds chat.mu. It returns a new
// value every time (a snapshot copies the item and encodes it after releasing
// the lock).
func (a *adaptiveState) turnView(loaded, evicted int) *turnTools {
	return &turnTools{Loaded: loaded, Evicted: evicted, LoadedNow: a.loaded, Total: a.total, Tokens: a.tokens}
}

// onToolsChanged keeps the page and the log in step with the window: the
// turn's counts on its user item, a `tools` event, and a `tools` log record.
// It runs on the turn goroutine, from the loop's sink.
func (c *chat) onToolsChanged(ch llm.ToolsChange) {
	if c.adapt == nil {
		return
	}
	c.mu.Lock()
	a := c.adapt
	a.loaded, a.tokens = len(ch.LoadedNow), ch.Tokens
	if ch.Reason == llm.ToolsReasonModel {
		a.loadRow = append(a.loadRow, ch.Loaded...)
	}
	turn := c.turnAt
	if turn < len(c.view) {
		loaded, evicted := len(ch.Loaded), len(ch.Evicted)
		if prev := c.view[turn].Tools; prev != nil {
			loaded, evicted = loaded+prev.Loaded, evicted+prev.Evicted
		}
		c.view[turn].Tools = a.turnView(loaded, evicted)
	}
	total := a.total
	c.mu.Unlock()

	ev := map[string]any{
		"turn": turn, "reason": ch.Reason, "loaded": nonNilStrings(ch.Loaded), "evicted": nonNilStrings(ch.Evicted),
		"loaded_now": nonNilStrings(ch.LoadedNow), "total": total, "tokens": ch.Tokens,
	}
	c.emit("tools", ev)
	c.log.Tools(turn, ch)
}

// logPreload writes the turn's preload record when the turn ended: the ranker
// that guessed, its best guesses, the budget, what was loaded and which of
// those the model then called. A turn the ranker did not run for has none.
func (c *chat) logPreload(turn int, res *llm.TurnResult) {
	if c.adapt == nil || res == nil {
		return
	}
	rec, ran := c.adapt.tap.take()
	if !ran && res.RankerError == "" {
		return
	}
	score := map[string]float64{}
	for _, s := range rec.Ranked {
		score[s.Name] = s.Score
	}
	picks := make([]map[string]any, 0, len(res.Preloaded))
	for _, n := range res.Preloaded {
		picks = append(picks, map[string]any{"name": n, "score": score[n]})
	}
	ranked := make([]map[string]any, 0, loggedRanked)
	for i, s := range rec.Ranked {
		if i == loggedRanked {
			break
		}
		ranked = append(ranked, map[string]any{"name": s.Name, "score": s.Score})
	}
	fields := map[string]any{
		"turn": turn, "ranker": rec.Kind, "ranked": ranked, "picks": picks, "budget": c.adapt.budget,
		"loaded": nonNilStrings(res.Preloaded), "hits": nonNilStrings(res.PreloadHits),
		"picked": len(res.Preloaded), "called": len(res.PreloadHits),
	}
	if rec.EmbeddingErr != "" {
		fields["embedding_error"] = rec.EmbeddingErr
	}
	if res.RankerError != "" {
		fields["ranker_error"] = rankErrorClass(res.RankerError)
	}
	c.log.Preload(fields)
}

// openWindow builds the ToolWindow of an adaptive chat. The catalogue (the tool
// names) must fit the preload budget P (window less reserve, over
// PreloadDivisor), and the opening (loader, core in full, a stub per other
// tool, system prompt and instructions) must leave P free; if either does not,
// the chat does not open, with the adaptive sentence of the opening-too-big
// refusal.
func (m *manager) openWindow(c *chat, cm llm.ChatModel, tools []llm.ToolDef, b *llm.Builder) (*llm.ToolWindow, error) {
	caps := cm.Caps()
	st := &adaptiveState{total: len(tools)}
	if caps.ContextWindow > 0 {
		st.usable = caps.ContextWindow - llm.ReplyReserve(caps)
		// A window with no room still has a budget to fail against.
		st.budget = max(llm.PreloadBudget(st.usable), 1)
	}
	ranker, tap, primary := newRanker(m.emb, tools)
	st.tap, st.ranker = tap, primary
	win, err := mcp.NewChatToolWindow(tools, ranker, st.budget)
	var big *mcp.CatalogueTooBigError
	switch {
	case errors.As(err, &big):
		return nil, llm.OpeningTooBigError(true, cm.Provider(), cm.Model(), big.Need, llm.ReplyReserve(caps), caps.ContextWindow)
	case err != nil:
		return nil, err
	}
	// The core tools are loaded from the opening: the first turn shows them.
	loaded, tokens := win.Observe(b.Snapshot())
	st.loaded, st.tokens = len(loaded), tokens
	// The opening counts what is really sent: the loader, the core in full,
	// the stub of every other tool, the system prompt and the repo
	// instructions. It must leave P free for the first guess and the message,
	// or the chat is refused here and not on its first turn.
	if st.usable > 0 {
		if open := openingTokens(win, b); open > st.usable-st.budget {
			return nil, llm.OpeningTooBigError(true, cm.Provider(), cm.Model(), open+st.budget, llm.ReplyReserve(caps), caps.ContextWindow)
		}
	}
	c.adapt = st
	c.log.Adaptive(primary, st.total, st.budget)
	return win, nil
}

// openingTokens is the estimate (bytes/4) of the request a new chat opens with,
// before any message: the window's projection of b.
func openingTokens(win *llm.ToolWindow, b *llm.Builder) int {
	rec := win.Project(b.Snapshot()).Record()
	n := 0
	for h := llm.HookTools; h <= llm.HookTurn; h++ {
		n += rec.Tokens[h]
	}
	return n
}

// warmRanker embeds the toolset in the background so the chat's first turn pays
// one query embedding, not one per tool. A failure costs only the head start:
// the ranker embeds what is missing, and falls back to the lexical guess if the
// embedder is down.
func (m *manager) warmRanker(c *chat, tools []llm.ToolDef) {
	if m.emb == nil {
		return
	}
	go func() {
		if err := mcp.WarmToolVectors(c.ctx, m.emb, tools); err != nil && c.ctx.Err() == nil {
			log.Warn().Err(err).Str("chat", c.id).Msg("chat: could not warm the tool vectors")
		}
	}()
}

// newRanker builds the chat's ranker over the toolset: the embedding and the
// lexical rankings fused by rank (llm.NewBlendedRanker) when the process has an
// embedder, and the lexical ranker alone when it has none. A call-time failure
// of the embedder leaves the lexical ranking to guess alone. primary names the
// ranker the chat starts with.
func newRanker(emb model.Embedder, tools []llm.ToolDef) (r llm.ToolRanker, tap *rankTap, primary string) {
	tap = &rankTap{}
	if emb == nil {
		return tap.wrap(rankerLexical, llm.NewLexicalRanker(tools)), tap, rankerLexical
	}
	em := tap.watchEmbedding(mcp.NewEmbeddingRanker(emb, tools))
	return tap.wrapBlend(llm.NewBlendedRanker(em, llm.NewLexicalRanker(tools))), tap, rankerBlended
}
