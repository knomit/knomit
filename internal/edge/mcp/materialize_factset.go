package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/rs/zerolog/log"

	"knomit/internal/corpus/materialize"
	"knomit/internal/corpus/repos"
)

// FactSet mode of knomit_materialize: `search` instead of `file` materializes
// that search as a factset (materialize.FactSetFirstPage), and a factset cursor pages
// it (materialize.FactSetNextPage). The pages are byte for byte the REST
// GET …/search/factset pages for the same input: one shared builder.

// factsetSearchKeys are the keys of the `search` object.
var factsetSearchKeys = map[string]any{
	"text": map[string]any{"type": "string", "description": "Free text, ranked by similarity."},
	"entities": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
		"description": "Entities, all must be present."},
	"domain": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
		"description": "Domain tags, all must match."},
	"domain_exact": map[string]any{"type": "boolean", "description": "Match domain tags exactly."},
	"path":         map[string]any{"type": "string", "description": "Path prefix."},
	"type": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
		"description": "Only these fact types."},
	"exclude_type":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
	"kind":           map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
	"exclude_kind":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
	"origin":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
	"ep":             map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Episode operations."},
	"motifs":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
	"motif_match":    map[string]any{"type": "string", "enum": []string{"exact", "stem", "token-2"}},
	"min_confidence": map[string]any{"type": "number"},
	"min_similarity": map[string]any{"type": "number"},
	"sort": map[string]any{"type": "string", "enum": []string{materialize.SortRelevance, materialize.SortRecent},
		"description": "relevance (default): the search list; recent: newest first."},
}

// factsetOnlyArgs may be passed only in factset mode.
var factsetOnlyArgs = []string{"search", "facts"}

// isFactSetFirst reports whether a first call is in factset mode.
func isFactSetFirst(req mcpgo.CallToolRequest) bool {
	_, ok := req.GetArguments()["search"]
	return ok
}

// materializeFactSetFirst builds the factset of the `search` argument and returns
// page 1.
func materializeFactSetFirst(ctx context.Context, req mcpgo.CallToolRequest, b *repos.Binding, mgr *repos.Manager, sWrite mcpStore) (*mcpgo.CallToolResult, error) {
	args := req.GetArguments()
	if _, ok := args["file"]; ok {
		return mcpgo.NewToolResultError("pass `file` (one fact) or `search` (a factset), not both"), nil
	}
	if a, ok := args["audience"]; ok && a != audienceHuman {
		return mcpgo.NewToolResultError(`factset mode serves the human pages: audience must be "human" or omitted`), nil
	}
	if err := rejectUnknownObjectKeys(req, "search", factsetSearchKeys, "knomit_materialize"); err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	search, err := factsetSearchArg(args["search"])
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	opts := materialize.DefaultOptions()
	for _, a := range []struct {
		name string
		dst  *int
	}{{"depth", &opts.Depth}, {"max_nodes", &opts.MaxNodes}} {
		v, present, err := intArgument(req, a.name)
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		if present {
			*a.dst = v
		}
	}
	opts.Unpin = req.GetBool("unpin", false)
	opts.CrossKB = req.GetBool("cross_kb", false)
	breq := materialize.FactSetRequest{
		Binding: b, Manager: mgr, MayRead: callerMayRead(ctx), Search: search, Options: opts,
		Facts: materialize.DefaultFactSetFacts, Limit: materialize.HumanDefaultLimit,
	}
	for _, a := range []struct {
		name string
		dst  *int
	}{{"facts", &breq.Facts}, {"limit", &breq.Limit}} {
		v, present, err := intArgument(req, a.name)
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		if present {
			*a.dst = v
		}
	}
	if c := req.GetString("commit", ""); c != "" {
		return mcpgo.NewToolResultError("a factset is materialized at the branch tip: the search reads the live index, so `commit` is not accepted"), nil
	}
	page, _, err := materialize.FactSetFirstPage(ctx, materializeScope(ctx, b, sWrite), breq)
	if err != nil {
		return factsetToolError(err), nil
	}
	return marshalFactSetPage(page)
}

// materializeFactSetResume serves the next page of a factset cursor, or seeks to
// `unit`. A factset cursor takes nothing but cursor, unit and binding: its page
// size was frozen by the first call, as on REST.
func materializeFactSetResume(ctx context.Context, req mcpgo.CallToolRequest, b *repos.Binding, mgr *repos.Manager, sWrite mcpStore, cursor string) (*mcpgo.CallToolResult, error) {
	for k := range req.GetArguments() {
		switch k {
		case "cursor", "unit", "binding":
		default:
			return mcpgo.NewToolResultError(fmt.Sprintf("%s cannot be passed with a factset cursor: the factset was fixed by the first call — pass only cursor (and unit to seek, binding)", k)), nil
		}
	}
	unit, present, err := intArgument(req, "unit")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	if present && unit < 1 {
		return mcpgo.NewToolResultError("unit must be at least 1"), nil
	}
	page, err := materialize.FactSetNextPage(ctx, materializeScope(ctx, b, sWrite), b, mgr, callerMayRead(ctx), cursor, unit)
	if err != nil {
		return factsetToolError(err), nil
	}
	return marshalFactSetPage(page)
}

// factsetToolError maps a factset error to its tool error, with the REST route's
// rule: the caller's own input errors and the factset's bounds are named, and
// anything else is a fixed text, the internal error only logged (it can
// carry SQL or file-path text).
func factsetToolError(err error) *mcpgo.CallToolResult {
	switch {
	case errors.Is(err, materialize.ErrCursorExpired):
		return mcpgo.NewToolResultError(errMaterializeExpired)
	case errors.Is(err, materialize.ErrStoreBusy):
		return mcpgo.NewToolResultError(errSessionBusy)
	case errors.Is(err, materialize.ErrCursorInUse):
		return mcpgo.NewToolResultError("another call is reading this cursor; a cursor has one reader at a time")
	case errors.Is(err, materialize.ErrFactSetBusy):
		return mcpgo.NewToolResultError(materialize.ErrFactSetBusy.Error())
	case errors.Is(err, materialize.ErrInvalid), errors.Is(err, materialize.ErrFactSetTooLarge):
		return mcpgo.NewToolResultError(err.Error())
	default:
		log.Error().Err(err).Msg("knomit_materialize factset failed")
		return mcpgo.NewToolResultError(errFactSetFailed)
	}
}

// errFactSetFailed is the tool error of every factset failure that is not the
// caller's to fix.
const errFactSetFailed = "the factset could not be served; retry, or start over without cursor"

func marshalFactSetPage(p materialize.FactSetPage) (*mcpgo.CallToolResult, error) {
	out, err := json.Marshal(p)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("marshal error: %v", err)), nil
	}
	return mcpgo.NewToolResultText(string(out)), nil
}

// factsetSearchArg decodes the `search` object.
func factsetSearchArg(raw any) (materialize.FactSetSearch, error) {
	obj, ok := raw.(map[string]any)
	if !ok {
		return materialize.FactSetSearch{}, errors.New("search must be an object")
	}
	s := materialize.FactSetSearch{Sort: materialize.SortRelevance}
	str := func(k string, dst *string) error {
		if v, ok := obj[k]; ok {
			sv, ok := v.(string)
			if !ok {
				return fmt.Errorf("search.%s must be a string", k)
			}
			*dst = sv
		}
		return nil
	}
	list := func(k string, dst *[]string) error {
		v, ok := obj[k]
		if !ok {
			return nil
		}
		items, ok := v.([]any)
		if !ok {
			return fmt.Errorf("search.%s must be an array of strings", k)
		}
		for _, it := range items {
			sv, ok := it.(string)
			if !ok {
				return fmt.Errorf("search.%s must be an array of strings", k)
			}
			if sv != "" {
				*dst = append(*dst, sv)
			}
		}
		return nil
	}
	num := func(k string, dst *float64) error {
		if v, ok := obj[k]; ok {
			f, ok := v.(float64)
			if !ok || math.IsNaN(f) {
				return fmt.Errorf("search.%s must be a number", k)
			}
			*dst = f
		}
		return nil
	}
	boolean := func(k string, dst *bool) error {
		if v, ok := obj[k]; ok {
			bv, ok := v.(bool)
			if !ok {
				return fmt.Errorf("search.%s must be true or false", k)
			}
			*dst = bv
		}
		return nil
	}
	for _, err := range []error{
		str("text", &s.Text), str("path", &s.Path), str("motif_match", &s.MotifMatch), str("sort", &s.Sort),
		list("entities", &s.Entities), list("domain", &s.Domains), list("type", &s.Types),
		list("exclude_type", &s.ExcludeTypes), list("kind", &s.Kinds), list("exclude_kind", &s.ExcludeKinds),
		list("origin", &s.Origins), list("ep", &s.Episodes), list("motifs", &s.Motifs),
		num("min_confidence", &s.MinConfidence), num("min_similarity", &s.MinSimilarity),
		boolean("domain_exact", &s.DomainExact),
	} {
		if err != nil {
			return materialize.FactSetSearch{}, err
		}
	}
	return s, nil
}
