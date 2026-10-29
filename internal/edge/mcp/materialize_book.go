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

// Book mode of knomit_materialize: `search` instead of `file` materializes
// that search as a book (materialize.BookFirstPage), and a book cursor pages
// it (materialize.BookNextPage). The pages are byte for byte the REST
// GET …/search/book pages for the same input: one shared builder.

// bookSearchKeys are the keys of the `search` object.
var bookSearchKeys = map[string]any{
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

// bookOnlyArgs may be passed only in book mode.
var bookOnlyArgs = []string{"search", "facts"}

// isBookFirst reports whether a first call is in book mode.
func isBookFirst(req mcpgo.CallToolRequest) bool {
	_, ok := req.GetArguments()["search"]
	return ok
}

// materializeBookFirst builds the book of the `search` argument and returns
// page 1.
func materializeBookFirst(ctx context.Context, req mcpgo.CallToolRequest, b *repos.Binding, mgr *repos.Manager, sWrite mcpStore) (*mcpgo.CallToolResult, error) {
	args := req.GetArguments()
	if _, ok := args["file"]; ok {
		return mcpgo.NewToolResultError("pass `file` (one fact) or `search` (a book), not both"), nil
	}
	if a, ok := args["audience"]; ok && a != audienceHuman {
		return mcpgo.NewToolResultError(`book mode serves the human pages: audience must be "human" or omitted`), nil
	}
	if err := rejectUnknownObjectKeys(req, "search", bookSearchKeys, "knomit_materialize"); err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	search, err := bookSearchArg(args["search"])
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
	breq := materialize.BookRequest{
		Binding: b, Manager: mgr, MayRead: callerMayRead(ctx), Search: search, Options: opts,
		Facts: materialize.DefaultBookFacts, Limit: materialize.HumanDefaultLimit,
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
		return mcpgo.NewToolResultError("a book is materialized at the branch tip: the search reads the live index, so `commit` is not accepted"), nil
	}
	page, _, err := materialize.BookFirstPage(ctx, materializeScope(ctx, b, sWrite), breq)
	if err != nil {
		return bookToolError(err), nil
	}
	return marshalBookPage(page)
}

// materializeBookResume serves the next page of a book cursor, or seeks to
// `unit`. A book cursor takes nothing but cursor, unit and binding: its page
// size was frozen by the first call, as on REST.
func materializeBookResume(ctx context.Context, req mcpgo.CallToolRequest, b *repos.Binding, mgr *repos.Manager, sWrite mcpStore, cursor string) (*mcpgo.CallToolResult, error) {
	for k := range req.GetArguments() {
		switch k {
		case "cursor", "unit", "binding":
		default:
			return mcpgo.NewToolResultError(fmt.Sprintf("%s cannot be passed with a book cursor: the book was fixed by the first call — pass only cursor (and unit to seek, binding)", k)), nil
		}
	}
	unit, present, err := intArgument(req, "unit")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	if present && unit < 1 {
		return mcpgo.NewToolResultError("unit must be at least 1"), nil
	}
	page, err := materialize.BookNextPage(ctx, materializeScope(ctx, b, sWrite), b, mgr, callerMayRead(ctx), cursor, unit)
	if err != nil {
		return bookToolError(err), nil
	}
	return marshalBookPage(page)
}

// bookToolError maps a book error to its tool error, with the REST route's
// rule: the caller's own input errors and the book's bounds are named, and
// anything else is a fixed text, the internal error only logged (it can
// carry SQL or file-path text).
func bookToolError(err error) *mcpgo.CallToolResult {
	switch {
	case errors.Is(err, materialize.ErrCursorExpired):
		return mcpgo.NewToolResultError(errMaterializeExpired)
	case errors.Is(err, materialize.ErrStoreBusy):
		return mcpgo.NewToolResultError(errSessionBusy)
	case errors.Is(err, materialize.ErrCursorInUse):
		return mcpgo.NewToolResultError("another call is reading this cursor; a cursor has one reader at a time")
	case errors.Is(err, materialize.ErrBookBusy):
		return mcpgo.NewToolResultError(materialize.ErrBookBusy.Error())
	case errors.Is(err, materialize.ErrInvalid), errors.Is(err, materialize.ErrBookTooLarge):
		return mcpgo.NewToolResultError(err.Error())
	default:
		log.Error().Err(err).Msg("knomit_materialize book failed")
		return mcpgo.NewToolResultError(errBookFailed)
	}
}

// errBookFailed is the tool error of every book failure that is not the
// caller's to fix.
const errBookFailed = "the book could not be served; retry, or start over without cursor"

func marshalBookPage(p materialize.BookPage) (*mcpgo.CallToolResult, error) {
	out, err := json.Marshal(p)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("marshal error: %v", err)), nil
	}
	return mcpgo.NewToolResultText(string(out)), nil
}

// bookSearchArg decodes the `search` object.
func bookSearchArg(raw any) (materialize.BookSearch, error) {
	obj, ok := raw.(map[string]any)
	if !ok {
		return materialize.BookSearch{}, errors.New("search must be an object")
	}
	s := materialize.BookSearch{Sort: materialize.SortRelevance}
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
			return materialize.BookSearch{}, err
		}
	}
	return s, nil
}
