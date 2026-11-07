package chat

import (
	"encoding/json"
	"unicode/utf8"

	"knomit/internal/engine/llm"
)

// toolLabel is how a tool's calls are worded for the person, taken from the
// tool's own definition so that changing a tool never needs a page change:
// its title (annotations.title) and the argument names, in order of
// preference, that hold its main argument (mcp.MetaKeyArgs).
type toolLabel struct {
	title string
	key   []string
}

// toolLabels indexes defs by name. A tool with neither a title nor a key is
// left out: the page shows such a call by its raw name.
func toolLabels(defs []llm.ToolDef) map[string]toolLabel {
	out := map[string]toolLabel{}
	for _, d := range defs {
		if d.Title != "" || len(d.Key) > 0 {
			out[d.Name] = toolLabel{title: d.Title, key: d.Key}
		}
	}
	return out
}

// maxKeyArg bounds the main argument a row is sent: the row shows one line,
// and a long query text would otherwise ride on every event and snapshot.
const maxKeyArg = 200

// wording is a call's title and main argument: the first of the tool's key
// argument names whose value is a non-empty string. Both are "" when the tool
// is unknown or marks nothing.
func (c *chat) wording(name string, args json.RawMessage) (title, keyArg string) {
	l, ok := c.labels[name]
	if !ok {
		return "", ""
	}
	if len(l.key) > 0 && len(args) > 0 {
		var m map[string]any
		if json.Unmarshal(args, &m) == nil {
			for _, k := range l.key {
				if s, ok := m[k].(string); ok && s != "" {
					keyArg = boundKeyArg(s)
					break
				}
			}
		}
	}
	return l.title, keyArg
}

// boundKeyArg cuts s to maxKeyArg bytes on a rune boundary, marking the cut.
func boundKeyArg(s string) string {
	if len(s) <= maxKeyArg {
		return s
	}
	cut := maxKeyArg
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// putWording adds a call's title and main argument to a tool_call event,
// each only when it has one.
func putWording(ev map[string]any, title, keyArg string) {
	if title != "" {
		ev["title"] = title
	}
	if keyArg != "" {
		ev["key_arg"] = keyArg
	}
}
