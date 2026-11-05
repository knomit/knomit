package chat

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rs/zerolog/log"

	"knomit/internal/engine/llm"
)

// maxLoggedResult is how much of one tool result the session log keeps inline
// (plan P23). A longer result is cut at a rune boundary and flagged; the full
// result lives only in the chat's temp file, which is deleted with the chat.
const maxLoggedResult = 8 * 1024

// hookNames name the builder's hooks in the log, in hook order (index 0 is
// unused, as in llm.RequestRecord).
var hookNames = [...]string{"", "tools", "system", "repo", "frame", "working_set", "conversation", "turn"}

// sessionLog is the persistent, append-only JSONL record of one chat at
// <logDir>/<chatID>.jsonl (dir 0700, file 0600). It implements llm.SessionLog.
// The filename stem and every line's session_id are the canonical chat id, the
// same string as the clientsessions session id. It outlives the chat: nothing
// in this package removes it.
type sessionLog struct {
	id string

	mu     sync.Mutex
	f      *os.File
	enc    *json.Encoder
	seq    int
	failed bool

	// meta returns what the gate knew about a call when it decided: the
	// grant action and the target pin.
	meta func(callID string) (action, pin string)
}

var _ llm.SessionLog = (*sessionLog)(nil)

// openSessionLog creates the chat's log file. An empty dir disables the log
// (a nil *sessionLog is a valid, silent log).
func openSessionLog(dir, id string, meta func(string) (string, string)) (*sessionLog, error) {
	if dir == "" {
		return nil, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("chat log dir: %w", err)
	}
	// MkdirAll leaves an existing directory's mode alone; this is the one
	// directory the package creates for the log, so tighten it.
	_ = os.Chmod(dir, 0o700)
	f, err := os.OpenFile(filepath.Join(dir, id+".jsonl"), os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("chat log: %w", err)
	}
	return &sessionLog{id: id, f: f, enc: json.NewEncoder(f), meta: meta}, nil
}

// write appends one record: type, then the caller's fields.
func (l *sessionLog) write(typ string, fields map[string]any) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return
	}
	l.seq++
	rec := map[string]any{"session_id": l.id, "seq": l.seq, "ts": time.Now().UTC().Format(time.RFC3339Nano), "type": typ}
	for k, v := range fields {
		rec[k] = v
	}
	if err := l.enc.Encode(rec); err != nil && !l.failed {
		l.failed = true
		log.Warn().Err(err).Str("chat", l.id).Msg("chat: writing the session log failed")
	}
}

// Start writes chat_start with the model's resolved context window and how it
// was resolved (llm.NumCtxHow*; "" when the model does not say).
func (l *sessionLog) Start(provider, model string, window int, how string) {
	l.write("chat_start", map[string]any{"provider": provider, "model": model, "context_window": window, "context_window_how": how})
}

// End writes chat_end and closes the file.
func (l *sessionLog) End(reason string) {
	if l == nil {
		return
	}
	l.write("chat_end", map[string]any{"reason": reason})
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		_ = l.f.Close()
		l.f = nil
	}
}

func (l *sessionLog) Request(r llm.RequestLog) {
	items := map[string][]string{}
	tokens := map[string]int{}
	for h := llm.HookTools; h <= llm.HookTurn; h++ {
		items[hookNames[h]] = nonNilStrings(r.Items[h])
		tokens[hookNames[h]] = r.Tokens[h]
	}
	rec := map[string]any{
		"provider": r.Provider, "model": r.Model, "items": items, "tokens": tokens,
		"estimated": r.Estimated, "usage": usageFields(r.Usage),
	}
	if p := r.Projection; p != nil {
		// An adaptive chat's request is the projection of the stored history:
		// items and tokens above are what was sent, and this is how much less.
		rec["projection"] = map[string]any{"stubbed_results": p.StubbedResults, "saved_tokens_est": p.SavedTokensEst}
	}
	l.write("request", rec)
}

// Adaptive writes the `adaptive` record, once, when an adaptive chat opens: the
// ranker it starts with, the catalogue's size and the preload budget P.
func (l *sessionLog) Adaptive(ranker string, tools, budget int) {
	l.write("adaptive", map[string]any{"ranker": ranker, "tools": tools, "preload_budget": budget})
}

// Tools writes a `tools` record for one change to the loaded set.
func (l *sessionLog) Tools(turn int, ch llm.ToolsChange) {
	f := map[string]any{
		"turn": turn, "reason": ch.Reason, "loaded": nonNilStrings(ch.Loaded), "evicted": nonNilStrings(ch.Evicted),
		"loaded_now": nonNilStrings(ch.LoadedNow), "tokens": ch.Tokens,
	}
	if ch.RankerError != "" {
		f["ranker_error"] = rankErrorClass(ch.RankerError)
	}
	l.write("tools", f)
}

// Preload writes a turn's `preload` record (see chat.logPreload).
func (l *sessionLog) Preload(fields map[string]any) { l.write("preload", fields) }

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func usageFields(u llm.Usage) map[string]any {
	m := map[string]any{
		"input": u.Input, "cache_read": u.CacheRead, "cache_write": u.CacheWrite, "output": u.Output,
		"cache_write_5m": u.CacheWrite5m, "cache_write_1h": u.CacheWrite1h,
		"prompt_eval": u.PromptEval, "estimated": u.Estimated,
	}
	if len(u.EstimatedFields) > 0 {
		m["estimated_fields"] = u.EstimatedFields
	}
	return m
}

func (l *sessionLog) Turn(e llm.Entry) {
	l.write("turn", map[string]any{"entry": e})
}

func (l *sessionLog) ToolCall(c llm.ToolCallLog) {
	result, truncated := c.Content, false
	if len(result) > maxLoggedResult {
		cut := maxLoggedResult
		for cut > 0 && !utf8.RuneStart(result[cut]) {
			cut--
		}
		result, truncated = result[:cut], true
	}
	args := c.Args
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	l.write("tool_call", map[string]any{
		"call_id": c.CallID, "name": c.Name, "args": args, "is_error": c.IsError,
		"result": result, "truncated": truncated, "bytes": len(c.Content),
	})
}

func (l *sessionLog) Approval(a llm.ApprovalLog) {
	var action, pin string
	if l != nil && l.meta != nil {
		action, pin = l.meta(a.CallID)
	}
	l.write("approval", map[string]any{
		"call_id": a.CallID, "tool": a.Tool, "action": action, "decision": a.Decision, "pin": pin,
	})
}
