package mcp

import (
	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// MetaKeyArgs is the tool _meta key that marks a tool's MAIN ARGUMENT: the
// argument names, in order of preference, whose first non-empty string value
// a person is shown beside the tool's title (a chat row reads
// "<title> · <main argument>", e.g. "Query · approval gate").
//
// It lives in _meta, not in the input schema, on purpose: the schema is what
// the model is sent (the chat copies it into the cached tool prefix, and every
// external MCP client hands it to its model), so a marker there would change
// the prompt and break the prefix cache of every chat for a word on a screen.
// _meta is outside the schema and no adapter sends it. The value is a JSON
// array of strings. Keys follow MCP's reverse-DNS _meta prefix convention.
const MetaKeyArgs = "io.knomit/key"

// withKeyArgs marks names as the tool's main argument (see MetaKeyArgs).
func withKeyArgs(names ...string) mcpgo.ToolOption {
	return func(t *mcpgo.Tool) {
		vals := make([]any, len(names))
		for i, n := range names {
			vals[i] = n
		}
		t.Meta = mcpgo.NewMetaFromMap(map[string]any{MetaKeyArgs: vals})
	}
}

// toolKeyArgs reads a tool's MetaKeyArgs, as set here or as it arrived over
// the wire (where the array decodes as []any). Anything that is not a string
// is skipped; no marker is nil.
func toolKeyArgs(t mcpgo.Tool) []string {
	if t.Meta == nil {
		return nil
	}
	return keyArgsOf(t.Meta.AdditionalFields[MetaKeyArgs])
}

func keyArgsOf(v any) []string {
	var out []string
	switch vs := v.(type) {
	case []string:
		out = append(out, vs...)
	case []any:
		for _, x := range vs {
			if s, ok := x.(string); ok && s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}
