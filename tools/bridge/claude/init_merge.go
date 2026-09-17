package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"

	"knomit/tools/bridge/knomitapi"
)

// Structural merging for the three files `claude init` does not own.
//
// The rule these merges follow is that the user's bytes are the user's: a merge
// is an INSERTION into the existing text (or a replacement of a region the
// integration delimited itself), never a re-emission of the whole document from
// a decoded structure. Re-emitting is semantically correct and practically
// hostile — it renumbers indentation, reorders keys and erases the shape the
// user reads every day, and it does so on files that are mostly theirs. That is
// why this file works in byte offsets rather than in decoded values.
//
// The other rule is that a merge init cannot do UNAMBIGUOUSLY does not happen at
// all: it falls back to the companion file, or, where a companion would be a
// silent failure, to a hard error.

// ---------------------------------------------------------------------------
// Hook identity
// ---------------------------------------------------------------------------

// hookIdentity reduces a settings.json hook command to what identifies the hook,
// so that re-running init does not register a second copy of one the user
// already has.
//
// The identity is the `claude hook <event>` suffix, NOT the command string.
// knomit-bridge is deliberately never placed on $PATH outside the macOS .app, so
// real configs invoke it by path — `${CLAUDE_PROJECT_DIR:-.}/dist/knomit-bridge`,
// `/usr/local/bin/knomit-bridge`, a `.exe` on Windows — and a string compare
// would call every one of those a different hook and add a duplicate that then
// runs the same hook twice per tool call. A duplicate is invisible in the diff
// of a long settings.json, so nothing would catch it afterwards.
//
// The first field must still name the bridge: an unrelated tool that happens to
// take `claude hook post-edit` arguments is NOT our hook, and merging into it
// would hide the user's own command behind ours.
func hookIdentity(command string) string {
	fields := strings.Fields(command)
	if len(fields) > 0 && knomitapi.IsKnomitCommand(fields[0]) {
		for i := 1; i+2 < len(fields); i++ {
			if fields[i] == "claude" && fields[i+1] == "hook" {
				return "knomit-bridge claude hook " + fields[i+2]
			}
		}
	}
	return strings.Join(fields, " ")
}

// hookEventName is the short name a summary line uses for a hook — the event
// the subcommand implements ("memory-guard"), not the whole command line.
func hookEventName(identity string) string {
	fields := strings.Fields(identity)
	if len(fields) > 0 {
		return fields[len(fields)-1]
	}
	return identity
}

// ---------------------------------------------------------------------------
// A positional index over a JSON document
// ---------------------------------------------------------------------------

// jsonSpan locates a value inside the document it was scanned from: open and
// close are the byte offsets of the delimiters of a container, or -1 for a
// scalar, which this file never needs to splice.
type jsonSpan struct {
	open, close int
	empty       bool
}

// jsonNode is one value of an indexed document. Only object members are kept by
// name — array elements are recovered from the array's own bytes when needed,
// which keeps the index small and its offsets few enough to reason about.
type jsonNode struct {
	span     jsonSpan
	isObject bool
	isArray  bool
	order    []string
	children map[string]*jsonNode
}

// object and array are the only safe way to ask what a node is before splicing
// into it, and every splice site must ask. The predicate that suggests itself —
// "does this value have delimiters?", i.e. open >= 0 — is NOT a type check, and
// guarding with it produced four defects at once: an object and an array both
// answer yes, so an object member spliced into an array (or an array element
// into an object) emits JSON that does not parse; and a scalar answers no with
// offsets of -1, which a caller reading "not a container" as "absent" then
// splices at, either panicking or appending a duplicate key beside the value it
// failed to recognise. That last one parses, so it is the quiet one. The weak
// predicate is deliberately not defined here — see the json-splicing gotcha.
func (n *jsonNode) object() bool { return n != nil && n.isObject }
func (n *jsonNode) array() bool  { return n != nil && n.isArray }

// child returns the named member, or nil. It is nil-safe so callers can walk a
// path through a document that may not have it.
func (n *jsonNode) child(name string) *jsonNode {
	if n == nil || !n.isObject {
		return nil
	}
	return n.children[name]
}

// indexJSON parses data for structure and POSITION. It rejects anything the
// standard decoder rejects, plus trailing content after the top-level value —
// a file with a stray second object is one a caller must not splice into.
func indexJSON(data []byte) (*jsonNode, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	root, err := indexValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("unexpected content after the top-level value")
	}
	return root, nil
}

func indexValue(dec *json.Decoder) (*jsonNode, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	delim, isDelim := tok.(json.Delim)
	if !isDelim {
		return &jsonNode{span: jsonSpan{open: -1, close: -1}}, nil
	}
	start := int(dec.InputOffset()) - 1
	switch delim {
	case '{':
		n := &jsonNode{isObject: true, children: map[string]*jsonNode{}}
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyTok.(string)
			if !ok {
				return nil, fmt.Errorf("object key is not a string")
			}
			value, err := indexValue(dec)
			if err != nil {
				return nil, err
			}
			if _, dup := n.children[key]; !dup {
				n.order = append(n.order, key)
			}
			n.children[key] = value
		}
		if _, err := dec.Token(); err != nil { // the closing brace
			return nil, err
		}
		n.span = jsonSpan{open: start, close: int(dec.InputOffset()) - 1, empty: len(n.order) == 0}
		return n, nil
	case '[':
		count := 0
		for dec.More() {
			if _, err := indexValue(dec); err != nil {
				return nil, err
			}
			count++
		}
		if _, err := dec.Token(); err != nil { // the closing bracket
			return nil, err
		}
		return &jsonNode{isArray: true, span: jsonSpan{open: start, close: int(dec.InputOffset()) - 1, empty: count == 0}}, nil
	}
	return nil, fmt.Errorf("unexpected delimiter %v", delim)
}

// elements returns an array's elements as their own raw bytes.
func elements(data []byte, span jsonSpan) ([]json.RawMessage, error) {
	var out []json.RawMessage
	if err := json.Unmarshal(data[span.open:span.close+1], &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Byte-level splicing
// ---------------------------------------------------------------------------

// indentUnit guesses the document's indentation step from its first indented
// line, so an inserted member looks like it belongs. Two spaces when the file
// offers no evidence — every file this tool writes uses two.
func indentUnit(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimLeft(line, " \t")
		if trimmed == "" || len(trimmed) == len(line) {
			continue
		}
		return line[:len(line)-len(trimmed)]
	}
	return "  "
}

// lineIndent returns the whitespace the line containing off begins with.
func lineIndent(data []byte, off int) string {
	start := bytes.LastIndexByte(data[:off], '\n') + 1
	line := string(data[start:off])
	return line[:len(line)-len(strings.TrimLeft(line, " \t"))]
}

// lastContentByte is the offset of the last non-whitespace byte strictly inside
// a container — the point a new member is appended after, so the container's own
// closing delimiter keeps the line and indentation it already had.
func lastContentByte(data []byte, span jsonSpan) int {
	for i := span.close - 1; i > span.open; i-- {
		switch data[i] {
		case ' ', '\t', '\n', '\r':
		default:
			return i
		}
	}
	return span.open
}

// reindent re-renders a value so its first line starts where the caller puts it
// and every later line sits at prefix. The template's entries are written on one
// line; a project's settings.json usually is not, and matching the destination
// is what keeps the merged file readable.
func reindent(raw []byte, prefix, unit string) (string, error) {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, prefix, unit); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// insertMember splices a new `"key": value` into an object.
func insertMember(data []byte, span jsonSpan, key string, value []byte) ([]byte, error) {
	unit := indentUnit(data)
	memberIndent := lineIndent(data, span.close) + unit
	rendered, err := reindent(value, memberIndent, unit)
	if err != nil {
		return nil, err
	}
	keyJSON, err := json.Marshal(key)
	if err != nil {
		return nil, err
	}
	member := string(keyJSON) + ": " + rendered
	if span.empty {
		return splice(data, span.open+1, span.open+1,
			"\n"+memberIndent+member+"\n"+lineIndent(data, span.close)), nil
	}
	at := lastContentByte(data, span) + 1
	return splice(data, at, at, ",\n"+memberIndent+member), nil
}

// insertElement splices a new element onto the end of an array.
func insertElement(data []byte, span jsonSpan, value []byte) ([]byte, error) {
	unit := indentUnit(data)
	elemIndent := lineIndent(data, span.close) + unit
	rendered, err := reindent(value, elemIndent, unit)
	if err != nil {
		return nil, err
	}
	if span.empty {
		return splice(data, span.open+1, span.open+1,
			"\n"+elemIndent+rendered+"\n"+lineIndent(data, span.close)), nil
	}
	at := lastContentByte(data, span) + 1
	return splice(data, at, at, ",\n"+elemIndent+rendered), nil
}

// replaceValue swaps a container's bytes for a freshly rendered value, keeping
// its surroundings untouched.
//
// A value the file kept on ONE line comes back on one line. Reflowing it is the
// same formatting churn this file exists to avoid, merely confined to the region
// init is allowed to rewrite — and `"args": ["--lens", "eng"]` becoming three
// lines is a diff the user did not ask for.
func replaceValue(data []byte, span jsonSpan, value []byte) ([]byte, error) {
	if !bytes.ContainsRune(data[span.open:span.close+1], '\n') {
		rendered, err := oneLine(value)
		if err != nil {
			return nil, err
		}
		return splice(data, span.open, span.close+1, rendered), nil
	}
	unit := indentUnit(data)
	rendered, err := reindent(value, lineIndent(data, span.open), unit)
	if err != nil {
		return nil, err
	}
	return splice(data, span.open, span.close+1, rendered), nil
}

// oneLine renders a value on a single line, keeping the source spelling when the
// source already is one line — the templates write `["--lens", "eng"]`, and
// json.Compact would strip the space after the comma for no reason.
func oneLine(value []byte) (string, error) {
	if !bytes.ContainsRune(value, '\n') {
		return string(bytes.TrimSpace(value)), nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, value); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func splice(data []byte, from, to int, text string) []byte {
	out := make([]byte, 0, len(data)+len(text))
	out = append(out, data[:from]...)
	out = append(out, text...)
	return append(out, data[to:]...)
}

// ---------------------------------------------------------------------------
// .claude/settings.json
// ---------------------------------------------------------------------------

// settingsHooks is the shape of one hook registration, used only to read the
// command strings out of an entry. The entry is INSERTED from its own raw bytes,
// never re-encoded from this struct, so a field the template gains later is
// carried across even though this struct does not know about it.
type settingsHooks struct {
	Hooks []struct {
		Command string `json:"command"`
	} `json:"hooks"`
}

func hookIdentitiesIn(entry json.RawMessage) []string {
	var parsed settingsHooks
	if err := json.Unmarshal(entry, &parsed); err != nil {
		return nil
	}
	ids := make([]string, 0, len(parsed.Hooks))
	for _, h := range parsed.Hooks {
		ids = append(ids, hookIdentity(h.Command))
	}
	return ids
}

// checkSettingsShape reports whether a settings.json has the shape the merge
// splices into: an object at the root, a `hooks` object if the key is present,
// and an array for each hook event. Anything else is a file init must decline
// rather than splice — see the failure modes on jsonNode.object.
func checkSettingsShape(data []byte) error {
	root, err := indexJSON(data)
	if err != nil {
		return err
	}
	if !root.object() {
		return fmt.Errorf("its top-level value is not a JSON object")
	}
	hooks := root.child("hooks")
	if hooks == nil {
		return nil
	}
	if !hooks.object() {
		return fmt.Errorf("its \"hooks\" value is not a JSON object")
	}
	for _, event := range hooks.order {
		if !hooks.child(event).array() {
			return fmt.Errorf("its hook event %q is not a JSON array", event)
		}
	}
	return nil
}

// mergeSettingsJSON adds the template's hook registrations to an existing
// settings.json and returns the merged bytes plus a description of each hook
// added. It returns the existing bytes unchanged, and no additions, when every
// template hook is already registered.
//
// Idempotence is per hook COMMAND and scoped to the event, not to the template's
// matcher: a user who put post-edit behind their own matcher HAS post-edit, and
// giving them ours as well would run it twice on every edit.
func mergeSettingsJSON(existing, template []byte) ([]byte, []string, error) {
	// A non-object root has span offsets of -1 and would be spliced at them.
	// preflightSettings rejects that before init writes anything; this is the
	// same guard at the point of use, so the function is safe on its own terms.
	if err := checkSettingsShape(existing); err != nil {
		return nil, nil, err
	}

	tmplRoot, err := indexJSON(template)
	if err != nil {
		// The template is //go:embed-bundled and author-controlled, so this is a
		// build-time bug rather than a condition to recover from.
		return nil, nil, fmt.Errorf("settings template does not parse: %w", err)
	}
	tmplHooks := tmplRoot.child("hooks")
	if tmplHooks == nil {
		return existing, nil, nil
	}

	merged := existing
	var added []string
	for _, event := range tmplHooks.order {
		entries, err := elements(template, tmplHooks.child(event).span)
		if err != nil {
			return nil, nil, fmt.Errorf("settings template event %q: %w", event, err)
		}
		for _, entry := range entries {
			// Insertion is per template ENTRY, and allPresent requires EVERY hook
			// in the entry to be registered before skipping it. An entry carrying
			// two hooks, one of them already present, would therefore re-insert
			// the present one alongside the missing one. Today's template gives
			// each entry exactly one hook, so the case is unreachable; splitting
			// an entry per hook would change the matcher the user sees, so it
			// wants a decision rather than a quiet fix.
			ids := hookIdentitiesIn(entry)
			// Re-index every round: each splice moves the offsets after it, and
			// recomputing them is far cheaper to get right than adjusting them.
			root, err := indexJSON(merged)
			if err != nil {
				return nil, nil, err
			}
			present, err := registeredHooks(merged, root, event)
			if err != nil {
				return nil, nil, err
			}
			if allPresent(ids, present) {
				continue
			}
			merged, err = insertHookEntry(merged, root, event, entry)
			if err != nil {
				return nil, nil, err
			}
			for _, id := range ids {
				if !present[id] {
					added = append(added, event+" "+hookEventName(id))
				}
			}
		}
	}
	return merged, added, nil
}

// registeredHooks collects the identity of every hook already registered under
// an event, wherever in that event it sits.
func registeredHooks(data []byte, root *jsonNode, event string) (map[string]bool, error) {
	present := map[string]bool{}
	node := root.child("hooks").child(event)
	if !node.array() {
		return present, nil
	}
	entries, err := elements(data, node.span)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		for _, id := range hookIdentitiesIn(entry) {
			present[id] = true
		}
	}
	return present, nil
}

func allPresent(ids []string, present map[string]bool) bool {
	if len(ids) == 0 {
		return true
	}
	for _, id := range ids {
		if !present[id] {
			return false
		}
	}
	return true
}

// insertHookEntry places one template entry under an event, creating the event
// array — and the `hooks` object itself — if the file has neither.
func insertHookEntry(data []byte, root *jsonNode, event string, entry json.RawMessage) ([]byte, error) {
	hooks := root.child("hooks")
	if hooks == nil {
		value := append(append([]byte(`{`+jsonStr(event)+`:[`), entry...), `]}`...)
		return insertMember(data, root.span, "hooks", value)
	}
	if !hooks.object() {
		// Never reached via runInit (preflightSettings rejects it), but a caller
		// that skipped the preflight must not get a second "hooks" key spliced in
		// beside this one.
		return nil, fmt.Errorf("\"hooks\" is not a JSON object")
	}
	eventNode := hooks.child(event)
	if eventNode == nil {
		value := append(append([]byte(`[`), entry...), ']')
		return insertMember(data, hooks.span, event, value)
	}
	if !eventNode.array() {
		return nil, fmt.Errorf("hook event %q is not a JSON array", event)
	}
	return insertElement(data, eventNode.span, entry)
}

// ---------------------------------------------------------------------------
// .mcp.json
// ---------------------------------------------------------------------------

// mergeMcpJSON brings the entry under the derived server key up to date, adding
// it if the project has no knomit entry at all.
//
// An existing entry is split into the half init owns and the half the user owns.
// `args` carry the SCOPE, which init derived, so init refreshes them. `command`
// is DEPLOYMENT-specific and stays exactly as written: knomit-bridge is
// deliberately never on $PATH outside the macOS .app, so a checkout pointing at
// its own build (`${CLAUDE_PROJECT_DIR:-.}/dist/knomit-bridge`) does so on
// purpose, and resetting it to the bare name silently stops the MCP server
// loading. Every other key the user set is preserved for the same reason.
//
// conflict is true for the one case a merge cannot decide: a knomit-bridge entry
// under a DIFFERENT key. Adding ours beside it would leave the project with two
// knomit scopes, which is not a merge artefact but a configuration that disables
// every hook — mcpBinding has no principled answer to "which repo do the hooks
// bind to?" and stands down rather than guess. Only a human can say which scope
// was meant, so that case keeps the companion and its warning.
func mergeMcpJSON(existing, template []byte, key string) (merged []byte, note string, conflict bool, err error) {
	tmplRoot, err := indexJSON(template)
	if err != nil {
		return nil, "", false, fmt.Errorf("mcp template does not parse: %w", err)
	}
	tmplEntry := tmplRoot.child("mcpServers").child(key)
	if !tmplEntry.object() {
		return nil, "", false, fmt.Errorf("mcp template has no %q entry", key)
	}
	value := template[tmplEntry.span.open : tmplEntry.span.close+1]

	root, err := indexJSON(existing)
	if err != nil || !root.object() {
		// Unparseable, or not an object: nothing to splice into. The companion is
		// the honest outcome, and unlike settings.json it is not a silent failure
		// — a broken .mcp.json means the MCP server is already not loading.
		return nil, "", true, nil
	}

	servers := root.child("mcpServers")
	if servers == nil {
		merged, err := insertMember(existing, root.span, "mcpServers",
			append(append([]byte(`{`+jsonStr(key)+`:`), value...), '}'))
		return merged, "(+" + key + ")", false, err
	}
	if !servers.object() {
		// "mcpServers" holding an array or a scalar is not a config init can add
		// a server to — splicing a member in would emit JSON that does not parse,
		// or a duplicate key beside it.
		return nil, "", true, nil
	}

	if entry := servers.child(key); entry != nil {
		if !entry.object() {
			return nil, "", true, nil
		}
		tmplArgs := tmplEntry.child("args")
		if !tmplArgs.array() {
			return nil, "", false, fmt.Errorf("mcp template entry %q has no args array", key)
		}
		args := template[tmplArgs.span.open : tmplArgs.span.close+1]

		existingArgs := entry.child("args")
		if existingArgs == nil {
			merged, err := insertMember(existing, entry.span, "args", args)
			return merged, "(" + key + " args)", false, err
		}
		if !existingArgs.array() {
			return nil, "", true, nil
		}
		if sameJSON(existing[existingArgs.span.open:existingArgs.span.close+1], args) {
			// Already naming the right scope. Rewriting it would churn the file
			// and report work that was not done.
			return existing, "", false, nil
		}
		merged, err := replaceValue(existing, existingArgs.span, args)
		return merged, "(" + key + " args)", false, err
	}

	// A knomit-bridge entry under another key is the two-scopes case.
	if otherKnomitServer(existing, servers) {
		return nil, "", true, nil
	}
	merged, err = insertMember(existing, servers.span, key, value)
	return merged, "(+" + key + ")", false, err
}

// otherKnomitServer reports whether any entry looks like a knomit bridge. Both
// signals count here, unlike in mcpBinding: this is not a question of which
// server to bind to but of whether a human should look before init adds a second
// scope, and a false positive costs only a companion file.
func otherKnomitServer(data []byte, servers *jsonNode) bool {
	for _, key := range servers.order {
		entry := servers.child(key)
		command := ""
		if entry.object() {
			var parsed struct {
				Command string `json:"command"`
			}
			if err := json.Unmarshal(data[entry.span.open:entry.span.close+1], &parsed); err == nil {
				command = parsed.Command
			}
		}
		if isKnomitServer(key, command) {
			return true
		}
	}
	return false
}

func sameJSON(a, b []byte) bool {
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

// ---------------------------------------------------------------------------
// CLAUDE.md
// ---------------------------------------------------------------------------

// blockMarkerClose ends the delimited region. Its presence is what makes an
// in-place replacement possible: without it init knows where the block starts
// but not where it stops, and a guess either eats the user's prose or leaves
// half a stale block behind.
const blockMarkerClose = "<!-- /knomit:integration -->"

type claudeMdState int

const (
	// blockCurrent: the marker this build ships is already there.
	blockCurrent claudeMdState = iota
	// blockOlder: a delimited block from an older version — replaceable.
	blockOlder
	// blockUndelimited: a block identified only by its heading, or an opening
	// marker with no closing one. Recognisable but not bounded.
	blockUndelimited
	// blockAbsent: no knomit block at all.
	blockAbsent
)

// classifyClaudeMd locates the knomit block in content. For blockOlder, start
// and end bound the region to replace (end is exclusive and includes the
// newline after the closing marker, if any) and version names the marker found.
func classifyClaudeMd(content string) (state claudeMdState, version string, start, end int) {
	if blockMarkerCurrent != "" && strings.Contains(content, blockMarkerCurrent) {
		return blockCurrent, "", 0, 0
	}
	if open := strings.Index(content, blockMarkerPrefix); open >= 0 {
		start = strings.LastIndexByte(content[:open], '\n') + 1
		if closeAt := strings.Index(content[open:], blockMarkerClose); closeAt >= 0 {
			end = open + closeAt + len(blockMarkerClose)
			if end < len(content) && content[end] == '\n' {
				end++
			}
			markerLine, _, _ := strings.Cut(content[open:], "\n")
			return blockOlder, blockVersion(markerLine), start, end
		}
		return blockUndelimited, "", 0, 0
	}
	// The template shipped without the HTML markers until c36015e7, and users
	// strip comments. Treating such a block as absent would append a SECOND copy.
	if strings.Contains(content, blockHeading) {
		return blockUndelimited, "", 0, 0
	}
	return blockAbsent, "", 0, 0
}

// blockVersion pulls the version token out of a marker line, e.g. "v3" from
// "<!-- knomit:integration v3 -->". Empty when the marker carries none.
func blockVersion(markerLine string) string {
	fields := strings.Fields(strings.TrimSpace(markerLine))
	for i, f := range fields {
		if f == strings.TrimPrefix(blockMarkerPrefix, "<!-- ") && i+1 < len(fields) {
			if next := fields[i+1]; next != "-->" {
				return next
			}
		}
	}
	return ""
}

// mergeClaudeMd brings the knomit block in an existing CLAUDE.md up to date.
//
// The marker exists precisely so the block can be REPLACED rather than merged by
// hand: hand-merging is what let installed blocks drift to "Nine /knomit-…
// slash commands" against a template saying eleven. Everything outside the
// delimited region is left byte-for-byte alone — it is the user's file.
func mergeClaudeMd(existing, block []byte) (merged []byte, note string, conflict bool) {
	content := string(existing)
	switch state, version, start, end := classifyClaudeMd(content); state {
	case blockCurrent:
		return existing, "", false
	case blockOlder:
		to := blockVersion(blockMarkerCurrent)
		note = "knomit block " + version + " → " + to
		if version == "" || to == "" {
			note = "knomit block replaced"
		}
		return []byte(content[:start] + string(block) + content[end:]), note, false
	case blockAbsent:
		sep := "\n\n"
		if content == "" {
			sep = ""
		} else if strings.HasSuffix(content, "\n") {
			sep = "\n"
		}
		return []byte(content + sep + string(block)), "(+knomit block)", false
	default: // blockUndelimited
		return existing, "", true
	}
}
