package mcp

import (
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
	args := req.GetArguments()
	raw, ok := args[key]
	if !ok {
		return fmt.Errorf("%s is required", key)
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("invalid %s: %v", key, err)
	}
	if err := json.Unmarshal(b, target); err != nil {
		return fmt.Errorf("invalid %s format: %v", key, err)
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
		// Arguments were not a JSON object (GetArguments type-asserts and
		// yields nil otherwise). There is nothing to enumerate, so there is
		// nothing this check can say; the ordinary per-argument accessors
		// handle it. Refusing here would reject callers on the shape of their
		// transport rather than the content of their call.
		return nil
	}

	var unknown []string
	for key := range args {
		// Transport metadata is not a caller mistake. MCP reserves `_meta`,
		// and clients attach underscore-prefixed keys of their own accord;
		// rejecting those would break working clients to catch a bug they do
		// not have.
		if strings.HasPrefix(key, "_") {
			continue
		}
		if _, declared := tool.InputSchema.Properties[key]; !declared {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) == 0 {
		return nil
	}

	// Sorted so the message is deterministic — a caller diffing two error
	// strings should see a difference only when the calls differ.
	sort.Strings(unknown)
	valid := make([]string, 0, len(tool.InputSchema.Properties))
	for key := range tool.InputSchema.Properties {
		valid = append(valid, key)
	}
	sort.Strings(valid)

	// The message names the offending keys AND the valid set: a caller that
	// invented a parameter cannot correct itself from "invalid arguments", and
	// the one that produced #121 would simply have re-sent the same call.
	return fmt.Errorf("unknown argument %s for %s; valid arguments are: %s",
		quotedList(unknown), tool.Name, strings.Join(valid, ", "))
}

// quotedList renders names as `"a", "b"` for an error message.
func quotedList(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = strconv.Quote(n)
	}
	return strings.Join(quoted, ", ")
}
