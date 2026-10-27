package mcp

import (
	"context"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
)

// The `binding` description is what an agent reads to decide whether it must
// pass a handle, so it has to say what the gate does. knomit_repos once served
// "Optional. ... REQUIRED on the unscoped /api/v1/mcp endpoint ..." — both at
// once — because the optional text was the required text with a prefix.
//
// Derived from the registration table, not from tool names: a tool whose gate
// mode changes, or a new tool, is checked without touching this test.
func TestBindingArg_DescriptionMatchesGateMode(t *testing.T) {
	regs := toolRegistrations(nil)
	seen := map[gateMode]int{}
	for _, r := range regs {
		seen[r.gate]++
		prop, declared := r.tool.InputSchema.Properties[bindingArgument]
		// The handle is never schema-required: a URL-scoped endpoint refuses it.
		require.NotContains(t, r.tool.InputSchema.Required, bindingArgument,
			"%s: `binding` must not be schema-required — a URL-scoped endpoint refuses it", r.tool.Name)

		if r.gate == gateUngated {
			require.False(t, declared, "%s is ungated and must not declare `binding`", r.tool.Name)
			continue
		}
		require.True(t, declared, "%s is gated and must declare `binding`", r.tool.Name)
		desc, _ := prop.(map[string]any)["description"].(string)
		require.NotEmpty(t, desc, "%s: `binding` has no description", r.tool.Name)

		// Every gated tool is refused a handle on a URL-scoped endpoint
		// (gateBinding checks presence before the mode), so every text says so.
		require.Contains(t, strings.ToLower(desc), "must not be passed on a url-scoped endpoint", r.tool.Name)

		switch r.gate {
		case gateRequired:
			require.Contains(t, desc, "REQUIRED", "%s is gateRequired: its `binding` must say REQUIRED", r.tool.Name)
			require.NotContains(t, strings.ToLower(desc), "optional",
				"%s is gateRequired: its `binding` must not call itself optional", r.tool.Name)
		case gateOptional:
			require.NotContains(t, desc, "REQUIRED",
				"%s is gateOptional (a missing handle passes): its `binding` must not say REQUIRED", r.tool.Name)
			require.True(t, strings.HasPrefix(desc, "Optional"),
				"%s is gateOptional: its `binding` must open with Optional: %q", r.tool.Name, desc)
			require.Contains(t, desc, "unresolvable",
				"%s is gateOptional: a bad handle is reported, not raised, and the text must say so", r.tool.Name)
		default:
			t.Fatalf("%s: unknown gate mode %d", r.tool.Name, r.gate)
		}
	}
	// The table must exercise every mode, or a branch above is vacuous.
	require.Positive(t, seen[gateRequired])
	require.Positive(t, seen[gateOptional])
	require.Positive(t, seen[gateUngated])
}

// Every tool refuses an argument its schema does not declare, and names it
// (knomit#122). query, explain, changes, update and retract used to ignore
// one and run as if it were absent, while every other tool refused it.
func TestEveryTool_RefusesAnUndeclaredArgument(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	onAgent := repos.WithBranch(repos.WithRepoInstance(context.Background(), ri), "agent/test")

	const bogus = "not_a_parameter"
	for _, r := range toolRegistrations(nil) {
		t.Run(r.tool.Name, func(t *testing.T) {
			_, clash := r.tool.InputSchema.Properties[bogus]
			require.False(t, clash, "the probe key must be undeclared")

			var req mcpgo.CallToolRequest
			req.Params.Name = r.tool.Name
			req.Params.Arguments = map[string]any{bogus: "x"}
			res, err := r.handler(onAgent, req)
			require.NoError(t, err)
			require.NotNil(t, res)
			require.True(t, res.IsError, "%s accepted an undeclared argument", r.tool.Name)
			text := resultText(t, res)
			require.Contains(t, text, `unknown argument "`+bogus+`"`,
				"%s must refuse the undeclared key by name, got: %s", r.tool.Name, text)
		})
	}
}

// queryFilterList is what the knomit_query description and its no-filter
// error tell a caller counts as a filter. Each named argument, given alone,
// must satisfy hasAnyFilter — or the text promises a call the handler refuses.
func TestQueryFilterList_EveryNameIsAFilter(t *testing.T) {
	values := map[string]any{
		"text": "x", "entities": []any{"x"}, "domain": []any{"x"}, "applies_to": []any{"x"},
		"path": "kb/x", "type": []any{"observation"}, "origin": "authored", "motifs": []any{"x-y"},
		"context": map[string]any{"task": "t-1"}, "min_confidence": 0.5, "expired": true,
		"expires_before": "2026-01-01T00:00:00Z", "expires_after": "2026-01-01T00:00:00Z",
	}
	names := strings.FieldsFunc(queryFilterList, func(r rune) bool { return r == ',' || r == ' ' })
	var checked int
	for _, n := range names {
		if n == "or" {
			continue
		}
		checked++
		v, ok := values[n]
		require.True(t, ok, "queryFilterList names %q, which this test has no value for", n)
		_, declared := queryTool().InputSchema.Properties[n]
		require.True(t, declared, "queryFilterList names %q, which knomit_query does not declare", n)
		var req mcpgo.CallToolRequest
		req.Params.Arguments = map[string]any{n: v}
		q, err := parseQueryFilters(req)
		require.NoError(t, err, n)
		require.True(t, hasAnyFilter(q), "%q alone is listed as a filter but hasAnyFilter rejects it", n)
	}
	require.Equal(t, len(values), checked, "every filter in values must be named in queryFilterList")
}
