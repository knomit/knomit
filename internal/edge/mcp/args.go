package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// unmarshalArg extracts the named structured argument from req, re-marshals it
// via JSON, and decodes it into *target. Returns an error message suitable for
// mcpgo.NewToolResultError if the arg is missing or malformed.
//
// Usage:
//
//	var inputs []learnFactInput
//	if err := unmarshalArg(req, "facts", &inputs); err != nil {
//	    return mcpgo.NewToolResultError(err.Error()), nil
//	}
func unmarshalArg[T any](req mcpgo.CallToolRequest, key string, target *T) error {
	return decodeArg(req, key, target, false)
}

// unmarshalArgStrict is unmarshalArg that also refuses keys the target type
// does not declare. Use it where ignoring an unknown key would do something
// the caller did not ask for.
func unmarshalArgStrict[T any](req mcpgo.CallToolRequest, key string, target *T) error {
	return decodeArg(req, key, target, true)
}

func decodeArg[T any](req mcpgo.CallToolRequest, key string, target *T, strict bool) error {
	args := req.GetArguments()
	raw, ok := args[key]
	if !ok {
		return fmt.Errorf("%s is required", key)
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("invalid %s: %v", key, err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	if strict {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(target); err != nil {
		return fmt.Errorf("invalid %s format: %v", key, err)
	}
	return nil
}

// rejectNonObjectArguments fails a call whose `arguments` is present but is not
// a JSON object. Split out of rejectUnknownArguments because the binding gate
// runs BEFORE the handler and must answer this case identically rather than
// letting it read as a missing `binding`.
//
// GetArguments type-asserts to map[string]any and yields nil for ANYTHING else
// — including `arguments` that is present but is a string, number or array.
// Those two cases need opposite answers.
//
// Absent arguments is legitimate: calling with none is the documented way to
// start a session, and there is nothing to enumerate.
//
// Present-but-not-an-object is NOT. An earlier comment claimed "the ordinary
// per-argument accessors handle it" — they do not, they silently default, which
// is exactly the failure mode this check exists to close. Left unrejected, a
// caller sending the #121 payload as a JSON string reached an unscoped
// whole-corpus pass and advanced the watermark: the same consequence as an
// unknown key, by a second route. It is wire-reachable, because mcp-go
// unmarshals `arguments` into a bare `any` and does no schema validation on the
// server path.
func rejectNonObjectArguments(req mcpgo.CallToolRequest, tool mcpgo.Tool) error {
	if req.GetArguments() == nil && req.GetRawArguments() != nil {
		return fmt.Errorf("invalid arguments for %s: expected a JSON object, got %T",
			tool.Name, req.GetRawArguments())
	}
	return nil
}

// rejectUnknownArguments fails a call carrying an argument key the tool does
// not declare (knomit#122).
//
// WHY THIS IS AN ERROR AND NOT A WARNING. An MCP tool call is a JSON object
// with no arity check: a caller can invent a parameter, the server never reads
// it, and the call runs a DIFFERENT, VALID, SILENT operation. On knomit-kb that
// was `{"effort": "medium", "scope": "{\"entities\": [...]}"}` — a `scope` key,
// which knomit_review does not have, holding stringified JSON. Every such call
// ran as a whole-corpus incremental pass, and an unscoped completion advances
// the review watermark, so one malformed call turned a populated corpus into
// permanent sub-millisecond done:true walls (#121). A warning returned in a
// field the caller is not reading would be the same silence one level up.
//
// ORDER MATTERS. This runs BEFORE any per-argument type validation. The
// original proposal was to type-check the known keys, and it would not have
// caught this bug at all: the failing key is never read, so no amount of
// validating `domain` and `entities` sees it.
//
// The valid set is DERIVED FROM THE TOOL'S OWN SCHEMA rather than listed here.
// A hand-maintained list beside the schema is a second declaration of the same
// thing; the two drift the first time a parameter is added, and the failure
// mode is the guard rejecting the tool's own new parameter.
func rejectUnknownArguments(req mcpgo.CallToolRequest, tool mcpgo.Tool) error {
	args := req.GetArguments()
	if args == nil {
		return rejectNonObjectArguments(req, tool)
	}

	// Transport metadata is not a caller mistake. MCP reserves `_meta`, and
	// clients attach underscore-prefixed keys of their own accord; rejecting
	// those would break working clients to catch a bug they do not have.
	isTransport := func(key string) bool { return strings.HasPrefix(key, "_") }
	unknown, valid := undeclaredKeys(args, tool.InputSchema.Properties, isTransport)
	if len(unknown) == 0 {
		return nil
	}

	// The message names the offending keys AND the valid set: a caller that
	// invented a parameter cannot correct itself from "invalid arguments", and
	// the one that produced #121 would simply have re-sent the same call.
	return fmt.Errorf("unknown argument %s for %s; valid arguments are: %s",
		quotedList(unknown), tool.Name, strings.Join(valid, ", "))
}

// rejectUnknownObjectKeys is rejectUnknownArguments one level down: it fails a
// call whose object argument `arg` carries a key its schema does not declare.
// Same reason, same message shape — a key the handler never reads makes the
// call do something other than what the caller asked, and report success.
// There is no transport-metadata exemption here: nothing but the caller writes
// inside an argument. A non-object value is left to the argument's decoder,
// which reports it.
func rejectUnknownObjectKeys(req mcpgo.CallToolRequest, arg string, declared map[string]any, toolName string) error {
	obj, ok := req.GetArguments()[arg].(map[string]any)
	if !ok {
		return nil
	}
	unknown, valid := undeclaredKeys(obj, declared, nil)
	if len(unknown) == 0 {
		return nil
	}
	return fmt.Errorf("unknown key %s in %s for %s; valid keys are: %s",
		quotedList(unknown), arg, toolName, strings.Join(valid, ", "))
}

// rejectUnknownItemKeys is rejectUnknownObjectKeys for an ARRAY of objects
// (knomit_learn's `facts`): every element is checked against declared, and
// each offending element is reported by its index as "<item> N: unknown key
// ...", one line per element, followed by the valid set. Elements that are not
// objects are left to the argument's decoder.
func rejectUnknownItemKeys(req mcpgo.CallToolRequest, arg string, declared map[string]any, item string) error {
	items, ok := req.GetArguments()[arg].([]any)
	if !ok {
		return nil
	}
	var lines []string
	var valid []string
	for i, el := range items {
		obj, ok := el.(map[string]any)
		if !ok {
			continue
		}
		unknown, v := undeclaredKeys(obj, declared, nil)
		if len(unknown) == 0 {
			continue
		}
		valid = v
		lines = append(lines, fmt.Sprintf("%s %d: unknown key %s", item, i, quotedList(unknown)))
	}
	if len(lines) == 0 {
		return nil
	}
	return fmt.Errorf("%s; valid keys are: %s", strings.Join(lines, "\n"), strings.Join(valid, ", "))
}

// undeclaredKeys returns obj's keys that declared lacks, and declared's keys,
// both sorted so an error built from them is deterministic — a caller diffing
// two error strings should see a difference only when the calls differ. skip,
// when non-nil, exempts keys from the check. unknown is nil when every key is
// declared.
func undeclaredKeys(obj, declared map[string]any, skip func(string) bool) (unknown, valid []string) {
	for key := range obj {
		if skip != nil && skip(key) {
			continue
		}
		if _, ok := declared[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) == 0 {
		return nil, nil
	}
	sort.Strings(unknown)
	valid = make([]string, 0, len(declared))
	for key := range declared {
		valid = append(valid, key)
	}
	sort.Strings(valid)
	return unknown, valid
}

// quotedList renders names as `"a", "b"` for an error message.
func quotedList(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = strconv.Quote(n)
	}
	return strings.Join(quoted, ", ")
}
