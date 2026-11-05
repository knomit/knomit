package llm

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Stubs: the render-time projection of ended turns, for an adaptive provider
// (Loop.Window != nil) only.
//
// The Builder's hook 6 is never edited: it keeps every tool_result whole and
// still refuses Replace and Remove (ErrHistoryAppendOnly). What the loop SENDS
// is a copy in which each tool_result of an ended turn is replaced by a stub: a
// one-line header saying the result was not kept, how big it was, and whether
// the model may get it back, then a short head excerpt. The tool_use block it
// answers is not touched, so the pairing by id holds in every request
// (invariant da80d551), and the running turn (hook 7) is sent whole. The
// session log's turn records are written from the stored entries and are
// whole. A provider with a stable prefix (Anthropic) never gets a window and
// never gets a stub.

// Stub copy: approved by the user (design section 7, C6). The model reads
// these, so they are constants, not inline strings.
const (
	// StubReplayable heads a stub whose call may be repeated to get the data
	// back. Arguments: the tool's name, tokens (bytes/4), the tool's name.
	StubReplayable = "[Earlier result of %s not kept: about %d tokens. Call %s again with the same arguments to get it back.]"
	// StubNotReplayable heads a stub whose call must not be offered back: a
	// write, or a read with a session effect. Arguments: name, tokens.
	StubNotReplayable = "[Earlier result of %s not kept: about %d tokens. Do not repeat this call unless the person asks.]"
	// StubExcerptIntro introduces the head excerpt; StubExcerptCut ends it when
	// the result was cut.
	StubExcerptIntro = "Start of the result:"
	StubExcerptCut   = "..."
)

// stubExcerptTokens is the most tokens of a result's head a stub keeps:
// E = U/64, U being the usable room (window less the reply reserve). It is a
// policy fraction, not a measurement: an excerpt is for recognition, not use,
// since the model re-fetches the real data, and it must scale with the window.
func stubExcerptTokens(usable int) int {
	if usable <= 0 {
		return 0
	}
	return usable / 64
}

// stubResult replaces res by its stub, keeping its id and error flag and
// setting Elided. The excerpt is at most capBytes of the result's head, cut at
// a rune boundary and preferably a line boundary. A result the stub would not
// shorten is returned as it is.
func stubResult(res ToolResult, name string, replayable bool, capBytes int) ToolResult {
	tokens := len(res.Content) / 4
	var text string
	if replayable {
		text = fmt.Sprintf(StubReplayable, name, tokens, name)
	} else {
		text = fmt.Sprintf(StubNotReplayable, name, tokens)
	}
	if capBytes > 0 && res.Content != "" {
		text += " " + StubExcerptIntro + " " + excerpt(res.Content, capBytes)
	}
	if len(text) >= len(res.Content) {
		return res
	}
	res.Content = text
	res.Elided = true
	return res
}

// excerpt is the head of s of at most capBytes bytes, then StubExcerptCut when
// s was cut.
func excerpt(s string, capBytes int) string {
	if len(s) <= capBytes {
		return s
	}
	head := s[:capBytes]
	for len(head) > 0 && !utf8.RuneStart(s[len(head)]) { // s[len(head)] is the first byte left out
		head = head[:len(head)-1]
	}
	if i := strings.LastIndexByte(head, '\n'); i >= len(head)/2 {
		head = head[:i]
	}
	return head + StubExcerptCut
}

// StubEnded returns snap with every tool_result of the ended turns (hook 6)
// replaced by its stub; hook 7, the running turn, and every other hook are
// returned as they are. usable is the room a request has (window less the
// reply reserve); 0 means the window is unknown and nothing is stubbed. It
// copies: snap and the Builder behind it are not touched. A result whose
// tool_use is not in the snapshot is left whole, since the stub names the tool.
func (w *ToolWindow) StubEnded(snap Snapshot, usable int) Snapshot {
	if usable <= 0 {
		return snap
	}
	calls := map[string]ToolUse{}
	for _, h := range []Hook{HookConversation, HookTurn} {
		for _, it := range snap.Hooks[h] {
			if it.Entry == nil {
				continue
			}
			for _, c := range toolUses(it.Entry.Blocks) {
				calls[c.ID] = c
			}
		}
	}
	capBytes := 4 * stubExcerptTokens(usable)

	out := snap
	out.Hooks[HookConversation] = make([]Item, len(snap.Hooks[HookConversation]))
	for i, it := range snap.Hooks[HookConversation] {
		out.Hooks[HookConversation][i] = w.stubItem(it, calls, capBytes)
	}
	return out
}

// stubItem is it with its tool_results stubbed, as a copy; it is returned
// itself when nothing in it changes.
func (w *ToolWindow) stubItem(it Item, calls map[string]ToolUse, capBytes int) Item {
	if it.Entry == nil {
		return it
	}
	var blocks []Block
	for i, bl := range it.Entry.Blocks {
		if bl.Kind != BlockToolResult || bl.ToolResult == nil {
			continue
		}
		c, ok := calls[bl.ToolResult.ToolUseID]
		// A loader result is small and tells the model it may load again
		// ("Load them again if you need them"); a stub saying "do not repeat
		// this call" would contradict it.
		if !ok || c.Name == LoadToolsName {
			continue
		}
		stub := stubResult(*bl.ToolResult, c.Name, w.replayableCall(c.Name, c.Args), capBytes)
		if !stub.Elided {
			continue
		}
		if blocks == nil {
			blocks = append([]Block(nil), it.Entry.Blocks...)
		}
		blocks[i].ToolResult = &stub
	}
	if blocks == nil {
		return it
	}
	e := *it.Entry
	e.Blocks = blocks
	it.Entry = &e
	return it
}

func (w *ToolWindow) replayableCall(name string, args json.RawMessage) bool {
	return w.replayable != nil && w.replayable(name, args)
}
