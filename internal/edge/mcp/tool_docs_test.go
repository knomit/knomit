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
// mode changes, or a new tool, is checked without touching this test. Run per
// ENDPOINT KIND as well, because a URL-scoped endpoint is served different
// texts (the handle is optional there) and a different knomit_bind schema.
func TestBindingArg_DescriptionMatchesGateMode(t *testing.T) {
	regs := toolRegistrations(nil)
	gates := gateModes(regs)
	registered := make([]mcpgo.Tool, 0, len(regs))
	for _, r := range regs {
		registered = append(registered, r.tool)
	}

	// The tool list each endpoint kind is SERVED, through the same filter
	// tools/list runs.
	for _, endpoint := range []struct {
		name string
		ctx  context.Context
		kind endpointKind
	}{
		{"unscoped", repos.WithSessionScoped(context.Background()), endpointUnscoped},
		{"url-scoped, default branch", repos.WithMountSession(context.Background(), "sid", "repo:uid-x"), endpointScoped},
		{"url-scoped, names a branch", context.Background(), endpointFixed},
	} {
		t.Run(endpoint.name, func(t *testing.T) {
			require.Equal(t, endpoint.kind, endpointKindOf(endpoint.ctx))
			served := endpointToolFilter(gates)(endpoint.ctx, registered)
			seen := map[gateMode]int{}
			for _, tool := range served {
				mode := gates[tool.Name]
				seen[mode]++
				prop, declared := tool.InputSchema.Properties[bindingArgument]
				// Never schema-required: a URL-scoped endpoint may omit it, and
				// the unscoped one answers its absence with an actionable error.
				require.NotContains(t, tool.InputSchema.Required, bindingArgument, tool.Name)

				if mode == gateUngated {
					require.False(t, declared, "%s is ungated and must not declare `binding`", tool.Name)
					_, hasRepo := tool.InputSchema.Properties["repo"]
					_, hasLens := tool.InputSchema.Properties["lens"]
					_, hasExp := tool.InputSchema.Properties["experiment"]
					unscoped := endpoint.kind == endpointUnscoped
					require.True(t, hasExp, "%s: the branch selector is offered wherever bind is listed", tool.Name)
					require.Equal(t, unscoped, hasRepo, "%s: `repo` only where the repo can vary", tool.Name)
					require.Equal(t, unscoped, hasLens, "%s: `lens` only where the lens can vary", tool.Name)
					continue
				}
				require.True(t, declared, "%s is gated and must declare `binding`", tool.Name)
				desc, _ := prop.(map[string]any)["description"].(string)
				require.Equal(t, bindingDescription(mode, endpoint.kind), desc, tool.Name)
				lower := strings.ToLower(desc)

				switch {
				case endpoint.kind == endpointFixed:
					require.True(t, strings.HasPrefix(desc, "Not accepted on this endpoint"), desc)
					require.NotContains(t, desc, "knomit_bind", "no handle can be had here, so none is offered")
				case mode == gateRequired && endpoint.kind == endpointUnscoped:
					require.Contains(t, desc, "REQUIRED")
					require.NotContains(t, lower, "optional")
				case mode == gateRequired && endpoint.kind == endpointScoped:
					require.NotContains(t, desc, "REQUIRED")
					require.True(t, strings.HasPrefix(desc, "Optional"), desc)
					require.Contains(t, lower, "omit it", "the text must say what absence means")
					require.Contains(t, lower, "default branch")
					require.NotContains(t, lower, "connection to", "a handle is tied to the endpoint, not the connection")
				case mode == gateOptional:
					require.NotContains(t, desc, "REQUIRED")
					require.True(t, strings.HasPrefix(desc, "Optional"), desc)
					require.Contains(t, desc, "unresolvable",
						"a bad handle is reported, not raised, and the text must say so")
				default:
					t.Fatalf("%s: unknown gate mode %d", tool.Name, mode)
				}
				// No endpoint tells its callers about another's rules.
				require.NotContains(t, lower, "must not be passed", tool.Name)
			}
			require.Positive(t, seen[gateRequired])
			require.Positive(t, seen[gateOptional])
			if endpoint.kind == endpointFixed {
				// knomit_bind is refused on every call here, so it is not listed.
				require.Zero(t, seen[gateUngated], "knomit_bind must not be advertised where it always fails")
				require.Len(t, served, len(registered)-1)
			} else {
				require.Positive(t, seen[gateUngated])
				require.Len(t, served, len(registered))
			}
		})
	}

	// The filter never mutates the registered tools it was handed.
	for _, tool := range registered {
		if p, ok := tool.InputSchema.Properties[bindingArgument].(map[string]any); ok {
			require.Equal(t, bindingDescription(gates[tool.Name], endpointUnscoped), p["description"], tool.Name)
		}
	}
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
