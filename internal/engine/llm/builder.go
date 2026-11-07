package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// Hook is one fixed section of a request. The numeric order IS the render
// order and the cache order: everything up to a hook boundary is a prefix the
// provider can cache, so hooks are ordered from most shared to most volatile.
//
//	1 tools         MCP tool definitions, as exposed to the chat principal.
//	                Shared per permission set (fleet-wide within a set).
//	2 system        knomit's chat system prompt. Fleet-wide.
//	3 repo          the MCP server's initialize instructions for the
//	                connection. Fleet-wide for the unscoped endpoint.
//	4 frame         a materialized frame. Optional; see AddFrame.
//	5 working set   individual facts. Per conversation.
//	6 conversation  the neutral history. Per conversation.
//	7 turn          the current user input. Volatile.
//
// Hooks 1-3 must never hold machine- or time-specific bytes (hostnames,
// branch names, handles, session ids, paths, users, dates): those go into
// hook 6, as tool results or a session-context note.
type Hook int

const (
	HookTools Hook = iota + 1
	HookSystem
	HookRepo
	HookFrame
	HookWorkingSet
	HookConversation
	HookTurn

	numHooks = 8 // index 0 unused, so Hooks can be indexed by Hook
)

func (h Hook) valid() bool { return h >= HookTools && h <= HookTurn }

// ToolDef is one tool as the model sees it. Schema is canonical JSON
// (CanonicalJSON), so its bytes do not depend on map order.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"input_schema"`
	// Title and Key are for the person, never for the model: Title is the
	// tool's own annotations.title (the words a chat row shows) and Key the
	// argument names, in order of preference, whose first non-empty string
	// value is the call's main argument. Neither is encoded (json "-"), so
	// neither reaches CanonicalPrefix or an adapter's request: changing a
	// title never changes the cached tool prefix.
	Title string   `json:"-"`
	Key   []string `json:"-"`
}

// Item is one addressable piece of a hook. Exactly one payload is set: Tool
// for HookTools, Entry for history and turn entries, Text otherwise.
type Item struct {
	ID    string   `json:"id"`
	Hook  Hook     `json:"hook"`
	Text  string   `json:"text,omitempty"`
	Tool  *ToolDef `json:"tool,omitempty"`
	Entry *Entry   `json:"entry,omitempty"`
}

var (
	// ErrDuplicateItem: Add was given an ID already in the builder.
	ErrDuplicateItem = errors.New("llm builder: duplicate item id")
	// ErrUnknownItem: Replace named an ID that is not in the builder.
	ErrUnknownItem = errors.New("llm builder: unknown item id")
	// ErrHistoryAppendOnly: Replace or Remove named a HookConversation item.
	// The neutral history is append-only (history.go).
	ErrHistoryAppendOnly = errors.New("llm builder: the conversation hook is append-only")
	// ErrFrameNotImplemented is what AddFrame returns this milestone.
	ErrFrameNotImplemented = errors.New("llm builder: frame hook not implemented")
)

// FrameSpec names a materialized frame: its root fact and the anchor commit.
type FrameSpec struct{ Root, Anchor string }

// Builder assembles a request from the fixed hooks. Items are addressed by ID,
// which is unique across all hooks, and keep their insertion order within a
// hook (HookTools is sorted by name at Snapshot time instead). Add, Remove and
// Replace are the seam later context management (eviction) uses; they refuse
// to edit HookConversation, which only grows.
// A Builder is not safe for concurrent use.
type Builder struct {
	hooks [numHooks][]Item
}

// NewBuilder returns an empty builder.
func NewBuilder() *Builder { return &Builder{} }

func (b *Builder) find(id string) (Hook, int, bool) {
	for h := HookTools; h <= HookTurn; h++ {
		for i, it := range b.hooks[h] {
			if it.ID == id {
				return h, i, true
			}
		}
	}
	return 0, 0, false
}

// Add appends it to its hook.
func (b *Builder) Add(it Item) error {
	if it.ID == "" {
		return errors.New("llm builder: item has no id")
	}
	if !it.Hook.valid() {
		return fmt.Errorf("llm builder: item %q: hook %d out of range", it.ID, it.Hook)
	}
	if it.Hook == HookTools && it.Tool == nil {
		return fmt.Errorf("llm builder: item %q: a tools-hook item needs a Tool", it.ID)
	}
	if _, _, ok := b.find(it.ID); ok {
		return fmt.Errorf("%w: %q", ErrDuplicateItem, it.ID)
	}
	b.hooks[it.Hook] = append(b.hooks[it.Hook], it)
	return nil
}

// Remove deletes the item with id. It returns ErrUnknownItem when there is
// none, and ErrHistoryAppendOnly for a HookConversation item, which it leaves
// in place.
func (b *Builder) Remove(id string) error {
	h, i, ok := b.find(id)
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownItem, id)
	}
	if h == HookConversation {
		return fmt.Errorf("%w: remove %q", ErrHistoryAppendOnly, id)
	}
	b.hooks[h] = append(b.hooks[h][:i:i], b.hooks[h][i+1:]...)
	return nil
}

// Replace swaps the item with id for it, in the same position. it must carry
// the same ID and the same Hook: moving an item between hooks is a Remove and
// an Add, because it changes which cached prefix the item belongs to. A
// HookConversation item is never replaced (ErrHistoryAppendOnly).
func (b *Builder) Replace(id string, it Item) error {
	h, i, ok := b.find(id)
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownItem, id)
	}
	if h == HookConversation {
		return fmt.Errorf("%w: replace %q", ErrHistoryAppendOnly, id)
	}
	if it.ID != id || it.Hook != h {
		return fmt.Errorf("llm builder: replace %q: want id %q in hook %d, got id %q in hook %d", id, id, h, it.ID, it.Hook)
	}
	b.hooks[h][i] = it
	return nil
}

// AddFrame is the hook-4 frame source. It is NOT implemented this milestone
// and always returns ErrFrameNotImplemented, adding nothing.
//
// Intent: a frame rendered through materialize.Plan + RenderAgent, which is
// deterministic per (root, anchor, options) and pinned by default, becomes a
// stable, fleet-shareable cached prefix for chats started from that frame, and
// is never trimmed. The hook itself exists: Add accepts HookFrame items and
// the Anthropic renderer places a breakpoint after them.
func (b *Builder) AddFrame(spec FrameSpec) error {
	return fmt.Errorf("%w (root %q at %q)", ErrFrameNotImplemented, spec.Root, spec.Anchor)
}

// Snapshot is an immutable copy of the builder's items, indexed by Hook.
type Snapshot struct {
	Hooks [numHooks][]Item
}

// Snapshot copies the current items. Tools are sorted by name, so the tools
// hook never depends on registration or insertion order.
func (b *Builder) Snapshot() Snapshot {
	var s Snapshot
	for h := HookTools; h <= HookTurn; h++ {
		if len(b.hooks[h]) == 0 {
			continue
		}
		s.Hooks[h] = append([]Item(nil), b.hooks[h]...)
	}
	sort.SliceStable(s.Hooks[HookTools], func(i, j int) bool {
		return s.Hooks[HookTools][i].Tool.Name < s.Hooks[HookTools][j].Tool.Name
	})
	return s
}

// canonicalHook is one hook's line in CanonicalPrefix.
type canonicalHook struct {
	Hook  Hook   `json:"hook"`
	Items []Item `json:"items"`
}

// CanonicalPrefix is the provider-independent byte form of hooks 1..through:
// one canonical JSON line per hook, holding its items in snapshot order. Two
// snapshots whose prefixes are byte-equal render byte-equal prefixes in every
// adapter, which is what the byte-identity tests compare.
func (s Snapshot) CanonicalPrefix(through Hook) []byte {
	var out []byte
	for h := HookTools; h <= through && h <= HookTurn; h++ {
		line, err := encodeCanonical(canonicalHook{Hook: h, Items: nonNil(s.Hooks[h])})
		if err != nil {
			// Items hold only strings and already-valid RawMessage; an
			// encode failure means a ToolDef.Schema or Args that is not
			// JSON, which no caller may put in a snapshot.
			panic(fmt.Sprintf("llm: canonical prefix: hook %d: %v", h, err))
		}
		out = append(out, line...)
		out = append(out, '\n')
	}
	return out
}

func nonNil(items []Item) []Item {
	if items == nil {
		return []Item{}
	}
	return items
}

// RequestRecord is the per-request instrumentation: which items each hook
// held and their estimated token counts (bytes/4; no extra API calls).
// Indexed by Hook; index 0 is unused.
type RequestRecord struct {
	Items     [numHooks][]string `json:"items"`
	Tokens    [numHooks]int      `json:"tokens"`
	Estimated bool               `json:"estimated"`
}

// Record names the items in each hook and estimates each hook's tokens as its
// payload bytes divided by four.
func (s Snapshot) Record() RequestRecord {
	r := RequestRecord{Estimated: true}
	for h := HookTools; h <= HookTurn; h++ {
		n := 0
		for _, it := range s.Hooks[h] {
			r.Items[h] = append(r.Items[h], it.ID)
			n += itemBytes(it)
		}
		r.Tokens[h] = n / 4
	}
	return r
}

func itemBytes(it Item) int {
	switch {
	case it.Tool != nil:
		raw, _ := encodeCanonical(it.Tool)
		return len(raw)
	case it.Entry != nil:
		raw, _ := encodeCanonical(it.Entry)
		return len(raw)
	default:
		return len(it.Text)
	}
}
