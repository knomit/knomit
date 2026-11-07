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

// wording is what the page is told about a call: its title, its main argument
// (the first of the tool's key argument names whose value is a non-empty
// string), the repo or lens its arguments name (callTarget) and whether it
// acts in one (inBase). Title and main argument are "" when the tool is
// unknown or marks nothing.
func (c *chat) wording(name string, args json.RawMessage) callWording {
	w := callWording{target: callTarget(args), inBase: inBase(args)}
	l, ok := c.labels[name]
	if !ok {
		return w
	}
	w.title = l.title
	if len(l.key) > 0 && len(args) > 0 {
		var m map[string]any
		if json.Unmarshal(args, &m) == nil {
			for _, k := range l.key {
				if s, ok := m[k].(string); ok && s != "" {
					w.keyArg = boundKeyArg(s)
					break
				}
			}
		}
	}
	return w
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

// inBase reports whether a call acts in a repo or lens: its arguments carry
// binding, repo or lens. The page words such a call "in <target>". It is
// decided here, from the whole arguments, because the page does not always
// have them: the running event drops arguments over maxApprovalArgs, so a
// page that decided from arguments would word a large call one way live and
// another after a reload.
func inBase(args json.RawMessage) bool {
	var m map[string]json.RawMessage
	if len(args) == 0 || json.Unmarshal(args, &m) != nil {
		return false
	}
	for _, k := range []string{"binding", "repo", "lens"} {
		if _, ok := m[k]; ok {
			return true
		}
	}
	return false
}

// callTarget is the repo or lens a call's arguments name explicitly (lens
// before repo), cut like a main argument; "" when they name neither. Sent for
// the same reason as inBase: the page must not read it from arguments it may
// not have.
func callTarget(args json.RawMessage) string {
	var m map[string]any
	if len(args) == 0 || json.Unmarshal(args, &m) != nil {
		return ""
	}
	for _, k := range []string{"lens", "repo"} {
		if s, ok := m[k].(string); ok && s != "" {
			return boundKeyArg(s)
		}
	}
	return ""
}

// callWording is what a tool_call event, row and record say about a call
// beyond its name and state.
type callWording struct {
	title, keyArg, target string
	inBase                bool
}

// putWording adds a call's title, main argument, target and in_base to a
// tool_call event, each only when it has one.
func putWording(ev map[string]any, w callWording) {
	if w.title != "" {
		ev["title"] = w.title
	}
	if w.keyArg != "" {
		ev["key_arg"] = w.keyArg
	}
	if w.target != "" {
		ev["target"] = w.target
	}
	if w.inBase {
		ev["in_base"] = true
	}
}
