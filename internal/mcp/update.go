package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"knomit/internal/fact"
	factpkg "knomit/internal/fact"
	"knomit/internal/federate"
	"knomit/internal/refs"
	"knomit/internal/repos"
	"knomit/internal/store"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// updateTool returns the Tool definition for knomit_update.
// updateToolName is knomit_update's registered name, shared by the tool
// definition and the handler's error messages.
const updateToolName = "knomit_update"

func updateTool() mcpgo.Tool {
	return mcpgo.NewTool(updateToolName,
		mcpgo.WithDescription("Update an existing fact in the knowledge base."),
		bindingArg(true),
		mcpgo.WithString("file",
			mcpgo.Required(),
			mcpgo.Description("Path to the fact file to update."),
		),
		mcpgo.WithString("moment_name",
			mcpgo.Required(),
			mcpgo.Description("A short label for this update moment."),
		),
		mcpgo.WithObject("updates",
			mcpgo.AdditionalProperties(false),
			mcpgo.Description("Fields to update. Include only the fields you want to change. origin and the topic/category path are immutable and not accepted here — fixing either requires knomit_retract plus a fresh knomit_learn. Send updates, ops, or both."),
			mcpgo.Properties(updateToolSchemaProperties()),
		),
		mcpgo.WithArray("ops",
			mcpgo.Description(opsDescription),
			mcpgo.Items(map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"op"},
				"properties": map[string]any{
					"op":      map[string]any{"type": "string", "enum": []string{fact.OpStrReplace, fact.OpAppend}, "description": "str_replace or append."},
					"old_str": map[string]any{"type": "string", "description": "str_replace: the exact text to replace. Must occur exactly once in the current body."},
					"new_str": map[string]any{"type": "string", "description": `str_replace: the replacement. Required; "" deletes old_str.`},
					"text":    map[string]any{"type": "string", "description": "append: the text to add at the end of the body."},
				},
			}),
		),
		mcpgo.WithString("if_commit",
			mcpgo.Description(ifCommitDescription),
		),
		traceArg(),
	)
}

// opsDescription is the agent-facing contract for knomit_update's ops.
const opsDescription = `Edit the body in place instead of resending it. Use ops, not updates.body, for any edit to a large fact. Send ops OR updates.body, never both; ops may be combined with every other updates field.

Each op is one of:
- {"op": "str_replace", "old_str": "...", "new_str": "..."} — old_str must occur EXACTLY ONCE in the body, byte for byte. new_str "" deletes it.
- {"op": "append", "text": "..."} — adds text at the end of the body as a new paragraph: knomit inserts only the newlines needed for one blank line before it and removes nothing you send. To extend the last line or list instead, str_replace it.

Ops apply in order, each to the body the previous op produced, so a later op may anchor on text an earlier one inserted. All ops land as ONE revision or none do: if any op fails, nothing is written and the error names the op by its zero-based index.

Matching is exact: no regex, no whitespace or Unicode normalisation. On 0 matches the error gives the longest prefix of old_str that does occur, its byte offset, and the first differing character on each side as U+XXXX — fix old_str from that (smart quotes, em dash vs "--", non-breaking space, trailing whitespace) and retry. On 2 or more matches it gives the count and offsets — widen old_str with surrounding text until it is unique.

The body's leading and trailing whitespace is not stored. An edit that leaves an unclosed ` + "```" + ` fence in a body whose fences were balanced is refused.`

// ifCommitDescription is the agent-facing contract for knomit_update's if_commit.
const ifCommitDescription = `Optional guard against editing a fact that changed since you read it. Pass the commit you read the fact at — the "commit" knomit_explain returned for it — as the full 40-character hash. The update proceeds only if the file's bytes at that commit equal its bytes now; an unrelated commit in between does not matter. Otherwise nothing is written and the error gives current_commit: read the fact again at current_commit and rebuild your edit against it. The check and the write are atomic, so two callers holding the same if_commit cannot both land. Applies to ops and to updates.body alike.`

// updateToolSchemaProperties is the knomit_update `updates` object's
// properties map. Extracted from the registration literal above for the same
// reason learnToolSchemaProperties is: conformance tests read the bytes
// actually served.
func updateToolSchemaProperties() map[string]any {
	return map[string]any{
		"title": map[string]any{"type": "string", "description": "New title."},
		"body":  map[string]any{"type": "string", "description": "New body text."},
		// Shared with knomit_learn via factschema.go, minus the
		// defaults: an update patches an existing fact, so declaring
		// a schema "default" would read as "omit this and it resets".
		"kind":       kindProperty(""),
		"type":       typeProperty(""),
		"confidence": map[string]any{"type": "number", "description": "Certainty level 0.0–1.0."},
		"sources":    map[string]any{"type": "integer", "description": "Number of independent sources."},
		"domain":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Replaces domain tags."},
		"entities":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Replaces entity list."},
		// Block A verbatim, shared with knomit_learn, so the two tools cannot
		// drift on what the field means. The wholesale-replacement rule is
		// stated once in the `updates` description above, where it covers every
		// list field at once — keeping it out of here is what lets this string
		// stay byte-identical to knomit_learn's.
		"motifs": motifsProperty(),
		// An explicit "" CLEARS the expiry; omitting the field leaves it.
		"expires": map[string]any{"type": "string", "description": expiresFieldDescription + ` On update, "" clears it; omit the field to leave it unchanged.`},
		"refs":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Replaces the ENTIRE refs list. Send every ref the fact should keep — any existing ref you leave out is dropped. To add or refresh a ref, read the current refs first and resend the full merged list. Omit the field to leave refs unchanged."},
	}
}

// updateInput represents the updates object in the request.
type updateInput struct {
	Kind       *string  `json:"kind"`
	Type       *string  `json:"type"`
	Confidence *float64 `json:"confidence"`
	Sources    *int     `json:"sources"`
	Body       *string  `json:"body"`
	Title      *string  `json:"title"`
	Refs       []string `json:"refs"`
	Domain     []string `json:"domain"`
	Entities   []string `json:"entities"`
	Motifs     []string `json:"motifs"`
	// Expires is a pointer so "" (clear) differs from absent (unchanged).
	Expires *string `json:"expires"`
}

// UpdateHandler returns the handler function for knomit_update.
func UpdateHandler() func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		b, err := repos.RequireBinding(ctx)
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		if !b.WriteOK() {
			return mcpgo.NewToolResultError(readOnlyViewMessage(b)), nil
		}
		ri := b.Write()
		s, release, err := storeIndices(ri)
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		defer release()
		// WHERE this write lands, and therefore also what it reads while
		// deciding: dedup, existence and ref resolution must all see the
		// branch the fact will be committed to. Inside an experiment that is
		// the experiment — an isolated world, not a diff against the agent
		// branch — so every one of them asks the binding, not the repo.
		writeBranch := b.WriteBranch()
		ontologyRoot := ri.OntologyRoot()
		ontology := ri.Ontology()

		// 1. Get arguments.
		file := req.GetString("file", "")
		if file == "" {
			return mcpgo.NewToolResultError("file is required"), nil
		}
		file, err = federate.WriteRepoPath(b, file)
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		file = factpkg.NormalizePath(ontologyRoot, file)
		// knomit_learn refuses to ALLOCATE a private path; this refuses to
		// write one that already exists. Same rule, both halves: a fact under
		// a dot-prefixed segment is skipped by the indexer, Verify and the OKF
		// exporter alike, so an update there would commit a revision no reader
		// ever sees and report success for it.
		//
		// The exception is knomit's OWN namespace: a path under
		// .knomit/<area>/ is job state, which WANTS to be invisible to
		// readers. Invisibility is the feature there, not the bug.
		if factpkg.IsPrivatePath(file) && !factpkg.IsWritablePrivatePath(file) {
			return mcpgo.NewToolResultError(fmt.Sprintf(
				"%s is private: a path segment beginning with '.' cannot hold a fact, "+
					"except under %s/<area>/", file, factpkg.PrivateRoot)), nil
		}
		momentName := req.GetString("moment_name", "")
		if momentName == "" {
			return mcpgo.NewToolResultError("moment_name is required"), nil
		}
		if err := checkMomentName(momentName); err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		// #349: the agent's trace entries, refused before anything is written.
		if ctx, err = applyTrace(ctx, req); err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}

		// 2b. Parse updates and ops — with the other argument checks, before
		// the file is looked up, so a bad argument is reported as that even
		// when the path is also wrong. updates.body and ops both rewrite the body,
		// so a call may carry one or the other, never both.
		var updates updateInput
		_, hasUpdates := req.GetArguments()["updates"]
		_, hasOps := req.GetArguments()["ops"]
		if !hasUpdates && !hasOps {
			return mcpgo.NewToolResultError("updates or ops is required"), nil
		}
		if hasUpdates {
			// A key the schema does not declare is refused, not ignored:
			// ignoring it reported success for a change that was never made
			// (origin, topic, path, a body-edit name that belongs in ops).
			// The accepted set is the served schema; the strict decode below
			// backs it, and a test keeps the struct and the schema equal.
			if err := rejectUnknownObjectKeys(req, "updates", updateToolSchemaProperties(), updateToolName); err != nil {
				return mcpgo.NewToolResultError(err.Error()), nil
			}
			if err := unmarshalArgStrict(req, "updates", &updates); err != nil {
				return mcpgo.NewToolResultError(err.Error()), nil
			}
			// Checked with the arguments, so a later ontology or refs error
			// cannot mask a bad title and cost a second round trip.
			// SerializeFact enforces the same rule; this reports it first.
			if updates.Title != nil {
				if err := factpkg.ValidateTitle(*updates.Title); err != nil {
					return mcpgo.NewToolResultError(fmt.Sprintf("updates.title: %v", err)), nil
				}
			}
		}
		// Strict: an unknown key on an op (say replace_all, or an occurrence
		// index) is refused rather than ignored, because ignoring it would
		// apply an edit the caller did not ask for.
		var ops []factpkg.BodyOp
		if hasOps {
			if err := unmarshalArgStrict(req, "ops", &ops); err != nil {
				return mcpgo.NewToolResultError(err.Error()), nil
			}
			if updates.Body != nil {
				return mcpgo.NewToolResultError("send updates.body or ops, not both: both rewrite the body"), nil
			}
		}

		// 3. Check file exists.
		exists, err := s.facts.FactExists(ctx, writeBranch, file)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("file exists check error: %v", err)), nil
		}
		if !exists {
			return mcpgo.NewToolResultError(fmt.Sprintf("file not found: %s", file)), nil
		}

		// 4. Read and parse existing fact.
		// WithHash: the blob read here is the precondition the write below
		// commits against, so nothing that lands in between is overwritten.
		readResult, err := s.facts.ReadFact(ctx, writeBranch, file, &store.ReadFactOpts{WithHash: true})
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("read file error: %v", err)), nil
		}
		content := readResult.Content
		fact, err := fact.ParseFact(file, content)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("parse fact error: %v", err)), nil
		}
		// The refs this fact ALREADY carried. They resolved at the commit that
		// wrote them and are never re-judged here — re-checking them against
		// today's corpus would mean a retraction anywhere in history makes
		// every fact that ever cited it uneditable. Captured before the merge
		// below, which may replace the list wholesale.
		priorRefs := append([]string(nil), fact.Refs...)

		// Optimistic concurrency. The guard compares the file's BYTES at
		// if_commit with its bytes now, not commit hashes: what the caller
		// anchored its edit on is the content it read, so the content is what
		// is checked. Any commit at which the file read the same passes — the
		// one knomit_explain returned, or any later one that left it alone.
		// The write below re-checks the blob inside the write lock, so the
		// guard also holds against a writer landing after this check.
		ifCommit, err := ifCommitArg(req)
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		if ifCommit != "" {
			at, rerr := s.facts.ReadFact(ctx, writeBranch, file, &store.ReadFactOpts{AtCommit: ifCommit})
			if rerr != nil || at.Content != content {
				reason := "the file changed since that commit"
				if rerr != nil {
					reason = "the file cannot be read at that commit"
				}
				return staleFactResult(ctx, s, writeBranch, fmt.Sprintf("if_commit %s does not match: %s", ifCommit, reason)), nil
			}
		}

		// 6. Merge updates into fact. (kind, type) validation is deferred
		// to SerializeFact below — it's the single source of truth for
		// kind/type consistency.
		if updates.Kind != nil {
			fact.Kind = factpkg.Kind(*updates.Kind)
		}
		if updates.Type != nil {
			fact.Type = factpkg.Type(*updates.Type)
		}
		if updates.Confidence != nil {
			fact.Confidence = *updates.Confidence
		}
		if updates.Sources != nil {
			fact.Sources = *updates.Sources
		}
		priorBody := fact.Body
		bodyChanged := updates.Body != nil || hasOps
		if updates.Body != nil {
			fact.Body = *updates.Body
		}
		var opDeltas []factpkg.BodyOpDelta
		if hasOps {
			if fact.Body, opDeltas, err = factpkg.ApplyBodyOps(fact.Body, ops); err != nil {
				return mcpgo.NewToolResultError(err.Error()), nil
			}
		}
		if updates.Title != nil {
			fact.Title = *updates.Title
		}
		if updates.Domain != nil {
			fact.Domain = updates.Domain
		}
		if updates.Entities != nil {
			fact.Entities = updates.Entities
		}
		// Wholesale replacement, like Domain and Entities. nil means
		// "unchanged"; an explicit [] clears. No validation here — a malformed
		// motif is rejected and a subject motif silently dropped by
		// SerializeFact, the single gate (MN4).
		if updates.Expires != nil {
			fact.Expires = *updates.Expires
		}
		if updates.Motifs != nil {
			fact.Motifs = updates.Motifs
		}
		// Refs replace wholesale, like Domain and Entities — the caller
		// sends the complete new list. Dropping a ref only affects this
		// and future revisions: prior revisions keep their refs in git
		// history and their DERIVED_FROM edges in the graph. Deduped so
		// a careless caller can't accumulate duplicates in one call.
		if updates.Refs != nil {
			var refs []string
			for _, ref := range updates.Refs {
				refs = factpkg.AppendUnique(refs, ref)
			}
			fact.Refs = refs
		}

		// The file format does not keep edge whitespace on the title or the
		// body (ParseFact trims both), so trim it here rather than fail the
		// roundtrip below on bytes no reader could ever see.
		fact.Title = strings.TrimSpace(fact.Title)
		fact.Body = strings.TrimSpace(fact.Body)
		if bodyChanged {
			if err := factpkg.CheckFenceEdit(priorBody, fact.Body); err != nil {
				return mcpgo.NewToolResultError(err.Error()), nil
			}
		}

		// 7. Validate the assembled fact against the ontology's rules.
		// Derive topic/category by stripping the ontologyRoot prefix and
		// the final /<uuid>.md segment from the normalized fact path.
		//
		// Private state is SKIPPED wholesale, exactly as knomit_learn skips it
		// (it guards on an empty topic path). A .knomit/<area>/ path has no
		// ontology placement: the TrimPrefix is a no-op, so the derived topic
		// would be ".knomit/<area>", and while an unknown topic makes the
		// per-topic walk a no-op, ValidateFact runs the ontology's ROOT rules
		// UNCONDITIONALLY first. Without this guard, any ontology declaring a
		// top-level `validations:` would let a job allocate its slot with learn
		// and then refuse every update to it — its whole write path after run
		// one.
		if ontology != nil && !factpkg.IsWritablePrivatePath(file) {
			topicCategory := strings.TrimPrefix(file, ontologyRoot+"/")
			topicCategory = path.Dir(topicCategory)
			if err := factpkg.ValidateFact(ontology, topicCategory, fact); err != nil {
				return mcpgo.NewToolResultError(err.Error()), nil
			}
		}

		// Refs this update ADDS must resolve; refs it carries forward are not
		// re-judged. The same refs.Gate serves every write path — this tool,
		// knomit_learn, the pipelines and the REST handlers — because refs
		// replace wholesale here, so a learn-only gate would be bypassed by
		// writing a fact clean and then updating its refs to garbage.
		//
		// The batch is this one fact, so its own path satisfies a self-reference.
		gate := refs.New(factpkg.ID12(ri.ID()), refs.FromFactQuery(s.factQuery, writeBranch))
		canon, _, err := gate.Apply(ctx, file, fact.Refs, priorRefs)
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		fact.Refs = canon

		// 8. Write updated fact.
		serialized, err := factpkg.SerializeFact(fact)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("serialize error: %v", err)), nil
		}
		if err := checkRoundtrip(file, fact, serialized); err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		commitMsg := fmt.Sprintf("update: %s", fact.Title)
		writeRes, err := s.facts.WriteFactIfUnchanged(ctx, writeBranch, file, serialized, commitMsg, "update", readResult.BlobHash)
		if errors.Is(err, store.ErrFactChanged) {
			reason := "the fact changed while this update was being applied"
			if ifCommit != "" {
				reason = fmt.Sprintf("if_commit %s does not match: %s", ifCommit, reason)
			}
			return staleFactResult(ctx, s, writeBranch, reason), nil
		}
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("write error: %v", err)), nil
		}

		dest := describeWriteDestination(b)
		result := map[string]interface{}{
			"file":       file,
			"commit":     writeRes.CommitHash,
			"written_to": dest,
			"summary":    dest.summary("1 fact revision"),
		}
		if len(opDeltas) > 0 {
			result["ops"] = opDeltas
		}
		out, err := json.Marshal(result)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("marshal error: %v", err)), nil
		}
		return mcpgo.NewToolResultText(string(out)), nil
	}
}

// ifCommitArg reads if_commit. Absent, null and "" all mean no guard — some
// MCP clients send null or "" for an unset optional. Anything else must be a
// full lowercase 40-hex commit hash: a number or an array must not read as ""
// and silently switch the guard off.
func ifCommitArg(req mcpgo.CallToolRequest) (string, error) {
	raw, ok := req.GetArguments()["if_commit"]
	if !ok || raw == nil || raw == "" {
		return "", nil
	}
	c, isString := raw.(string)
	if !isString || !commitHashRE.MatchString(c) {
		return "", fmt.Errorf("if_commit must be a full 40-character lowercase hex commit hash, got %v", raw)
	}
	return c, nil
}

var commitHashRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

// staleFactResult is the refusal for an update whose fact is no longer what
// the caller read. It carries the write branch's tip as current_commit and
// nothing else — never the body. The tip is the right commit to hand back:
// the guard compares bytes, and the file's bytes at the tip are by definition
// its bytes now, so a retry that re-reads there and sends if_commit=tip passes
// unless yet another write lands first.
func staleFactResult(ctx context.Context, s mcpStore, branch, reason string) *mcpgo.CallToolResult {
	tip, err := s.branches.HeadCommit(ctx, branch)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("%s; nothing was written (current_commit unavailable: %v)", reason, err))
	}
	return mcpgo.NewToolResultError(fmt.Sprintf(
		"%s; nothing was written. current_commit: %s — read the fact again at current_commit and rebuild the edit against it",
		reason, tip))
}

// checkRoundtrip refuses bytes that would not read back as what was written:
// the file is parsed and serialised again, and the two serialisations must be
// identical. It catches any field content the parser treats as structure — a
// CR LF pair it normalises away, a newline in the title — which would
// otherwise commit a revision no reader sees as sent. The error names the
// field that differed and shows where.
func checkRoundtrip(file string, sent factpkg.Fact, serialized string) error {
	parsed, err := factpkg.ParseFact(file, serialized)
	if err != nil {
		return fmt.Errorf("roundtrip check failed: the written fact would not parse: %v", err)
	}
	again, err := factpkg.SerializeFact(parsed)
	if err == nil && again == serialized {
		return nil
	}
	switch {
	case parsed.Title != sent.Title:
		return fmt.Errorf("roundtrip check failed: the title would read back differently — %s", firstDifference(sent.Title, parsed.Title))
	case parsed.Body != sent.Body:
		return fmt.Errorf("roundtrip check failed: the body would read back differently — %s", firstDifference(sent.Body, parsed.Body))
	default:
		return fmt.Errorf("roundtrip check failed: the frontmatter would read back differently")
	}
}

// firstDifference describes where sent and read first differ, as a short
// quoted window of each from that byte offset.
func firstDifference(sent, read string) string {
	i := 0
	for i < len(sent) && i < len(read) && sent[i] == read[i] {
		i++
	}
	// Back up to a rune boundary so neither window starts mid-character.
	for i > 0 && i < len(sent) && !utf8.RuneStart(sent[i]) {
		i--
	}
	window := func(s string) string {
		s = s[min(i, len(s)):]
		if len(s) > 30 {
			cut := 30
			for cut > 0 && !utf8.RuneStart(s[cut]) {
				cut--
			}
			s = s[:cut] + "…"
		}
		return strconv.Quote(s)
	}
	return fmt.Sprintf("at byte %d, sent %s, would read %s", i, window(sent), window(read))
}
