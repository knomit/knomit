package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/rs/zerolog/log"

	"knomit/internal/engine/llm"
)

// A chat's title has two sources (decision, chat review 2026-11-08: auto-title
// plus rename, chosen over rename-only and over auto-only):
//
//   - Auto: once the chat has COMPLETED titleAfterTurns (3) turns, the chat's own
//     model is asked once, with no tools, no thinking and a tight output budget,
//     for a 3-6 word title (titlePrompt), from the conversation so far (cut to
//     titleContextRunes). Not after the first turn: people bind first ("bind to
//     X") and only then start the real work, so the first turn has nothing to
//     name. A turn counts only when it finished normally: a turn that errored or
//     that the person stopped does not. A chat with fewer than three completed
//     turns never gets an auto-title and keeps its first-message name until it is
//     renamed. A usable answer replaces the first-message name; a failed call or
//     an unusable answer leaves that name. An answer cut off at the budget counts
//     only when its first line was finished (firstLineEnded). There is no retry:
//     the attempt is made once per chat (titleTried).
//   - User: PATCH /chats/{id} {"title"} renames the chat and marks it
//     user-titled, and from then on auto-title never touches it, including an
//     auto call already in flight when the rename lands.
//
// Either way the title lives with the chat on the server (the rail row and the
// top bar both read it from rowState) and is written to the persistent session
// log as a "title" record; the change goes out as chat_state, the event that
// already carries the row.

// titlePrompt is the auto-title request's instruction, sent as the last user
// message after the conversation so far. Model-facing copy (approved verbatim).
const titlePrompt = "Write a short title for this conversation, 3 to 6 words, that says what it is about. " +
	"Name the subject, not the steps: leave out setup instructions such as binding to a repository. " +
	"Reply with the title only, with no quotes and no closing punctuation."

const (
	// titleMaxTokens is the auto-title request's output budget: room for six
	// words in any tokenizer, and no more. Thinking is off (NoThinking), so none
	// of it is spent on reasoning.
	titleMaxTokens = 32
	// titleAfterTurns is how many turns must complete normally before the one
	// auto-title call is made.
	titleAfterTurns = 3
	// titleContextRunes bounds the conversation the request carries, in total.
	// Each user message keeps up to titleMessageRunes and goes in first; the
	// answers share what is left, each up to titleMessageRunes. Three turns at
	// the caps (3 x 600 + 3 x 600) fit inside it.
	titleContextRunes = 4000
	titleMessageRunes = 600
	// titleTimeout bounds the one call.
	titleTimeout = 60 * time.Second
	// titleMaxWords and titleMaxRunes bound what is accepted as a title. An
	// answer past them is the model talking, not naming, and is not used.
	titleMaxWords = 12
	titleMaxRunes = 100
)

// errBadTitle is a rename to nothing (empty or whitespace only).
var errBadTitle = errors.New("title is empty")

// titleStreamer sends the auto-title request. It is a manager seam: the default
// streams the chat's own model (defaultTitleStream); tests replace it.
type titleStreamer func(ctx context.Context, c *chat, s llm.Snapshot, o llm.StreamOptions) (*llm.Response, error)

func defaultTitleStream(ctx context.Context, c *chat, s llm.Snapshot, o llm.StreamOptions) (*llm.Response, error) {
	// A no-op sink, NOT nil: ChatModel says sink may be nil, but the chat's model
	// is wrapped by llm.WrapChat, whose resilient wrapper calls sink unguarded.
	return c.loop.Model.Stream(ctx, s, o, func(llm.Event) {})
}

// setUserTitle renames the chat. The name is the text with its whitespace
// collapsed and cut as a first-message name is (titleOf); empty is refused.
func (c *chat) setUserTitle(text string) (string, error) {
	t := titleOf(text)
	if t == "" {
		return "", errBadTitle
	}
	c.mu.Lock()
	if c.ended {
		c.mu.Unlock()
		return "", errEnded
	}
	c.title, c.userTitled = t, true
	c.lastActive = time.Now()
	c.mu.Unlock()
	c.log.Title(map[string]any{"source": "user", "title": t})
	c.noteState()
	return t, nil
}

// maybeAutoTitle counts a turn that completed normally and, at the
// titleAfterTurns-th, starts the one auto-title call. turn is that turn's view
// index (its user item): the call's cost is added to it. It must be called from
// the turn's goroutine while c.turns still counts it, so the chat's end waits
// for the call too.
func (c *chat) maybeAutoTitle(turn int) {
	c.mu.Lock()
	c.turnsDone++
	if c.turnsDone < titleAfterTurns || c.ended || c.userTitled || c.titleTried || c.m.titleStream == nil {
		c.mu.Unlock()
		return
	}
	c.titleTried = true
	snap, ok := c.titleSnapshotLocked(turn)
	c.mu.Unlock()
	if !ok {
		return
	}
	c.turns.Add(1)
	go func() {
		defer c.turns.Done()
		c.autoTitle(turn, snap)
	}()
}

// titleSnapshotLocked is the auto-title request: the conversation up to and
// including the turn at view index turn (every user message, and each turn's
// assistant text), cut to titleContextRunes as the constants say, then
// titlePrompt. No tools, no system prompt.
func (c *chat) titleSnapshotLocked(turn int) (llm.Snapshot, bool) {
	if turn < 0 || turn >= len(c.view) || c.view[turn].Kind != "user" {
		return llm.Snapshot{}, false
	}
	// The turns: each user message and its answer's text, in order.
	type exchange struct{ user, answer string }
	var turns []exchange
	end := len(c.view)
	for i := turn + 1; i < len(c.view); i++ {
		if c.view[i].Kind == "user" {
			end = i
			break
		}
	}
	for _, it := range c.view[:end] {
		switch {
		case it.Kind == "user":
			turns = append(turns, exchange{user: it.Text})
		case it.Kind == "assistant" && strings.TrimSpace(it.Text) != "" && len(turns) > 0:
			x := &turns[len(turns)-1]
			if x.answer != "" {
				x.answer += "\n\n"
			}
			x.answer += it.Text
		}
	}
	// The user messages go in first, then the answers share what is left.
	left := titleContextRunes
	for i := range turns {
		turns[i].user = cutRunes(strings.TrimSpace(turns[i].user), min(titleMessageRunes, max(left, 0)))
		left -= len([]rune(turns[i].user))
	}
	for i := range turns {
		turns[i].answer = cutRunes(strings.TrimSpace(turns[i].answer), min(titleMessageRunes, max(left, 0)))
		left -= len([]rune(turns[i].answer))
	}
	var s llm.Snapshot
	entry := func(id string, role llm.Role, text string) llm.Item {
		return llm.Item{ID: id, Hook: llm.HookConversation, Entry: &llm.Entry{ID: id, Role: role, Blocks: []llm.Block{{Kind: llm.BlockText, Text: text}}}}
	}
	for i, x := range turns {
		if x.user != "" {
			s.Hooks[llm.HookConversation] = append(s.Hooks[llm.HookConversation], entry(fmt.Sprintf("title-user-%d", i), llm.RoleUser, x.user))
		}
		if x.answer != "" {
			s.Hooks[llm.HookConversation] = append(s.Hooks[llm.HookConversation], entry(fmt.Sprintf("title-answer-%d", i), llm.RoleAssistant, x.answer))
		}
	}
	t := entry("title-ask", llm.RoleUser, titlePrompt)
	t.Hook = llm.HookTurn
	s.Hooks[llm.HookTurn] = []llm.Item{t}
	return s, true
}

// autoTitle makes the one call and applies its answer. Whatever happens, the
// call's usage (when the provider reported one) is priced into the turn.
func (c *chat) autoTitle(turn int, snap llm.Snapshot) {
	ctx, cancel := context.WithTimeout(c.ctx, titleTimeout)
	defer cancel()
	resp, err := c.m.titleStream(ctx, c, snap, llm.StreamOptions{MaxOutput: titleMaxTokens, NoThinking: true})
	var stop *llm.StopError
	if err != nil && errors.As(err, &stop) {
		resp = stop.Response
	}
	if resp != nil {
		c.addSideUsage(turn, resp.Usage)
	}
	// A small model often names the chat on its first line and then keeps
	// going until the budget cuts it off (lfm3 repeats the title in variants).
	// A first line the model ENDED (a newline follows it) is a whole answer.
	if stop != nil && stop.Reason == llm.StopMaxTokens && firstLineEnded(resp) {
		err = nil
	}
	if err != nil {
		log.Warn().Err(err).Str("chat", c.id).Msg("chat: the title request failed; keeping the first-message title")
		return
	}
	title := cleanTitle(resp)
	if title == "" {
		log.Warn().Str("chat", c.id).Msg("chat: the title request gave no usable title; keeping the first-message title")
		return
	}
	c.mu.Lock()
	if c.ended || c.userTitled {
		// The person renamed it while the call ran: theirs stands.
		c.mu.Unlock()
		return
	}
	c.title = title
	c.mu.Unlock()
	c.log.Title(map[string]any{"source": "auto", "title": title, "turn": turn, "usage": usageFields(resp.Usage)})
	c.noteState()
}

// addSideUsage adds a side request's usage, and its price, to the turn's view
// item, which is where every request's usage and cost are summed (onLoopEvent's
// LoopUsage). The session cost (the rail's cost_usd, the page's sessionCost) is
// the sum of the turns, so the side request counts toward it as the turn's own
// requests do. It is announced as a usage event marked side, which the page
// adds to the turn without taking it for the context meter's latest prompt.
func (c *chat) addSideUsage(turn int, u llm.Usage) {
	var cost *llm.Cost
	if c.loop != nil && c.loop.Price != nil {
		cc := c.loop.Price.Estimate(u)
		cost = &cc
	}
	c.mu.Lock()
	if c.ended || turn >= len(c.view) {
		c.mu.Unlock()
		return
	}
	it := &c.view[turn]
	// NEW values, never edits: a snapshot encodes the item after the lock.
	var tu turnUsage
	if it.Usage != nil {
		tu = *it.Usage
	}
	tu.Input += u.Input
	tu.CacheRead += u.CacheRead
	tu.CacheWrite += u.CacheWrite
	tu.Output += u.Output
	it.Usage = &tu
	if cost != nil {
		usd := cost.USD
		if it.CostUSD != nil {
			usd += *it.CostUSD
		}
		it.CostUSD = &usd
		var sum llm.Cost
		if it.CostRows != nil {
			sum.Rows = llm.CostRows(*it.CostRows)
		}
		it.CostRows = costRowsOf(sum.Add(llm.Cost{Rows: cost.Rows}).Rows)
	}
	c.usageSeq++
	seq := c.usageSeq
	c.mu.Unlock()
	out := map[string]any{"usage": usageView(u), "seq": seq, "turn": turn, "side": true}
	if cost != nil {
		out["cost_usd"] = cost.USD
		out["cost_rows"] = costRowsOf(cost.Rows)
	}
	c.emit("usage", out)
	c.noteState()
}

// cleanTitle is the answer's text as a title: its first non-empty line, with
// whitespace collapsed, wrapping quotes and a closing full stop removed. Empty
// when the answer has no text, or is too long to be a title.
func cleanTitle(resp *llm.Response) string {
	if resp == nil {
		return ""
	}
	var text strings.Builder
	for _, b := range resp.Blocks {
		if b.Kind == llm.BlockText {
			text.WriteString(b.Text)
		}
	}
	line := ""
	for _, l := range strings.Split(text.String(), "\n") {
		if strings.TrimSpace(l) != "" {
			line = l
			break
		}
	}
	t := strings.Join(strings.Fields(line), " ")
	t = strings.TrimPrefix(t, "Title:")
	t = strings.TrimSpace(t)
	t = strings.Trim(t, "\"'`*“”‘’")
	t = strings.TrimRightFunc(t, func(r rune) bool { return r == '.' || unicode.IsSpace(r) })
	if t == "" || len(strings.Fields(t)) > titleMaxWords || len([]rune(t)) > titleMaxRunes {
		return ""
	}
	return t
}

// firstLineEnded reports whether the response's text has a non-empty line that
// a newline closes: on a cut-off response, that line was finished.
func firstLineEnded(resp *llm.Response) bool {
	if resp == nil {
		return false
	}
	var text strings.Builder
	for _, b := range resp.Blocks {
		if b.Kind == llm.BlockText {
			text.WriteString(b.Text)
		}
	}
	lines := strings.Split(text.String(), "\n")
	for _, l := range lines[:len(lines)-1] {
		if strings.TrimSpace(l) != "" {
			return true
		}
	}
	return false
}

// cutRunes keeps the first n runes of s.
func cutRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
