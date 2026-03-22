package mcp

import (
	"encoding/json"
	"fmt"

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
